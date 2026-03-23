package nutanix

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/rs/zerolog/log"
)

// Client is an HTTP client for Nutanix Prism Central API.
type Client struct {
	baseURL    string // v3 API base: https://host:9440/api/nutanix/v3
	hostURL    string // host base: https://host:9440 (for v4 API paths)
	username   string
	password   string
	httpClient *http.Client
}

// ClientConfig holds Nutanix connection parameters.
type ClientConfig struct {
	PrismCentral string
	Username     string
	Password     string
	Insecure     bool
}

// NewClient creates a new Nutanix API client.
func NewClient(cfg ClientConfig) *Client {
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.Insecure},
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second, // TCP keepalive every 30s prevents idle kills
		}).DialContext,
		IdleConnTimeout:       0,                  // no idle timeout for long uploads
		ResponseHeaderTimeout: 0,                  // wait indefinitely for response
		DisableKeepAlives:     false,
		DisableCompression:    true,               // raw disk data — compression wastes CPU
		MaxIdleConnsPerHost:   2,
		WriteBufferSize:       4 * 1024 * 1024,    // 4 MB TCP write buffer
		ReadBufferSize:        64 * 1024,           // 64 KB read buffer
		ForceAttemptHTTP2:     false,               // HTTP/1.1 for streaming PUT
	}
	hostBase := fmt.Sprintf("https://%s:9440", cfg.PrismCentral)
	return &Client{
		baseURL:  hostBase + "/api/nutanix/v3",
		hostURL:  hostBase,
		username: cfg.Username,
		password: cfg.Password,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   0, // no timeout for large uploads
		},
	}
}

// TaskStatus represents a Nutanix task status.
type TaskStatus struct {
	UUID            string `json:"uuid"`
	Status          string `json:"status"`
	PercentComplete int    `json:"percentage_complete"`
	ErrorDetail     string `json:"error_detail,omitempty"`
}

// doRequest executes an HTTP request against the v3 API with auth.
func (c *Client) doRequest(ctx context.Context, method, path string, body interface{}) ([]byte, int, error) {
	url := c.baseURL + path
	return c.doRequestFull(ctx, method, url, body, nil)
}

// doRequestFull executes an HTTP request with auth and optional extra headers.
func (c *Client) doRequestFull(ctx context.Context, method, url string, body interface{}, extraHeaders map[string]string) ([]byte, int, error) {
	var reqBody io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, 0, fmt.Errorf("marshaling request: %w", err)
		}
		reqBody = bytes.NewReader(data)
	}

	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("creating request: %w", err)
	}

	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range extraHeaders {
		req.Header.Set(k, v)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("executing request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("reading response: %w", err)
	}

	return respBody, resp.StatusCode, nil
}

// after returns a channel that receives after n seconds.
func after(seconds int) <-chan time.Time {
	return time.After(time.Duration(seconds) * time.Second)
}

// doUpload sends a binary PUT request for image upload.
func (c *Client) doUpload(ctx context.Context, path string, reader io.Reader, size int64) error {
	url := c.baseURL + path

	log.Info().
		Str("url", path).
		Int64("size_mb", size/(1024*1024)).
		Msg("starting HTTP PUT upload")

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, reader)
	if err != nil {
		return fmt.Errorf("creating upload request: %w", err)
	}

	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = size

	resp, err := c.httpClient.Do(req)
	if err != nil {
		log.Error().Err(err).
			Str("url", path).
			Int64("content_length", size).
			Msg("HTTP upload request failed")
		return fmt.Errorf("uploading: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		log.Error().
			Int("status", resp.StatusCode).
			Str("body", string(body)).
			Msg("upload returned error status")
		return fmt.Errorf("upload failed with status %d: %s", resp.StatusCode, string(body))
	}

	log.Info().
		Int("status", resp.StatusCode).
		Str("url", path).
		Msg("HTTP upload completed successfully")

	return nil
}

// doUploadGzip sends a gzip-compressed binary PUT request for image upload.
// Uses chunked transfer encoding (Content-Length = -1) since the compressed
// size is not known in advance. Sets Content-Encoding: gzip so the server
// decompresses the stream.
func (c *Client) doUploadGzip(ctx context.Context, path string, reader io.Reader) error {
	url := c.baseURL + path

	log.Info().
		Str("url", path).
		Msg("starting HTTP PUT upload (gzip, chunked)")

	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, reader)
	if err != nil {
		return fmt.Errorf("creating upload request: %w", err)
	}

	req.SetBasicAuth(c.username, c.password)
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("Content-Encoding", "gzip")
	req.ContentLength = -1 // unknown compressed size → chunked transfer

	resp, err := c.httpClient.Do(req)
	if err != nil {
		log.Error().Err(err).
			Str("url", path).
			Msg("HTTP gzip upload request failed")
		return fmt.Errorf("uploading (gzip): %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		log.Error().
			Int("status", resp.StatusCode).
			Str("body", string(body)).
			Msg("gzip upload returned error status")
		return fmt.Errorf("upload failed with status %d: %s", resp.StatusCode, string(body))
	}

	log.Info().
		Int("status", resp.StatusCode).
		Str("url", path).
		Msg("HTTP gzip upload completed successfully")

	return nil
}

// WaitForTask polls a task until it completes or fails.
func (c *Client) WaitForTask(ctx context.Context, taskUUID string) error {
	log.Info().Str("task", taskUUID).Msg("waiting for Nutanix task")

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}

		body, status, err := c.doRequest(ctx, http.MethodGet, "/tasks/"+taskUUID, nil)
		if err != nil {
			return fmt.Errorf("polling task: %w", err)
		}
		if status >= 300 {
			return fmt.Errorf("task poll returned status %d", status)
		}

		var task TaskStatus
		if err := json.Unmarshal(body, &task); err != nil {
			return fmt.Errorf("parsing task status: %w", err)
		}

		log.Debug().
			Str("task", taskUUID).
			Str("status", task.Status).
			Int("percent", task.PercentComplete).
			Msg("task progress")

		switch task.Status {
		case "SUCCEEDED":
			log.Info().Str("task", taskUUID).Msg("task completed")
			return nil
		case "FAILED":
			return fmt.Errorf("task failed: %s", task.ErrorDetail)
		}
	}
}

// TestConnection verifies connectivity to Prism Central.
func (c *Client) TestConnection(ctx context.Context) error {
	_, status, err := c.doRequest(ctx, http.MethodPost, "/clusters/list", map[string]interface{}{
		"kind":   "cluster",
		"length": 1,
	})
	if err != nil {
		return fmt.Errorf("testing Nutanix connection: %w", err)
	}
	if status == 401 {
		return fmt.Errorf("authentication failed")
	}
	if status >= 300 {
		return fmt.Errorf("unexpected status %d", status)
	}
	log.Info().Msg("Nutanix connection verified")
	return nil
}

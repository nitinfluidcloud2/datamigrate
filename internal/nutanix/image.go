package nutanix

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"

	"github.com/rs/zerolog/log"
)

// ImageCreateSpec is the request body for creating an image.
type ImageCreateSpec struct {
	Spec struct {
		Name      string `json:"name"`
		Resources struct {
			ImageType string `json:"image_type"`
		} `json:"resources"`
		Description string `json:"description"`
	} `json:"spec"`
	Metadata struct {
		Kind string `json:"kind"`
	} `json:"metadata"`
}

// ImageResponse is the response from image creation.
type ImageResponse struct {
	Metadata struct {
		UUID string `json:"uuid"`
	} `json:"metadata"`
	Status struct {
		ExecutionContext struct {
			TaskUUID string `json:"task_uuid"`
		} `json:"execution_context"`
	} `json:"status"`
}

// CreateImage creates a new image entry on Nutanix.
func (c *Client) CreateImage(ctx context.Context, name string, sizeBytes int64) (string, error) {
	log.Info().Str("name", name).Int64("size", sizeBytes).Msg("creating Nutanix image")

	spec := ImageCreateSpec{}
	spec.Spec.Name = name
	spec.Spec.Resources.ImageType = "DISK_IMAGE"
	spec.Spec.Description = fmt.Sprintf("Migrated disk image (%d bytes)", sizeBytes)
	spec.Metadata.Kind = "image"

	body, status, err := c.doRequest(ctx, http.MethodPost, "/images", spec)
	if err != nil {
		return "", fmt.Errorf("creating image: %w", err)
	}
	if status >= 300 {
		return "", fmt.Errorf("create image failed with status %d: %s", status, string(body))
	}

	var resp ImageResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return "", fmt.Errorf("parsing image response: %w", err)
	}

	uuid := resp.Metadata.UUID
	log.Info().Str("uuid", uuid).Msg("image created")

	// Wait for the creation task
	if taskUUID := resp.Status.ExecutionContext.TaskUUID; taskUUID != "" {
		if err := c.WaitForTask(ctx, taskUUID); err != nil {
			return "", fmt.Errorf("waiting for image creation: %w", err)
		}
	}

	return uuid, nil
}

// UploadImage uploads a qcow2 file to an existing image.
func (c *Client) UploadImage(ctx context.Context, imageUUID string, filePath string) error {
	log.Info().Str("uuid", imageUUID).Str("file", filePath).Msg("uploading image")

	f, err := os.Open(filePath)
	if err != nil {
		return fmt.Errorf("opening image file: %w", err)
	}
	defer f.Close()

	stat, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stating image file: %w", err)
	}

	path := fmt.Sprintf("/images/%s/file", imageUUID)
	return c.doUpload(ctx, path, io.Reader(f), stat.Size())
}

// UploadImageStream uploads raw disk data from a reader to an image.
// This allows streaming data directly without writing to local disk.
func (c *Client) UploadImageStream(ctx context.Context, imageUUID string, reader io.Reader, size int64) error {
	log.Info().Str("uuid", imageUUID).Int64("size", size).Msg("streaming image upload (raw)")

	path := fmt.Sprintf("/images/%s/file", imageUUID)
	return c.doUpload(ctx, path, reader, size)
}

// UploadImageStreamGzip uploads gzip-compressed disk data from a reader.
// Uses chunked transfer encoding since the compressed size is unknown.
// Nutanix decompresses the stream server-side via Content-Encoding: gzip.
func (c *Client) UploadImageStreamGzip(ctx context.Context, imageUUID string, reader io.Reader) error {
	log.Info().Str("uuid", imageUUID).Msg("streaming image upload (gzip)")

	path := fmt.Sprintf("/images/%s/file", imageUUID)
	return c.doUploadGzip(ctx, path, reader)
}

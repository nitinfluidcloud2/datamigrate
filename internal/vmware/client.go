package vmware

import (
	"context"
	"fmt"
	"net/url"

	"github.com/rs/zerolog/log"
	"github.com/vmware/govmomi"
	"github.com/vmware/govmomi/vim25"
	"github.com/vmware/govmomi/vim25/soap"
)

// Client wraps govmomi for VMware vSphere operations.
type Client struct {
	govClient  *govmomi.Client
	vimClient  *vim25.Client
	vcenterURL string
	cfg        ClientConfig
}

// ClientConfig holds connection parameters.
type ClientConfig struct {
	VCenter  string
	Username string
	Password string
	Insecure bool
}

// NewClient creates a new VMware client connected to vCenter.
func NewClient(ctx context.Context, cfg ClientConfig) (*Client, error) {
	u, err := soap.ParseURL(cfg.VCenter)
	if err != nil {
		return nil, fmt.Errorf("parsing vcenter URL: %w", err)
	}
	u.User = url.UserPassword(cfg.Username, cfg.Password)

	gc, err := govmomi.NewClient(ctx, u, cfg.Insecure)
	if err != nil {
		return nil, fmt.Errorf("connecting to vcenter: %w", err)
	}

	log.Info().Str("vcenter", cfg.VCenter).Msg("connected to vCenter")

	return &Client{
		govClient:  gc,
		vimClient:  gc.Client,
		vcenterURL: cfg.VCenter,
		cfg:        cfg,
	}, nil
}

// VimClient returns the underlying vim25 client.
func (c *Client) VimClient() *vim25.Client {
	return c.vimClient
}

// Relogin re-authenticates the existing session with vCenter.
// Use this when the session has expired after long-running operations.
func (c *Client) Relogin(ctx context.Context) error {
	// Create a fresh connection — session re-login via govClient.Login()
	// consistently fails on managed vSphere (OVH) after session expiry.
	// Fresh connection is more reliable.
	userInfo := url.UserPassword(c.cfg.Username, c.cfg.Password)

	u, parseErr := soap.ParseURL(c.cfg.VCenter)
	if parseErr != nil {
		return fmt.Errorf("parsing vcenter URL: %w", parseErr)
	}
	u.User = userInfo

	gc, err := govmomi.NewClient(ctx, u, c.cfg.Insecure)
	if err != nil {
		return fmt.Errorf("re-connecting to vCenter: %w", err)
	}

	c.govClient = gc
	c.vimClient = gc.Client
	log.Info().Str("vcenter", c.cfg.VCenter).Msg("re-connected to vCenter")
	return nil
}

// Logout disconnects from vCenter.
func (c *Client) Logout(ctx context.Context) error {
	return c.govClient.Logout(ctx)
}

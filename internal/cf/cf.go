package cf

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"

	cloudflare "github.com/cloudflare/cloudflare-go"
)

// Client wraps the Cloudflare API for DNS record and tunnel management.
// When created with an empty token, all methods become no-ops that return nil errors.
type Client struct {
	api       *cloudflare.API
	zoneID    string
	accountID string
	tunnelID  string
	noop      bool
	tunnelMu  sync.Mutex // serialises get-modify-put on tunnel config
}

// New creates a Cloudflare client. All four values are optional — the client
// becomes a no-op for DNS when token/zoneID are absent, and tunnel management
// is disabled when tunnelID/accountID are absent.
func New(token, zoneID, tunnelID, accountID string) (*Client, error) {
	if token == "" || zoneID == "" {
		return &Client{noop: true}, nil
	}
	api, err := cloudflare.NewWithAPIToken(token)
	if err != nil {
		return nil, fmt.Errorf("create cloudflare client: %w", err)
	}
	return &Client{
		api:       api,
		zoneID:    zoneID,
		tunnelID:  tunnelID,
		accountID: accountID,
	}, nil
}

// ---- DNS records ------------------------------------------------------------

// CreateRecord creates an A record and returns its ID.
func (c *Client) CreateRecord(ctx context.Context, name, ip string) (string, error) {
	if c.noop {
		return "", nil
	}
	proxied := false
	rec, err := c.api.CreateDNSRecord(ctx, cloudflare.ZoneIdentifier(c.zoneID), cloudflare.CreateDNSRecordParams{
		Type:    "A",
		Name:    name,
		Content: ip,
		TTL:     60,
		Proxied: &proxied,
	})
	if err != nil {
		return "", fmt.Errorf("create DNS record %s: %w", name, err)
	}
	return rec.ID, nil
}

// UpdateRecord updates an existing A record's content.
func (c *Client) UpdateRecord(ctx context.Context, recordID, ip string) error {
	if c.noop {
		return nil
	}
	_, err := c.api.UpdateDNSRecord(ctx, cloudflare.ZoneIdentifier(c.zoneID), cloudflare.UpdateDNSRecordParams{
		ID:      recordID,
		Content: ip,
	})
	if err != nil {
		return fmt.Errorf("update DNS record %s: %w", recordID, err)
	}
	return nil
}

// DeleteRecord deletes a DNS record by ID.
func (c *Client) DeleteRecord(ctx context.Context, recordID string) error {
	if c.noop {
		return nil
	}
	return c.api.DeleteDNSRecord(ctx, cloudflare.ZoneIdentifier(c.zoneID), recordID)
}

// FindRecord looks up a record ID and current IP by exact name.
func (c *Client) FindRecord(ctx context.Context, name string) (string, string, error) {
	if c.noop {
		return "", "", nil
	}
	records, _, err := c.api.ListDNSRecords(ctx, cloudflare.ZoneIdentifier(c.zoneID), cloudflare.ListDNSRecordsParams{
		Name: name,
	})
	if err != nil {
		return "", "", err
	}
	if len(records) == 0 {
		return "", "", nil
	}
	return records[0].ID, records[0].Content, nil
}

// ---- Tunnel management ------------------------------------------------------

// CreateTunnel creates a new named Cloudflare Tunnel and returns its ID and token.
// The caller is responsible for persisting the token — it authenticates cloudflared.
func (c *Client) CreateTunnel(ctx context.Context, name string) (tunnelID, token string, err error) {
	if c.noop || c.accountID == "" {
		return "", "", fmt.Errorf("cloudflare account not configured")
	}
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		return "", "", fmt.Errorf("generate tunnel secret: %w", err)
	}
	tunnel, err := c.api.CreateTunnel(ctx, cloudflare.AccountIdentifier(c.accountID), cloudflare.TunnelCreateParams{
		Name:      name,
		Secret:    base64.StdEncoding.EncodeToString(secret),
		ConfigSrc: "cloudflare",
	})
	if err != nil {
		return "", "", fmt.Errorf("create tunnel: %w", err)
	}

	tokenStr, err := c.api.GetTunnelToken(ctx, cloudflare.AccountIdentifier(c.accountID), tunnel.ID)
	if err != nil {
		return "", "", fmt.Errorf("get tunnel token: %w", err)
	}

	c.SetTunnelID(tunnel.ID)
	return tunnel.ID, tokenStr, nil
}

// DeleteTunnel deletes a Cloudflare Tunnel by ID.
func (c *Client) DeleteTunnel(ctx context.Context, tunnelID string) error {
	if c.noop || c.accountID == "" {
		return nil
	}
	return c.api.DeleteTunnel(ctx, cloudflare.AccountIdentifier(c.accountID), tunnelID)
}

// SetTunnelID updates the active tunnel ID used for ingress management.
func (c *Client) SetTunnelID(id string) {
	c.tunnelMu.Lock()
	c.tunnelID = id
	c.tunnelMu.Unlock()
}

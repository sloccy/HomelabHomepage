package cf

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sync"
)

// Client wraps the Cloudflare API for DNS record and tunnel management.
// When created with an empty token, all methods become no-ops that return nil errors.
type Client struct {
	api       *apiClient
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
	return &Client{
		api:       newAPIClient(token),
		zoneID:    zoneID,
		tunnelID:  tunnelID,
		accountID: accountID,
	}, nil
}

// ---- DNS records ------------------------------------------------------------

func (c *Client) CreateRecord(ctx context.Context, name, ip string) (string, error) {
	if c.noop {
		return "", nil
	}
	id, err := c.createDNSRecord(ctx, map[string]any{
		"type":    "A",
		"name":    name,
		"content": ip,
		"ttl":     60,
		"proxied": false,
	})
	if err != nil {
		return "", fmt.Errorf("create DNS record %s: %w", name, err)
	}
	return id, nil
}

func (c *Client) UpdateRecord(ctx context.Context, recordID, ip string) error {
	if c.noop {
		return nil
	}
	_, err := c.api.do(ctx, http.MethodPatch,
		"zones/"+c.zoneID+"/dns_records/"+recordID,
		map[string]any{"content": ip},
	)
	if err != nil {
		return fmt.Errorf("update DNS record %s: %w", recordID, err)
	}
	return nil
}

func (c *Client) DeleteRecord(ctx context.Context, recordID string) error {
	if c.noop {
		return nil
	}
	_, err := c.api.do(ctx, http.MethodDelete,
		"zones/"+c.zoneID+"/dns_records/"+recordID,
		nil,
	)
	if err != nil {
		return fmt.Errorf("delete DNS record %s: %w", recordID, err)
	}
	return nil
}

func (c *Client) FindRecord(ctx context.Context, name string) (recordID, ip string, err error) {
	if c.noop {
		return "", "", nil
	}
	path := "zones/" + c.zoneID + "/dns_records?name=" + url.QueryEscape(name)
	result, err := c.api.do(ctx, http.MethodGet, path, nil)
	if err != nil {
		return "", "", fmt.Errorf("find DNS record %s: %w", name, err)
	}
	var recs []dnsRecord
	if err := json.Unmarshal(result, &recs); err != nil {
		return "", "", fmt.Errorf("decode DNS records: %w", err)
	}
	if len(recs) == 0 {
		return "", "", nil
	}
	return recs[0].ID, recs[0].Content, nil
}

// createDNSRecord posts params to the DNS records endpoint and returns the new record ID.
func (c *Client) createDNSRecord(ctx context.Context, params map[string]any) (string, error) {
	result, err := c.api.do(ctx, http.MethodPost, "zones/"+c.zoneID+"/dns_records", params)
	if err != nil {
		return "", err
	}
	var rec dnsRecord
	if err := json.Unmarshal(result, &rec); err != nil {
		return "", fmt.Errorf("decode DNS record: %w", err)
	}
	return rec.ID, nil
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

	result, err := c.api.do(ctx, http.MethodPost,
		"accounts/"+c.accountID+"/cfd_tunnel",
		map[string]any{
			"name":          name,
			"tunnel_secret": base64.StdEncoding.EncodeToString(secret),
			"config_src":    "cloudflare",
		},
	)
	if err != nil {
		return "", "", fmt.Errorf("create tunnel: %w", err)
	}
	var tun struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(result, &tun); err != nil {
		return "", "", fmt.Errorf("decode tunnel: %w", err)
	}

	tokenResult, err := c.api.do(ctx, http.MethodGet,
		"accounts/"+c.accountID+"/cfd_tunnel/"+tun.ID+"/token",
		nil,
	)
	if err != nil {
		return "", "", fmt.Errorf("get tunnel token: %w", err)
	}
	// Result is a bare JSON string.
	var tokenStr string
	if err := json.Unmarshal(tokenResult, &tokenStr); err != nil {
		return "", "", fmt.Errorf("decode tunnel token: %w", err)
	}

	c.SetTunnelID(tun.ID)
	return tun.ID, tokenStr, nil
}

func (c *Client) DeleteTunnel(ctx context.Context, tunnelID string) error {
	if c.noop || c.accountID == "" {
		return nil
	}
	_, err := c.api.do(ctx, http.MethodDelete,
		"accounts/"+c.accountID+"/cfd_tunnel/"+tunnelID,
		nil,
	)
	if err != nil {
		return fmt.Errorf("delete tunnel %s: %w", tunnelID, err)
	}
	return nil
}

func (c *Client) SetTunnelID(id string) {
	c.tunnelMu.Lock()
	c.tunnelID = id
	c.tunnelMu.Unlock()
}

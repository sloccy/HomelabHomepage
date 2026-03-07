package cf

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

const cfBase = "https://api.cloudflare.com/client/v4"

// Client wraps the Cloudflare API for DNS record and tunnel management.
// When created with an empty token, all methods become no-ops that return nil errors.
type Client struct {
	hc        *http.Client
	token     string
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
		hc:        &http.Client{Timeout: 30 * time.Second},
		token:     token,
		zoneID:    zoneID,
		tunnelID:  tunnelID,
		accountID: accountID,
	}, nil
}

// cfResponse is the common Cloudflare API envelope.
type cfResponse[T any] struct {
	Success bool     `json:"success"`
	Errors  []cfErr  `json:"errors"`
	Result  T        `json:"result"`
}

type cfErr struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// cfDo executes an authenticated Cloudflare API request.
// body may be nil for requests with no body. out may be nil to discard the result.
func (c *Client) cfDo(ctx context.Context, method, path string, body, out any) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("marshal request: %w", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, cfBase+path, bodyReader)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("cloudflare API %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	if out == nil {
		// Caller doesn't need the result; still check for API-level errors.
		var envelope cfResponse[json.RawMessage]
		if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
			return nil // non-JSON response (e.g. 204) — ignore
		}
		if !envelope.Success && len(envelope.Errors) > 0 {
			return fmt.Errorf("cloudflare API error: %d %s", envelope.Errors[0].Code, envelope.Errors[0].Message)
		}
		return nil
	}

	var envelope cfResponse[json.RawMessage]
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("decode response: %w", err)
	}
	if !envelope.Success {
		if len(envelope.Errors) > 0 {
			return fmt.Errorf("cloudflare API error: %d %s", envelope.Errors[0].Code, envelope.Errors[0].Message)
		}
		return fmt.Errorf("cloudflare API returned failure")
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return fmt.Errorf("decode result: %w", err)
	}
	return nil
}

// ---- DNS records ------------------------------------------------------------

type dnsRecord struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

type createDNSParams struct {
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
}

type updateDNSParams struct {
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
}

// CreateRecord creates an A record and returns its ID.
func (c *Client) CreateRecord(ctx context.Context, name, ip string) (string, error) {
	if c.noop {
		return "", nil
	}
	var rec dnsRecord
	err := c.cfDo(ctx, http.MethodPost,
		"/zones/"+c.zoneID+"/dns_records",
		createDNSParams{Type: "A", Name: name, Content: ip, TTL: 60},
		&rec,
	)
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
	err := c.cfDo(ctx, http.MethodPatch,
		"/zones/"+c.zoneID+"/dns_records/"+recordID,
		updateDNSParams{Content: ip},
		nil,
	)
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
	return c.cfDo(ctx, http.MethodDelete,
		"/zones/"+c.zoneID+"/dns_records/"+recordID,
		nil, nil,
	)
}

// FindRecord looks up a record ID and current IP by exact name.
func (c *Client) FindRecord(ctx context.Context, name string) (string, string, error) {
	if c.noop {
		return "", "", nil
	}
	var records []dnsRecord
	if err := c.cfDo(ctx, http.MethodGet,
		"/zones/"+c.zoneID+"/dns_records?name="+name,
		nil, &records,
	); err != nil {
		return "", "", err
	}
	if len(records) == 0 {
		return "", "", nil
	}
	return records[0].ID, records[0].Content, nil
}

// ---- Tunnel management ------------------------------------------------------

type tunnelCreateParams struct {
	Name      string `json:"name"`
	TunnelSecret string `json:"tunnel_secret"`
	ConfigSrc string `json:"config_src"`
}

type tunnelInfo struct {
	ID string `json:"id"`
}

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
	var info tunnelInfo
	if err := c.cfDo(ctx, http.MethodPost,
		"/accounts/"+c.accountID+"/cfd_tunnel",
		tunnelCreateParams{
			Name:         name,
			TunnelSecret: base64.StdEncoding.EncodeToString(secret),
			ConfigSrc:    "cloudflare",
		},
		&info,
	); err != nil {
		return "", "", fmt.Errorf("create tunnel: %w", err)
	}

	var tokenStr string
	if err := c.cfDo(ctx, http.MethodGet,
		"/accounts/"+c.accountID+"/cfd_tunnel/"+info.ID+"/token",
		nil, &tokenStr,
	); err != nil {
		return "", "", fmt.Errorf("get tunnel token: %w", err)
	}

	c.SetTunnelID(info.ID)
	return info.ID, tokenStr, nil
}

// DeleteTunnel deletes a Cloudflare Tunnel by ID.
func (c *Client) DeleteTunnel(ctx context.Context, tunnelID string) error {
	if c.noop || c.accountID == "" {
		return nil
	}
	return c.cfDo(ctx, http.MethodDelete,
		"/accounts/"+c.accountID+"/cfd_tunnel/"+tunnelID,
		nil, nil,
	)
}

// SetTunnelID updates the active tunnel ID used for ingress management.
func (c *Client) SetTunnelID(id string) {
	c.tunnelMu.Lock()
	c.tunnelID = id
	c.tunnelMu.Unlock()
}

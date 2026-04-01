package cf

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"

	cloudflare "github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/dns"
	"github.com/cloudflare/cloudflare-go/v6/option"
	"github.com/cloudflare/cloudflare-go/v6/zero_trust"
)

// Client wraps the Cloudflare API for DNS record and tunnel management.
// When created with an empty token, all methods become no-ops that return nil errors.
type Client struct {
	api       *cloudflare.Client
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
		api:       cloudflare.NewClient(option.WithAPIToken(token)),
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
	proxied := false
	rec, err := c.api.DNS.Records.New(ctx, dns.RecordNewParams{
		ZoneID: cloudflare.F(c.zoneID),
		Body: dns.ARecordParam{
			Type:    cloudflare.F(dns.ARecordTypeA),
			Name:    cloudflare.F(name),
			Content: cloudflare.F(ip),
			TTL:     cloudflare.F(dns.TTL(60)),
			Proxied: cloudflare.F(proxied),
		},
	})
	if err != nil {
		return "", fmt.Errorf("create DNS record %s: %w", name, err)
	}
	return rec.ID, nil
}

func (c *Client) UpdateRecord(ctx context.Context, recordID, ip string) error {
	if c.noop {
		return nil
	}
	_, err := c.api.DNS.Records.Edit(ctx, recordID, dns.RecordEditParams{
		ZoneID: cloudflare.F(c.zoneID),
		Body:   dns.RecordEditParamsBody{Content: cloudflare.F(ip)},
	})
	if err != nil {
		return fmt.Errorf("update DNS record %s: %w", recordID, err)
	}
	return nil
}

func (c *Client) DeleteRecord(ctx context.Context, recordID string) error {
	if c.noop {
		return nil
	}
	_, err := c.api.DNS.Records.Delete(ctx, recordID, dns.RecordDeleteParams{
		ZoneID: cloudflare.F(c.zoneID),
	})
	return err
}

func (c *Client) FindRecord(ctx context.Context, name string) (recordID, ip string, err error) {
	if c.noop {
		return "", "", nil
	}
	page, err := c.api.DNS.Records.List(ctx, dns.RecordListParams{
		ZoneID: cloudflare.F(c.zoneID),
		Name:   cloudflare.F(dns.RecordListParamsName{Exact: cloudflare.F(name)}),
	})
	if err != nil {
		return "", "", err
	}
	if len(page.Result) == 0 {
		return "", "", nil
	}
	return page.Result[0].ID, page.Result[0].Content, nil
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
	tunnel, err := c.api.ZeroTrust.Tunnels.Cloudflared.New(ctx, zero_trust.TunnelCloudflaredNewParams{
		AccountID:    cloudflare.F(c.accountID),
		Name:         cloudflare.F(name),
		TunnelSecret: cloudflare.F(base64.StdEncoding.EncodeToString(secret)),
		ConfigSrc:    cloudflare.F(zero_trust.TunnelCloudflaredNewParamsConfigSrcCloudflare),
	})
	if err != nil {
		return "", "", fmt.Errorf("create tunnel: %w", err)
	}

	tokenStr, err := c.api.ZeroTrust.Tunnels.Cloudflared.Token.Get(ctx, tunnel.ID, zero_trust.TunnelCloudflaredTokenGetParams{
		AccountID: cloudflare.F(c.accountID),
	})
	if err != nil {
		return "", "", fmt.Errorf("get tunnel token: %w", err)
	}

	c.SetTunnelID(tunnel.ID)
	return tunnel.ID, *tokenStr, nil
}

func (c *Client) DeleteTunnel(ctx context.Context, tunnelID string) error {
	if c.noop || c.accountID == "" {
		return nil
	}
	_, err := c.api.ZeroTrust.Tunnels.Cloudflared.Delete(ctx, tunnelID, zero_trust.TunnelCloudflaredDeleteParams{
		AccountID: cloudflare.F(c.accountID),
	})
	return err
}

func (c *Client) SetTunnelID(id string) {
	c.tunnelMu.Lock()
	c.tunnelID = id
	c.tunnelMu.Unlock()
}

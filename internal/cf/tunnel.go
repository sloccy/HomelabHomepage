package cf

import (
	"context"
	"fmt"
	"log"

	cloudflare "github.com/cloudflare/cloudflare-go/v6"
	"github.com/cloudflare/cloudflare-go/v6/dns"
	"github.com/cloudflare/cloudflare-go/v6/zero_trust"
)

// TunnelEnabled reports whether Cloudflare Tunnel management is active.
func (c *Client) TunnelEnabled() bool {
	return !c.noop && c.tunnelID != "" && c.accountID != ""
}

// TunnelAvailable reports whether tunnel creation is possible (account configured),
// even if no tunnel has been created yet.
func (c *Client) TunnelAvailable() bool {
	return !c.noop && c.accountID != ""
}

// AddTunnelRoute adds a hostname ingress rule to the tunnel and creates the
// corresponding CNAME DNS record pointing to the tunnel endpoint.
// Returns the CNAME DNS record ID for later cleanup.
func (c *Client) AddTunnelRoute(ctx context.Context, hostname, backend string) (cnameID string, err error) {
	if !c.TunnelEnabled() {
		return "", nil
	}
	if err := c.modifyIngress(ctx, func(rules []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress) []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress {
		return append(rules, zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
			Hostname: cloudflare.F(hostname),
			Service:  cloudflare.F(backend),
		})
	}); err != nil {
		return "", fmt.Errorf("add tunnel route %s: %w", hostname, err)
	}
	// Remove any pre-existing DNS record with this hostname before creating the CNAME.
	// Handles stale A record IDs in the store, pre-existing manual records, etc.
	if existingID, _, _ := c.FindRecord(ctx, hostname); existingID != "" {
		_ = c.DeleteRecord(ctx, existingID) // best-effort; proceed even if this fails
	}
	cnameID, err = c.createCNAME(ctx, hostname)
	if err != nil {
		return "", fmt.Errorf("create CNAME for %s: %w", hostname, err)
	}
	return cnameID, nil
}

// RemoveTunnelRoute removes the hostname ingress rule from the tunnel and
// deletes its CNAME DNS record.
func (c *Client) RemoveTunnelRoute(ctx context.Context, hostname, cnameID string) error {
	if !c.TunnelEnabled() {
		return nil
	}
	if err := c.modifyIngress(ctx, func(rules []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress) []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress {
		var filtered []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress
		for _, r := range rules {
			if r.Hostname.Value != hostname {
				filtered = append(filtered, r)
			}
		}
		return filtered
	}); err != nil {
		return fmt.Errorf("remove tunnel route %s: %w", hostname, err)
	}
	if cnameID != "" {
		if err := c.DeleteRecord(ctx, cnameID); err != nil {
			return fmt.Errorf("delete CNAME for %s: %w", hostname, err)
		}
	}
	return nil
}

// ReplaceTunnelRoute atomically removes an old hostname route and adds a new one
// in a single ingress update, then swaps the CNAME DNS record.
// Returns the new CNAME record ID.
func (c *Client) ReplaceTunnelRoute(ctx context.Context, oldHostname, newHostname, backend, oldCNAMEID string) (newCNAMEID string, err error) {
	if !c.TunnelEnabled() {
		return "", nil
	}
	if err := c.modifyIngress(ctx, func(rules []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress) []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress {
		var filtered []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress
		for _, r := range rules {
			if r.Hostname.Value != oldHostname {
				filtered = append(filtered, r)
			}
		}
		return append(filtered, zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
			Hostname: cloudflare.F(newHostname),
			Service:  cloudflare.F(backend),
		})
	}); err != nil {
		return "", fmt.Errorf("replace tunnel route %s→%s: %w", oldHostname, newHostname, err)
	}
	if oldCNAMEID != "" {
		if err := c.DeleteRecord(ctx, oldCNAMEID); err != nil {
			// Log-only: new CNAME creation is more important.
			log.Printf("cf: delete old CNAME %s: %v", oldHostname, err)
		}
	}
	newCNAMEID, err = c.createCNAME(ctx, newHostname)
	if err != nil {
		return "", fmt.Errorf("create CNAME for %s: %w", newHostname, err)
	}
	return newCNAMEID, nil
}

// modifyIngress applies fn to the current tunnel ingress rules while holding
// the tunnel mutex, always ensuring the catch-all rule is last.
func (c *Client) modifyIngress(ctx context.Context, fn func([]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress) []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress) error {
	c.tunnelMu.Lock()
	defer c.tunnelMu.Unlock()

	result, err := c.api.ZeroTrust.Tunnels.Cloudflared.Configurations.Get(ctx, c.tunnelID, zero_trust.TunnelCloudflaredConfigurationGetParams{
		AccountID: cloudflare.F(c.accountID),
	})
	if err != nil {
		return fmt.Errorf("get tunnel config: %w", err)
	}

	// Convert the response ingress rules to param types for the update.
	// Separate named rules from the catch-all (Hostname == "").
	var named []zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress
	var catchAll *zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress
	for _, r := range result.Config.Ingress {
		rule := zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
			Hostname: cloudflare.F(r.Hostname),
			Service:  cloudflare.F(r.Service),
		}
		if r.Hostname == "" {
			catchAll = &rule
		} else {
			named = append(named, rule)
		}
	}

	named = fn(named)

	// Always re-append catch-all last; create a default if one wasn't present.
	if catchAll != nil {
		named = append(named, *catchAll)
	} else {
		named = append(named, zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{
			Service: cloudflare.F("http_status:404"),
		})
	}

	_, err = c.api.ZeroTrust.Tunnels.Cloudflared.Configurations.Update(ctx, c.tunnelID, zero_trust.TunnelCloudflaredConfigurationUpdateParams{
		AccountID: cloudflare.F(c.accountID),
		Config: cloudflare.F(zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfig{
			Ingress: cloudflare.F(named),
		}),
	})
	if err != nil {
		return fmt.Errorf("update tunnel configuration: %w", err)
	}
	return nil
}

// createCNAME creates a proxied CNAME record pointing to the tunnel endpoint.
func (c *Client) createCNAME(ctx context.Context, hostname string) (string, error) {
	if c.noop || c.zoneID == "" {
		return "", nil
	}
	rec, err := c.api.DNS.Records.New(ctx, dns.RecordNewParams{
		ZoneID: cloudflare.F(c.zoneID),
		Body: dns.CNAMERecordParam{
			Type:    cloudflare.F(dns.CNAMERecordTypeCNAME),
			Name:    cloudflare.F(hostname),
			Content: cloudflare.F(c.tunnelID + ".cfargotunnel.com"),
			TTL:     cloudflare.F(dns.TTL1),
			Proxied: cloudflare.F(true),
		},
	})
	if err != nil {
		return "", fmt.Errorf("create CNAME %s: %w", hostname, err)
	}
	return rec.ID, nil
}

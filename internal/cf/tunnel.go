package cf

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
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
	if err := c.modifyIngress(ctx, func(rules []ingressRule) []ingressRule {
		return append(removeHostname(rules, hostname), ingressRule{Hostname: hostname, Service: backend})
	}); err != nil {
		return "", fmt.Errorf("add tunnel route %s: %w", hostname, err)
	}
	// Remove any pre-existing DNS record with this hostname before creating the CNAME.
	// Handles stale A record IDs in the store, pre-existing manual records, etc.
	if existingID, _, _ := c.FindRecord(ctx, hostname); existingID != "" {
		if err := c.DeleteRecord(ctx, existingID); err != nil {
			log.Printf("cf: delete stale record for %s: %v", hostname, err)
		}
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
	if err := c.modifyIngress(ctx, func(rules []ingressRule) []ingressRule {
		return removeHostname(rules, hostname)
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
	if err := c.modifyIngress(ctx, func(rules []ingressRule) []ingressRule {
		return append(removeHostname(rules, oldHostname), ingressRule{Hostname: newHostname, Service: backend})
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
func (c *Client) modifyIngress(ctx context.Context, fn func([]ingressRule) []ingressRule) error {
	c.tunnelMu.Lock()
	defer c.tunnelMu.Unlock()

	result, err := c.api.do(ctx, http.MethodGet,
		"accounts/"+c.accountID+"/cfd_tunnel/"+c.tunnelID+"/configurations",
		nil,
	)
	if err != nil {
		return fmt.Errorf("get tunnel config: %w", err)
	}
	var cfg tunnelConfigResult
	if err := json.Unmarshal(result, &cfg); err != nil {
		return fmt.Errorf("decode tunnel config: %w", err)
	}

	// Separate named rules from the catch-all (Hostname == "").
	var named []ingressRule
	var catchAll *ingressRule
	for _, r := range cfg.Config.Ingress {
		if r.Hostname == "" {
			catchAll = &r
		} else {
			named = append(named, r)
		}
	}

	named = fn(named)

	// Always re-append catch-all last; create a default if one wasn't present.
	if catchAll != nil {
		named = append(named, *catchAll)
	} else {
		named = append(named, ingressRule{Service: "http_status:404"})
	}

	_, err = c.api.do(ctx, http.MethodPut,
		"accounts/"+c.accountID+"/cfd_tunnel/"+c.tunnelID+"/configurations",
		map[string]any{
			"config": map[string]any{
				"ingress": named,
			},
		},
	)
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
	id, err := c.createDNSRecord(ctx, map[string]any{
		"type":    "CNAME",
		"name":    hostname,
		"content": c.tunnelID + ".cfargotunnel.com",
		"ttl":     1,
		"proxied": true,
	})
	if err != nil {
		return "", fmt.Errorf("create CNAME %s: %w", hostname, err)
	}
	return id, nil
}

// removeHostname returns a new slice with all rules for hostname removed.
func removeHostname(rules []ingressRule, hostname string) []ingressRule {
	filtered := make([]ingressRule, 0, len(rules))
	for _, r := range rules {
		if r.Hostname != hostname {
			filtered = append(filtered, r)
		}
	}
	return filtered
}

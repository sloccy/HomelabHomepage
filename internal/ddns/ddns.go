package ddns

import (
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"lantern/internal/cf"
	"lantern/internal/config"
	"lantern/internal/store"
)

const ipifyURL = "https://api.ipify.org"

var ipifyClient = &http.Client{Timeout: 10 * time.Second}

// Manager periodically checks the public IP and updates Cloudflare records.
type Manager struct {
	cfg   *config.Config
	store *store.Store
	cf    *cf.Client
}

func New(cfg *config.Config, st *store.Store, cfClient *cf.Client) *Manager {
	return &Manager{cfg: cfg, store: st, cf: cfClient}
}

// Run detects the public IP on startup then every 10 minutes, updating DNS records on change.
func (m *Manager) Run(ctx context.Context) {
	// Check immediately on startup.
	m.checkAndUpdate(ctx)

	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.checkAndUpdate(ctx)
		}
	}
}

func (m *Manager) checkAndUpdate(ctx context.Context) {
	ip, err := getPublicIP(ctx)
	if err != nil {
		log.Printf("ddns: get public IP: %v", err)
		return
	}

	previous := m.store.GetPublicIP()
	if ip == previous {
		return
	}

	log.Printf("ddns: public IP changed %q → %q", previous, ip)
	m.store.SetPublicIP(ip)

	domains := m.store.GetDDNSDomains()
	if len(domains) == 0 {
		m.store.SaveLog("ddns")
		return
	}

	for _, domain := range domains {
		recordID, current, err := m.cf.FindRecord(ctx, domain)
		if err != nil {
			log.Printf("ddns: find record %s: %v", domain, err)
			continue
		}
		if recordID == "" {
			// Create it.
			id, err := m.cf.CreateRecord(ctx, domain, ip)
			if err != nil {
				log.Printf("ddns: create record %s: %v", domain, err)
			} else {
				log.Printf("ddns: created %s → %s (id %s)", domain, ip, id)
			}
			continue
		}
		if current == ip {
			continue
		}
		if err := m.cf.UpdateRecord(ctx, recordID, ip); err != nil {
			log.Printf("ddns: update %s: %v", domain, err)
		} else {
			log.Printf("ddns: updated %s → %s", domain, ip)
		}
	}
	m.store.SaveLog("ddns")
}

func getPublicIP(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ipifyURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := ipifyClient.Do(req)
	if err != nil {
		return "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(body)), nil
}

package discovery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"sync"
	"time"

	"launchpad/internal/cf"
	"launchpad/internal/config"
	"launchpad/internal/store"
)

// Discoverer orchestrates both network scanning and Docker watching.
type Discoverer struct {
	cfg   *config.Config
	store *store.Store
	cf    *cf.Client

	mu       sync.Mutex
	scanning bool
	lastScan time.Time
	nextScan time.Time
}

func New(cfg *config.Config, st *store.Store, cfClient *cf.Client) *Discoverer {
	return &Discoverer{
		cfg:   cfg,
		store: st,
		cf:    cfClient,
	}
}

// ScheduledScan runs a network scan on the configured interval.
// It does NOT scan on startup — first scan is at interval after start.
func (d *Discoverer) ScheduledScan(ctx context.Context) {
	d.mu.Lock()
	d.nextScan = time.Now().Add(d.cfg.ScanInterval)
	d.mu.Unlock()

	ticker := time.NewTicker(d.cfg.ScanInterval)
	defer ticker.Stop()
	log.Printf("discovery: scheduled scan every %s (first at %s)",
		d.cfg.ScanInterval, d.nextScan.Format(time.RFC3339))

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			d.runScan(ctx)
			d.mu.Lock()
			d.nextScan = time.Now().Add(d.cfg.ScanInterval)
			d.mu.Unlock()
		}
	}
}

// ScanNow triggers an immediate network scan (called from the API).
// Returns immediately if a scan is already in progress.
func (d *Discoverer) ScanNow(ctx context.Context) {
	d.mu.Lock()
	if d.scanning {
		d.mu.Unlock()
		return
	}
	d.scanning = true
	d.mu.Unlock()

	go func() {
		d.runScan(ctx)
		d.mu.Lock()
		d.nextScan = time.Now().Add(d.cfg.ScanInterval)
		d.mu.Unlock()
	}()
}

// Status returns current scan status for the API.
func (d *Discoverer) Status() (scanning bool, last, next time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.scanning, d.lastScan, d.nextScan
}

func (d *Discoverer) runScan(ctx context.Context) {
	log.Println("discovery: starting network scan")
	start := time.Now()
	results := d.scanNetwork(ctx, d.store.GetScanSubnets())

	var discovered []*store.DiscoveredService
	for _, r := range results {
		// Skip IPs/ports already assigned as services.
		alreadyAssigned := false
		for _, svc := range d.store.GetAllServices() {
			if svc.Target == r.url ||
				svc.Target == fmt.Sprintf("http://%s:%d", r.ip, r.port) ||
				svc.Target == fmt.Sprintf("https://%s:%d", r.ip, r.port) {
				alreadyAssigned = true
				break
			}
		}
		if alreadyAssigned {
			continue
		}
		discovered = append(discovered, &store.DiscoveredService{
			ID:           newID(),
			IP:           r.ip,
			Port:         r.port,
			Title:        r.title,
			Icon:         r.icon,
			Source:       "network",
			DiscoveredAt: time.Now(),
		})
	}

	d.store.ReplaceNetworkDiscovered(discovered)
	now := time.Now()
	d.store.SetLastScan(now)
	_ = d.store.Save()

	d.mu.Lock()
	d.lastScan = now
	d.scanning = false
	d.mu.Unlock()

	log.Printf("discovery: scan complete in %s — found %d network services",
		time.Since(start).Round(time.Second), len(discovered))
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

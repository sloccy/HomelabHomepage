package discovery

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"atlas/internal/cf"
	"atlas/internal/config"
	"atlas/internal/store"
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
	logLines []string // recent scan log lines (capped at 30)
}

// logf writes to the standard logger and appends to the internal scan log buffer.
func (d *Discoverer) logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	log.Println(msg)
	d.mu.Lock()
	ts := time.Now().Format("15:04:05")
	d.logLines = append(d.logLines, ts+" "+msg)
	if len(d.logLines) > 2000 {
		d.logLines = d.logLines[len(d.logLines)-2000:]
	}
	d.mu.Unlock()
}

// ScanLog returns a snapshot of recent scan log lines.
func (d *Discoverer) ScanLog() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]string, len(d.logLines))
	copy(out, d.logLines)
	return out
}

func New(cfg *config.Config, st *store.Store, cfClient *cf.Client) *Discoverer {
	return &Discoverer{
		cfg:   cfg,
		store: st,
		cf:    cfClient,
	}
}

// ScheduledScan runs a light discovery (mDNS + SSDP + WS-Discovery) on the
// configured interval. Full TCP sweeps are manual-only via ScanNow.
// It does NOT scan on startup — first run is at interval after start.
func (d *Discoverer) ScheduledScan(ctx context.Context) {
	d.mu.Lock()
	d.nextScan = time.Now().Add(d.cfg.ScanInterval)
	d.mu.Unlock()

	ticker := time.NewTicker(d.cfg.ScanInterval)
	defer ticker.Stop()
	log.Printf("discovery: light scan every %s (first at %s)",
		d.cfg.ScanInterval, d.nextScan.Format(time.RFC3339))

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Skip if a manual full scan is running.
			d.mu.Lock()
			skip := d.scanning
			d.mu.Unlock()
			if !skip {
				d.runLightScan(ctx)
			}

			d.mu.Lock()
			d.nextScan = time.Now().Add(d.cfg.ScanInterval)
			d.mu.Unlock()
		}
	}
}

// ScanNow triggers an immediate full network scan (called from the API).
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
		d.runScan(context.Background())
	}()
}

// Status returns current scan status for the API.
func (d *Discoverer) Status() (scanning bool, last, next time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.scanning, d.lastScan, d.nextScan
}

// runLightScan runs mDNS, SSDP, and WS-Discovery without a TCP sweep.
// Used for scheduled background discovery; does not affect d.scanning.
func (d *Discoverer) runLightScan(ctx context.Context) {
	assignedTargets := make(map[string]bool)
	for _, svc := range d.store.GetAllServices() {
		assignedTargets[svc.Target] = true
		assignedTargets[strings.TrimRight(svc.Target, "/")] = true
	}

	ch := d.scanNetwork(ctx, nil, false)
	for r := range ch {
		if assignedTargets[r.url] ||
			assignedTargets[fmt.Sprintf("http://%s:%d", r.ip, r.port)] ||
			assignedTargets[fmt.Sprintf("https://%s:%d", r.ip, r.port)] {
			continue
		}
		d.store.UpsertNetworkDiscovered(&store.DiscoveredService{
			ID:           newID(),
			IP:           r.ip,
			Port:         r.port,
			Title:        r.title,
			Icon:         r.icon,
			ServiceName:  r.serviceName,
			Confidence:   r.confidence,
			Source:       "network",
			DiscoveredAt: time.Now(),
		})
	}
	_ = d.store.Save()
}

func (d *Discoverer) runScan(ctx context.Context) {
	d.mu.Lock()
	d.logLines = nil
	d.mu.Unlock()

	d.logf("Starting full network scan…")
	start := time.Now()

	subnets := d.store.GetScanSubnets()
	if len(subnets) > 0 {
		d.logf("Using configured subnets: %v", subnets)
	} else {
		d.logf("No subnets configured — will auto-detect from local interfaces")
	}

	// Build assigned-targets set once (O(1) lookup per result).
	// Store both the raw target and a trailing-slash-stripped form so either
	// format matches regardless of how the target was originally saved.
	assignedTargets := make(map[string]bool)
	for _, svc := range d.store.GetAllServices() {
		assignedTargets[svc.Target] = true
		assignedTargets[strings.TrimRight(svc.Target, "/")] = true
	}
	d.logf("Skipping %d already-assigned service targets", len(assignedTargets)/2)

	// Clear old network entries so partial results are visible as they arrive.
	d.store.ClearNetworkDiscovered()

	ch := d.scanNetwork(ctx, subnets, true)

	count := 0
	for r := range ch {
		// Skip IPs/ports already assigned as services.
		if assignedTargets[r.url] ||
			assignedTargets[fmt.Sprintf("http://%s:%d", r.ip, r.port)] ||
			assignedTargets[fmt.Sprintf("https://%s:%d", r.ip, r.port)] {
			continue
		}
		d.store.UpsertNetworkDiscovered(&store.DiscoveredService{
			ID:           newID(),
			IP:           r.ip,
			Port:         r.port,
			Title:        r.title,
			Icon:         r.icon,
			ServiceName:  r.serviceName,
			Confidence:   r.confidence,
			Source:       "network",
			DiscoveredAt: time.Now(),
		})
		count++
		// Flush to disk every 10 results so the UI shows partial progress.
		if count%10 == 0 {
			_ = d.store.Save()
		}
	}

	now := time.Now()
	d.store.SetLastScan(now)
	_ = d.store.Save()

	d.mu.Lock()
	d.lastScan = now
	d.scanning = false
	d.mu.Unlock()

	d.logf("Scan complete: %d services found in %s",
		count, time.Since(start).Round(time.Second))
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

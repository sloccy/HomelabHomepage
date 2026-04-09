package web

import (
	"context"
	"crypto/tls"
	"maps"
	"net/http"
	"sync"
	"time"
)

// healthClient is used for background service health checks.
// Keep-alives are enabled so TCP/TLS connections are reused across the 30s polling cycles.
var healthClient = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // health-checking arbitrary backend services: TLS cert validity is not meaningful
	},
}

// healthConcurrency caps the number of simultaneous health-check goroutines.
const healthConcurrency = 20

const (
	healthUp   = "up"
	healthDown = "down"
)

// StartHealthChecker polls all assigned services every 30 seconds and records
// whether each is reachable. Any HTTP response (including 3xx/4xx/5xx) counts
// as "up" — only a connection failure counts as "down".
func (s *Server) StartHealthChecker(ctx context.Context) {
	s.checkHealth(ctx)
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.checkHealth(ctx)
		}
	}
}

func (s *Server) checkHealth(ctx context.Context) {
	services := s.store.GetAllServices()
	result := make(map[string]string, len(services))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, healthConcurrency)
	for _, svc := range services {
		if svc.SkipHealth {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(id, target string) {
			defer wg.Done()
			defer func() { <-sem }()
			status := healthDown
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, http.NoBody)
			if err == nil {
				resp, err := healthClient.Do(req)
				if err == nil {
					_ = resp.Body.Close()
					status = healthUp
				}
			}
			mu.Lock()
			result[id] = status
			mu.Unlock()
		}(svc.ID, svc.Target)
	}
	wg.Wait()
	s.healthMu.Lock()
	s.health = result
	s.healthMu.Unlock()
}

func (s *Server) healthSnapshot() map[string]string {
	s.healthMu.RLock()
	out := make(map[string]string, len(s.health))
	maps.Copy(out, s.health)
	s.healthMu.RUnlock()
	return out
}

func (s *Server) getHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.healthSnapshot())
}

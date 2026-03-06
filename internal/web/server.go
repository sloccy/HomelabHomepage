package web

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"lantern/internal/cf"
	"lantern/internal/config"
	"lantern/internal/discovery"
	"lantern/internal/store"
	"lantern/internal/sysinfo"
	"lantern/internal/tunnel"
)

// faviconClient is used to proxy favicon requests to internal services.
var faviconClient = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	},
}

// healthClient is used for background service health checks.
var healthClient = &http.Client{
	Timeout: 5 * time.Second,
	Transport: &http.Transport{
		TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
		DisableKeepAlives: true,
	},
}

//go:embed static
var staticFiles embed.FS

// Scanner is the subset of discovery.Discoverer the web server needs.
type Scanner interface {
	ScanNow(ctx context.Context)
	Status() (scanning bool, last, next time.Time)
	ScanLog() []string
}

// Server serves the web GUI and REST API.
type Server struct {
	cfg      *config.Config
	store    *store.Store
	cf       *cf.Client
	scanner  Scanner
	tunnel   *tunnel.Manager
	mux      *http.ServeMux
	healthMu sync.RWMutex
	health   map[string]string // service ID → "up" | "down"
}

func New(cfg *config.Config, st *store.Store, cfClient *cf.Client) *Server {
	s := &Server{cfg: cfg, store: st, cf: cfClient}
	s.mux = http.NewServeMux()
	s.routes()
	return s
}

func (s *Server) SetScanner(sc Scanner)              { s.scanner = sc }
func (s *Server) SetTunnelManager(t *tunnel.Manager) { s.tunnel = t }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	// Static files.
	sub, _ := fs.Sub(staticFiles, "static")
	fileServer := http.FileServer(http.FS(sub))
	s.mux.Handle("GET /", fileServer)

	// API routes (Go 1.22 pattern matching).
	s.mux.HandleFunc("GET /api/services", s.listServices)
	s.mux.HandleFunc("POST /api/services", s.createService)
	s.mux.HandleFunc("PUT /api/services/{id}", s.updateService)
	s.mux.HandleFunc("DELETE /api/services/{id}", s.deleteService)

	s.mux.HandleFunc("GET /api/discovered", s.listDiscovered)
	s.mux.HandleFunc("DELETE /api/discovered/{id}", s.deleteDiscovered)

	s.mux.HandleFunc("POST /api/scan", s.triggerScan)
	s.mux.HandleFunc("GET /api/status", s.getStatus)

	s.mux.HandleFunc("GET /api/scan/subnets", s.listScanSubnets)
	s.mux.HandleFunc("POST /api/scan/subnets", s.addScanSubnet)
	s.mux.HandleFunc("DELETE /api/scan/subnets", s.removeScanSubnet)

	s.mux.HandleFunc("GET /api/ddns", s.listDDNS)
	s.mux.HandleFunc("POST /api/ddns", s.addDDNS)
	s.mux.HandleFunc("DELETE /api/ddns/{domain}", s.removeDDNS)

	s.mux.HandleFunc("GET /api/favicon", s.getFavicon)
	s.mux.HandleFunc("POST /api/services/reorder", s.reorderServices)
	s.mux.HandleFunc("POST /api/services/{id}/icon", s.uploadServiceIcon)
	s.mux.HandleFunc("POST /api/services/{id}/favicon", s.pullServiceFavicon)
	s.mux.HandleFunc("DELETE /api/services/{id}/icon", s.clearServiceIcon)

	s.mux.HandleFunc("POST /api/discovered/{id}/ignore", s.ignoreDiscovered)
	s.mux.HandleFunc("GET /api/ignored", s.listIgnored)
	s.mux.HandleFunc("DELETE /api/ignored/{id}", s.unignoreService)

	s.mux.HandleFunc("GET /api/health", s.getHealth)
	s.mux.HandleFunc("GET /api/sysinfo", s.getSysinfo)

	s.mux.HandleFunc("GET /api/bookmarks", s.listBookmarks)
	s.mux.HandleFunc("POST /api/bookmarks", s.createBookmark)
	s.mux.HandleFunc("PUT /api/bookmarks/{id}", s.updateBookmark)
	s.mux.HandleFunc("DELETE /api/bookmarks/{id}", s.deleteBookmark)

	s.mux.HandleFunc("GET /api/settings", s.getSettings)
	s.mux.HandleFunc("PUT /api/settings", s.updateSettings)

	s.mux.HandleFunc("GET /api/tunnel", s.getTunnel)
	s.mux.HandleFunc("POST /api/tunnel", s.createTunnel)
	s.mux.HandleFunc("DELETE /api/tunnel", s.deleteTunnel)
}

// ---- Health checker ---------------------------------------------------------

// StartHealthChecker polls all assigned services every 30 seconds and records
// whether each is reachable. Any HTTP response (including 3xx/4xx/5xx) counts
// as "up" — only a connection failure counts as "down".
func (s *Server) StartHealthChecker(ctx context.Context) {
	s.checkHealth()
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.checkHealth()
		}
	}
}

func (s *Server) checkHealth() {
	services := s.store.GetAllServices()
	result := make(map[string]string, len(services))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, svc := range services {
		if svc.SkipHealth {
			continue
		}
		wg.Add(1)
		go func(id, target string) {
			defer wg.Done()
			status := "down"
			req, err := http.NewRequest(http.MethodGet, target, nil)
			if err == nil {
				resp, err := healthClient.Do(req)
				if err == nil {
					resp.Body.Close()
					status = "up"
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

func (s *Server) getHealth(w http.ResponseWriter, r *http.Request) {
	s.healthMu.RLock()
	out := make(map[string]string, len(s.health))
	for k, v := range s.health {
		out[k] = v
	}
	s.healthMu.RUnlock()
	writeJSON(w, http.StatusOK, out)
}

// ---- Helpers ----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func apiError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// ---- Services ---------------------------------------------------------------

func (s *Server) listServices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.GetAllServices())
}

type createServiceRequest struct {
	DiscoveredID string `json:"discovered_id"` // optional: assign from discovered
	Name         string `json:"name"`
	Subdomain    string `json:"subdomain"`
	Target       string `json:"target"` // required if not from discovered
	Category     string `json:"category"`
	Tunnel       bool   `json:"tunnel"` // route via CF tunnel instead of A record
}

func (s *Server) createService(w http.ResponseWriter, r *http.Request) {
	var req createServiceRequest
	if err := readJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	req.Subdomain = sanitiseSubdomain(req.Subdomain)
	if req.Subdomain == "" {
		apiError(w, http.StatusBadRequest, "subdomain is required")
		return
	}
	if s.store.GetServiceBySubdomain(req.Subdomain) != nil {
		apiError(w, http.StatusConflict, "subdomain already assigned")
		return
	}

	target := req.Target
	name := req.Name
	source := "manual"
	var containerID string
	var discoveredIcon string

	if req.DiscoveredID != "" {
		disc := s.store.GetDiscoveredByID(req.DiscoveredID)
		if disc == nil {
			apiError(w, http.StatusNotFound, "discovered service not found")
			return
		}
		scheme := "http"
		if disc.Port == 443 || disc.Port == 8443 || disc.Port == 9443 {
			scheme = "https"
		}
		target = fmt.Sprintf("%s://%s:%d", scheme, disc.IP, disc.Port)
		if name == "" {
			name = disc.Title
		}
		source = disc.Source
		containerID = disc.ContainerID
		discoveredIcon = disc.Icon
	}

	if target == "" {
		apiError(w, http.StatusBadRequest, "target is required")
		return
	}
	if name == "" {
		name = req.Subdomain
	}

	svc := &store.Service{
		ID:          newID(),
		Name:        name,
		Subdomain:   req.Subdomain,
		Target:      target,
		Icon:        discoveredIcon,
		Category:    req.Category,
		Source:      source,
		ContainerID: containerID,
		CreatedAt:   time.Now(),
	}

	// Create tunnel route or DNS A record based on per-service choice.
	hostname := req.Subdomain + "." + s.cfg.Domain
	if req.Tunnel && s.cf.TunnelEnabled() {
		cnameID, err := s.cf.AddTunnelRoute(r.Context(), hostname, svc.Target)
		if err != nil {
			log.Printf("web: add tunnel route %s: %v", req.Subdomain, err)
		} else {
			svc.DNSRecordID = cnameID
			svc.TunnelRouteID = hostname
		}
	} else if s.cfg.ServerIP != "" {
		dnsID, err := s.cf.CreateRecord(r.Context(), hostname, s.cfg.ServerIP)
		if err != nil {
			log.Printf("web: create DNS %s: %v", req.Subdomain, err)
		} else {
			svc.DNSRecordID = dnsID
		}
	}

	s.store.AddService(svc)
	if req.DiscoveredID != "" {
		s.store.RemoveDiscovered(req.DiscoveredID)
	}
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}

	// Asynchronously fetch favicon if not already a real image (empty or emoji fallback).
	if !strings.HasPrefix(svc.Icon, "data:") {
		go func(id, target string) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			icon := discovery.FetchFaviconForTarget(ctx, target)
			if icon == "" {
				return
			}
			if existing := s.store.GetServiceByID(id); existing != nil {
				updated := *existing
				updated.Icon = icon
				s.store.UpdateService(id, &updated)
				_ = s.store.Save()
			}
		}(svc.ID, svc.Target)
	}

	writeJSON(w, http.StatusCreated, svc)
}

type updateServiceRequest struct {
	Name       string  `json:"name"`
	Subdomain  string  `json:"subdomain"`
	Target     string  `json:"target"`
	Category   string  `json:"category"`
	Icon       *string `json:"icon"`        // nil = keep existing; "" = clear; non-empty = set
	Tunnel     *bool   `json:"tunnel"`      // nil = keep existing; true/false = enable/disable
	SkipHealth *bool   `json:"skip_health"` // nil = keep existing; true/false = skip health check
}

func (s *Server) updateService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	svc := s.store.GetServiceByID(id)
	if svc == nil {
		apiError(w, http.StatusNotFound, "service not found")
		return
	}

	var req updateServiceRequest
	if err := readJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid JSON")
		return
	}

	newSub := sanitiseSubdomain(req.Subdomain)
	if newSub == "" {
		newSub = svc.Subdomain
	}

	oldSub := svc.Subdomain
	oldDNSID := svc.DNSRecordID
	oldTunnelRoute := svc.TunnelRouteID

	icon := svc.Icon
	if req.Icon != nil {
		icon = *req.Icon
	}
	skipHealth := svc.SkipHealth
	if req.SkipHealth != nil {
		skipHealth = *req.SkipHealth
	}
	newTarget := firstNonEmpty(req.Target, svc.Target)
	updated := &store.Service{
		ID:          svc.ID,
		Name:        firstNonEmpty(req.Name, svc.Name),
		Subdomain:   newSub,
		Target:      newTarget,
		Icon:        icon,
		Category:    req.Category,
		Source:      svc.Source,
		ContainerID: svc.ContainerID,
		SkipHealth:  skipHealth,
		CreatedAt:   svc.CreatedAt,
	}

	// Determine whether the caller wants tunnel on/off (nil = keep current state).
	currentlyTunneled := oldTunnelRoute != ""
	wantTunnel := currentlyTunneled
	if req.Tunnel != nil {
		wantTunnel = *req.Tunnel
	}

	oldHostname := oldSub + "." + s.cfg.Domain
	newHostname := newSub + "." + s.cfg.Domain

	if s.cf.TunnelEnabled() && wantTunnel {
		if currentlyTunneled {
			// Already tunneled — re-route only if something changed.
			if newSub != oldSub || newTarget != svc.Target {
				cnameID, err := s.cf.ReplaceTunnelRoute(r.Context(), oldHostname, newHostname, newTarget, oldDNSID)
				if err != nil {
					log.Printf("web: replace tunnel route %s→%s: %v", oldSub, newSub, err)
				} else {
					updated.DNSRecordID = cnameID
					updated.TunnelRouteID = newHostname
				}
			} else {
				updated.DNSRecordID = oldDNSID
				updated.TunnelRouteID = oldTunnelRoute
			}
		} else {
			// Switching from A record to tunnel.
			if oldDNSID != "" {
				if err := s.cf.DeleteRecord(r.Context(), oldDNSID); err != nil {
					log.Printf("web: delete DNS %s: %v", oldSub, err)
				}
			}
			cnameID, err := s.cf.AddTunnelRoute(r.Context(), newHostname, newTarget)
			if err != nil {
				log.Printf("web: add tunnel route %s: %v", newSub, err)
			} else {
				updated.DNSRecordID = cnameID
				updated.TunnelRouteID = newHostname
			}
		}
	} else {
		// Want A record (or tunnel not configured).
		if currentlyTunneled {
			// Switching from tunnel to A record.
			if err := s.cf.RemoveTunnelRoute(r.Context(), oldTunnelRoute, oldDNSID); err != nil {
				log.Printf("web: remove tunnel route %s: %v", oldSub, err)
			}
			if s.cfg.ServerIP != "" {
				dnsID, err := s.cf.CreateRecord(r.Context(), newHostname, s.cfg.ServerIP)
				if err != nil {
					log.Printf("web: create DNS %s: %v", newSub, err)
				} else {
					updated.DNSRecordID = dnsID
				}
			}
		} else if newSub != oldSub {
			// Subdomain changed, swap A records.
			if oldDNSID != "" {
				if err := s.cf.DeleteRecord(r.Context(), oldDNSID); err != nil {
					log.Printf("web: delete DNS %s: %v", oldSub, err)
				}
			}
			if s.cfg.ServerIP != "" {
				dnsID, err := s.cf.CreateRecord(r.Context(), newHostname, s.cfg.ServerIP)
				if err != nil {
					log.Printf("web: create DNS %s: %v", newSub, err)
				} else {
					updated.DNSRecordID = dnsID
				}
			}
		} else {
			updated.DNSRecordID = oldDNSID
		}
	}

	s.store.UpdateService(id, updated)
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deleteService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, dnsID, tunnelRoute := s.store.DeleteService(id)
	if tunnelRoute != "" {
		if err := s.cf.RemoveTunnelRoute(r.Context(), tunnelRoute, dnsID); err != nil {
			log.Printf("web: remove tunnel route: %v", err)
		}
	} else if dnsID != "" {
		if err := s.cf.DeleteRecord(r.Context(), dnsID); err != nil {
			log.Printf("web: delete DNS record: %v", err)
		}
	}
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Discovered -------------------------------------------------------------

func (s *Server) listDiscovered(w http.ResponseWriter, r *http.Request) {
	discovered := s.store.GetAllDiscovered()
	sort.Slice(discovered, func(i, j int) bool {
		a := net.ParseIP(discovered[i].IP).To4()
		b := net.ParseIP(discovered[j].IP).To4()
		if a == nil {
			a = net.ParseIP(discovered[i].IP)
		}
		if b == nil {
			b = net.ParseIP(discovered[j].IP)
		}
		if cmp := bytes.Compare(a, b); cmp != 0 {
			return cmp < 0
		}
		return discovered[i].Port < discovered[j].Port
	})
	writeJSON(w, http.StatusOK, discovered)
}

func (s *Server) deleteDiscovered(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.store.RemoveDiscovered(id)
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Scan -------------------------------------------------------------------

func (s *Server) triggerScan(w http.ResponseWriter, r *http.Request) {
	if s.scanner != nil {
		s.scanner.ScanNow(r.Context())
	}
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "scan started"})
}

type statusResponse struct {
	Scanning      bool      `json:"scanning"`
	LastScan      time.Time `json:"last_scan"`
	NextScan      time.Time `json:"next_scan"`
	ScanInterval  string    `json:"scan_interval"`
	PublicIP      string    `json:"public_ip"`
	Domain        string    `json:"domain"`
	ServerIP      string    `json:"server_ip"`
	TunnelAvailable bool     `json:"tunnel_available"`
	TunnelEnabled bool      `json:"tunnel_enabled"`
	TunnelID      string    `json:"tunnel_id,omitempty"`
	TunnelRunning bool      `json:"tunnel_running"`
	ScanLog       []string  `json:"scan_log,omitempty"`
}

func (s *Server) getStatus(w http.ResponseWriter, r *http.Request) {
	var scanning bool
	var last, next time.Time
	var scanLog []string
	if s.scanner != nil {
		scanning, last, next = s.scanner.Status()
		scanLog = s.scanner.ScanLog()
	}
	tunnelID := ""
	tunnelRunning := false
	if s.tunnel != nil {
		st := s.tunnel.Status()
		tunnelRunning = st.Running
		if st.TunnelID != "" && len(st.TunnelID) >= 8 {
			tunnelID = st.TunnelID[:8] + "…"
		}
	} else if s.cf.TunnelEnabled() && len(s.cfg.CFTunnelID) >= 8 {
		// Fallback: externally-managed tunnel via env var (no manager).
		tunnelID = s.cfg.CFTunnelID[:8] + "…"
	}
	writeJSON(w, http.StatusOK, statusResponse{
		Scanning:      scanning,
		LastScan:      last,
		NextScan:      next,
		ScanInterval:  s.cfg.ScanInterval.String(),
		PublicIP:      s.store.GetPublicIP(),
		Domain:        s.cfg.Domain,
		ServerIP:      s.cfg.ServerIP,
		TunnelAvailable: s.cf.TunnelAvailable(),
		TunnelEnabled: s.cf.TunnelEnabled(),
		TunnelID:      tunnelID,
		TunnelRunning: tunnelRunning,
		ScanLog:       scanLog,
	})
}

// ---- Tunnel -----------------------------------------------------------------

func (s *Server) getTunnel(w http.ResponseWriter, r *http.Request) {
	if s.tunnel == nil {
		apiError(w, http.StatusNotFound, "tunnel manager not available")
		return
	}
	st := s.tunnel.Status()
	if st.TunnelID == "" {
		apiError(w, http.StatusNotFound, "no tunnel configured")
		return
	}
	writeJSON(w, http.StatusOK, st)
}

func (s *Server) createTunnel(w http.ResponseWriter, r *http.Request) {
	if s.tunnel == nil {
		apiError(w, http.StatusServiceUnavailable, "tunnel manager not available")
		return
	}
	if st := s.tunnel.Status(); st.TunnelID != "" {
		apiError(w, http.StatusConflict, "tunnel already exists")
		return
	}
	info, err := s.tunnel.Create(r.Context())
	if err != nil {
		log.Printf("web: create tunnel: %v", err)
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, tunnel.TunnelStatus{
		TunnelID:  info.TunnelID,
		Running:   true,
		CreatedAt: info.CreatedAt,
	})
}

func (s *Server) deleteTunnel(w http.ResponseWriter, r *http.Request) {
	if s.tunnel == nil {
		apiError(w, http.StatusNotFound, "tunnel manager not available")
		return
	}
	if err := s.tunnel.Delete(r.Context()); err != nil {
		log.Printf("web: delete tunnel: %v", err)
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Scan subnets -----------------------------------------------------------

func (s *Server) listScanSubnets(w http.ResponseWriter, r *http.Request) {
	subnets := s.store.GetScanSubnets()
	if subnets == nil {
		subnets = []string{}
	}
	writeJSON(w, http.StatusOK, subnets)
}

func (s *Server) addScanSubnet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CIDR string `json:"cidr"`
	}
	if err := readJSON(r, &req); err != nil || req.CIDR == "" {
		apiError(w, http.StatusBadRequest, "cidr is required")
		return
	}
	if _, _, err := net.ParseCIDR(req.CIDR); err != nil {
		apiError(w, http.StatusBadRequest, "invalid CIDR notation")
		return
	}
	s.store.AddScanSubnet(req.CIDR)
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	writeJSON(w, http.StatusCreated, map[string]string{"cidr": req.CIDR})
}

func (s *Server) removeScanSubnet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CIDR string `json:"cidr"`
	}
	if err := readJSON(r, &req); err != nil || req.CIDR == "" {
		apiError(w, http.StatusBadRequest, "cidr is required")
		return
	}
	s.store.RemoveScanSubnet(req.CIDR)
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- DDNS -------------------------------------------------------------------

func (s *Server) listDDNS(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"domains":   s.store.GetDDNSDomains(),
		"public_ip": s.store.GetPublicIP(),
	})
}

func (s *Server) addDDNS(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Domain string `json:"domain"`
	}
	if err := readJSON(r, &req); err != nil || req.Domain == "" {
		apiError(w, http.StatusBadRequest, "domain is required")
		return
	}
	req.Domain = strings.ToLower(strings.TrimSpace(req.Domain))
	s.store.AddDDNSDomain(req.Domain)

	// Immediately create/update record if we know the public IP.
	if ip := s.store.GetPublicIP(); ip != "" {
		if _, err := s.cf.CreateRecord(r.Context(), req.Domain, ip); err != nil {
			log.Printf("web: ddns create record %s: %v", req.Domain, err)
		}
	}
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	writeJSON(w, http.StatusCreated, map[string]string{"domain": req.Domain})
}

func (s *Server) removeDDNS(w http.ResponseWriter, r *http.Request) {
	domain := r.PathValue("domain")
	s.store.RemoveDDNSDomain(domain)

	recordID, _, err := s.cf.FindRecord(r.Context(), domain)
	if err == nil && recordID != "" {
		if err := s.cf.DeleteRecord(r.Context(), recordID); err != nil {
			log.Printf("web: delete ddns record %s: %v", domain, err)
		}
	}
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Favicon proxy ----------------------------------------------------------

// getFavicon proxies a favicon from an internal service target, avoiding
// mixed-content and CORS issues in the browser.
func (s *Server) getFavicon(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		http.NotFound(w, r)
		return
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		http.NotFound(w, r)
		return
	}
	faviconURL := u.Scheme + "://" + u.Host + "/favicon.ico"
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, faviconURL, nil)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	resp, err := faviconClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		http.NotFound(w, r)
		return
	}
	defer resp.Body.Close()
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/x-icon"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = io.Copy(w, io.LimitReader(resp.Body, 64*1024))
}

// ---- Service icon -----------------------------------------------------------

func (s *Server) uploadServiceIcon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	svc := s.store.GetServiceByID(id)
	if svc == nil {
		apiError(w, http.StatusNotFound, "service not found")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 512*1024)
	if err := r.ParseMultipartForm(512 * 1024); err != nil {
		apiError(w, http.StatusBadRequest, "file too large or invalid")
		return
	}
	file, hdr, err := r.FormFile("icon")
	if err != nil {
		apiError(w, http.StatusBadRequest, "icon file required")
		return
	}
	defer file.Close()
	ct := hdr.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/png"
	}
	data, err := io.ReadAll(io.LimitReader(file, 512*1024))
	if err != nil || len(data) == 0 {
		apiError(w, http.StatusBadRequest, "could not read file")
		return
	}
	updated := *svc
	updated.Icon = "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(data)
	s.store.UpdateService(id, &updated)
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	writeJSON(w, http.StatusOK, &updated)
}

func (s *Server) clearServiceIcon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	svc := s.store.GetServiceByID(id)
	if svc == nil {
		apiError(w, http.StatusNotFound, "service not found")
		return
	}
	updated := *svc
	updated.Icon = ""
	s.store.UpdateService(id, &updated)
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	writeJSON(w, http.StatusOK, &updated)
}

// ---- Service reorder --------------------------------------------------------

func (s *Server) reorderServices(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := readJSON(r, &req); err != nil || len(req.IDs) == 0 {
		apiError(w, http.StatusBadRequest, "ids array is required")
		return
	}
	s.store.ReorderServices(req.IDs)
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	w.WriteHeader(http.StatusOK)
}

// ---- Favicon pull -----------------------------------------------------------

func (s *Server) pullServiceFavicon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	svc := s.store.GetServiceByID(id)
	if svc == nil {
		apiError(w, http.StatusNotFound, "service not found")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	icon := discovery.FetchFaviconForTarget(ctx, svc.Target)
	if icon == "" {
		apiError(w, http.StatusUnprocessableEntity, "no favicon found")
		return
	}
	updated := *svc
	updated.Icon = icon
	s.store.UpdateService(id, &updated)
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	writeJSON(w, http.StatusOK, &updated)
}

// ---- Ignore discovered ------------------------------------------------------

func (s *Server) ignoreDiscovered(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.IgnoreDiscovered(id); err != nil {
		apiError(w, http.StatusNotFound, err.Error())
		return
	}
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) listIgnored(w http.ResponseWriter, r *http.Request) {
	ignored := s.store.GetIgnored()
	if ignored == nil {
		ignored = []*store.IgnoredService{}
	}
	writeJSON(w, http.StatusOK, ignored)
}

func (s *Server) unignoreService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.UnignoreService(id); err != nil {
		apiError(w, http.StatusNotFound, err.Error())
		return
	}
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Bookmarks --------------------------------------------------------------

func (s *Server) listBookmarks(w http.ResponseWriter, r *http.Request) {
	bms := s.store.GetAllBookmarks()
	if bms == nil {
		bms = []*store.Bookmark{}
	}
	writeJSON(w, http.StatusOK, bms)
}

func (s *Server) createBookmark(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name     string `json:"name"`
		URL      string `json:"url"`
		Icon     string `json:"icon"`
		Category string `json:"category"`
	}
	if err := readJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	if req.URL == "" {
		apiError(w, http.StatusBadRequest, "url is required")
		return
	}
	if req.Name == "" {
		req.Name = req.URL
	}
	bm := &store.Bookmark{
		ID:       newID(),
		Name:     req.Name,
		URL:      req.URL,
		Icon:     req.Icon,
		Category: req.Category,
	}
	s.store.AddBookmark(bm)
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	writeJSON(w, http.StatusCreated, bm)
}

func (s *Server) updateBookmark(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Name     string `json:"name"`
		URL      string `json:"url"`
		Icon     string `json:"icon"`
		Category string `json:"category"`
	}
	if err := readJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	updated := &store.Bookmark{
		ID:       id,
		Name:     req.Name,
		URL:      req.URL,
		Icon:     req.Icon,
		Category: req.Category,
	}
	if !s.store.UpdateBookmark(id, updated) {
		apiError(w, http.StatusNotFound, "bookmark not found")
		return
	}
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deleteBookmark(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.store.DeleteBookmark(id) {
		apiError(w, http.StatusNotFound, "bookmark not found")
		return
	}
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	w.WriteHeader(http.StatusNoContent)
}

// ---- Settings ---------------------------------------------------------------

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.GetSettings())
}

func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var req store.Settings
	if err := readJSON(r, &req); err != nil {
		apiError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	s.store.UpdateSettings(req)
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	writeJSON(w, http.StatusOK, s.store.GetSettings())
}

// ---- Sysinfo ----------------------------------------------------------------

func (s *Server) getSysinfo(w http.ResponseWriter, r *http.Request) {
	stats, err := sysinfo.Get()
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

// ---- Utilities --------------------------------------------------------------

func sanitiseSubdomain(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else if r == '_' || r == '.' {
			b.WriteRune('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

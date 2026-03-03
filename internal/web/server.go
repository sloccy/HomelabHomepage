package web

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net"
	"net/http"
	"strings"
	"time"

	"launchpad/internal/cf"
	"launchpad/internal/config"
	"launchpad/internal/store"
)

//go:embed static
var staticFiles embed.FS

// Scanner is the subset of discovery.Discoverer the web server needs.
type Scanner interface {
	ScanNow(ctx context.Context)
	Status() (scanning bool, last, next time.Time)
}

// Server serves the web GUI and REST API.
type Server struct {
	cfg     *config.Config
	store   *store.Store
	cf      *cf.Client
	scanner Scanner
	mux     *http.ServeMux
}

func New(cfg *config.Config, st *store.Store, cfClient *cf.Client) *Server {
	s := &Server{cfg: cfg, store: st, cf: cfClient}
	s.mux = http.NewServeMux()
	s.routes()
	return s
}

func (s *Server) SetScanner(sc Scanner) { s.scanner = sc }

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
		Source:      source,
		ContainerID: containerID,
		CreatedAt:   time.Now(),
	}

	// Create DNS record.
	dnsID, err := s.cf.CreateRecord(r.Context(), req.Subdomain+"."+s.cfg.Domain, s.cfg.ServerIP)
	if err != nil {
		log.Printf("web: create DNS %s: %v", req.Subdomain, err)
	} else {
		svc.DNSRecordID = dnsID
	}

	s.store.AddService(svc)
	if req.DiscoveredID != "" {
		s.store.RemoveDiscovered(req.DiscoveredID)
	}
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	writeJSON(w, http.StatusCreated, svc)
}

type updateServiceRequest struct {
	Name      string `json:"name"`
	Subdomain string `json:"subdomain"`
	Target    string `json:"target"`
	Icon      string `json:"icon"`
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

	updated := &store.Service{
		ID:          svc.ID,
		Name:        firstNonEmpty(req.Name, svc.Name),
		Subdomain:   newSub,
		Target:      firstNonEmpty(req.Target, svc.Target),
		Icon:        firstNonEmpty(req.Icon, svc.Icon),
		Source:      svc.Source,
		ContainerID: svc.ContainerID,
		CreatedAt:   svc.CreatedAt,
	}

	if newSub != oldSub {
		if oldDNSID != "" {
			if err := s.cf.DeleteRecord(r.Context(), oldDNSID); err != nil {
				log.Printf("web: delete DNS %s: %v", oldSub, err)
			}
		}
		dnsID, err := s.cf.CreateRecord(r.Context(), newSub+"."+s.cfg.Domain, s.cfg.ServerIP)
		if err != nil {
			log.Printf("web: create DNS %s: %v", newSub, err)
		} else {
			updated.DNSRecordID = dnsID
		}
	} else {
		updated.DNSRecordID = oldDNSID
	}

	s.store.UpdateService(id, updated)
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) deleteService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_, dnsID := s.store.DeleteService(id)
	if dnsID != "" {
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
	writeJSON(w, http.StatusOK, s.store.GetAllDiscovered())
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
	Scanning     bool      `json:"scanning"`
	LastScan     time.Time `json:"last_scan"`
	NextScan     time.Time `json:"next_scan"`
	ScanInterval string    `json:"scan_interval"`
	PublicIP     string    `json:"public_ip"`
	Domain       string    `json:"domain"`
	ServerIP     string    `json:"server_ip"`
}

func (s *Server) getStatus(w http.ResponseWriter, r *http.Request) {
	var scanning bool
	var last, next time.Time
	if s.scanner != nil {
		scanning, last, next = s.scanner.Status()
	}
	writeJSON(w, http.StatusOK, statusResponse{
		Scanning:     scanning,
		LastScan:     last,
		NextScan:     next,
		ScanInterval: s.cfg.ScanInterval.String(),
		PublicIP:     s.store.GetPublicIP(),
		Domain:       s.cfg.Domain,
		ServerIP:     s.cfg.ServerIP,
	})
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

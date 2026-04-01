package web

import (
	"context"
	"net"
	"net/http"
	"time"

	"lantern/internal/store"
	"lantern/internal/util"
)

func (s *Server) listDiscovered(w http.ResponseWriter, r *http.Request) {
	discovered := s.store.GetAllDiscovered()
	util.SortDiscoveredByIP(discovered)
	writeJSON(w, http.StatusOK, discovered)
}

func (s *Server) deleteDiscovered(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.store.RemoveDiscovered(id)
	s.save()
	w.WriteHeader(http.StatusOK)
}

func (s *Server) triggerScan(w http.ResponseWriter, r *http.Request) {
	if s.scanner != nil {
		s.scanner.ScanNow(context.Background())
	}
	renderTemplate(w, "status.html", s.buildStatusData())
}

type statusResponse struct {
	Scanning        bool      `json:"scanning"`
	LastScan        time.Time `json:"last_scan"`
	NextScan        time.Time `json:"next_scan"`
	ScanInterval    string    `json:"scan_interval"`
	PublicIP        string    `json:"public_ip"`
	Domain          string    `json:"domain"`
	ServerIP        string    `json:"server_ip"`
	TunnelAvailable bool      `json:"tunnel_available"`
	TunnelEnabled   bool      `json:"tunnel_enabled"`
	TunnelID        string    `json:"tunnel_id,omitempty"`
	TunnelRunning   bool      `json:"tunnel_running"`
	ScanLog         []string  `json:"scan_log,omitempty"`
}

func (s *Server) getStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.buildStatusData())
}

func (s *Server) listScanSubnets(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.GetScanSubnets())
}

func (s *Server) addScanSubnet(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	if err := r.ParseForm(); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid form data")
		return
	}
	cidr := r.FormValue("cidr")
	if cidr == "" {
		errorResponse(w, http.StatusBadRequest, "cidr is required")
		return
	}
	if _, _, err := net.ParseCIDR(cidr); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid CIDR notation")
		return
	}
	s.store.AddScanSubnet(cidr)
	s.save()
	renderTemplate(w, "subnets.html", subnetsFragData{Subnets: s.store.GetScanSubnets()})
}

func (s *Server) removeScanSubnet(w http.ResponseWriter, r *http.Request) {
	cidr := r.URL.Query().Get("cidr")
	if cidr == "" {
		apiError(w, http.StatusBadRequest, "cidr is required")
		return
	}
	s.store.RemoveScanSubnet(cidr)
	s.save()
	w.WriteHeader(http.StatusOK)
}

func (s *Server) ignoreDiscovered(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.IgnoreDiscovered(id); err != nil {
		apiError(w, http.StatusNotFound, err.Error())
		return
	}
	s.save()
	w.Header().Set("HX-Trigger", `{"refreshIgnored":null}`)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) listIgnored(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.GetIgnored())
}

func (s *Server) unignoreService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ig, err := s.store.UnignoreService(id)
	if err != nil {
		apiError(w, http.StatusNotFound, err.Error())
		return
	}
	// Re-add as a discovered service so it appears in the Discovered section.
	s.store.AddDiscovered(&store.DiscoveredService{
		ID:           ig.ID,
		IP:           ig.IP,
		Port:         ig.Port,
		Title:        ig.Title,
		Source:       store.SourceNetwork,
		DiscoveredAt: time.Now(),
	})
	s.save()
	w.Header().Set("HX-Trigger", `{"refreshDiscovered":null}`)
	w.WriteHeader(http.StatusOK)
}

package web

import (
	"bytes"
	"context"
	"log"
	"net"
	"net/http"
	"sort"
	"time"

	"lantern/internal/store"
)

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
	if err := r.ParseForm(); err != nil {
		errorTrigger(w, "invalid form data")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	cidr := r.FormValue("cidr")
	if cidr == "" {
		errorTrigger(w, "cidr is required")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if _, _, err := net.ParseCIDR(cidr); err != nil {
		errorTrigger(w, "invalid CIDR notation")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.store.AddScanSubnet(cidr)
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	renderTemplate(w, "subnets.html", subnetsFragData{Subnets: s.store.GetScanSubnets()})
}

func (s *Server) removeScanSubnet(w http.ResponseWriter, r *http.Request) {
	cidr := r.URL.Query().Get("cidr")
	if cidr == "" {
		apiError(w, http.StatusBadRequest, "cidr is required")
		return
	}
	s.store.RemoveScanSubnet(cidr)
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) ignoreDiscovered(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.IgnoreDiscovered(id); err != nil {
		apiError(w, http.StatusNotFound, err.Error())
		return
	}
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	hxTrigger(w, "refreshIgnored", nil)
	w.WriteHeader(http.StatusOK)
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
		Source:       "network",
		DiscoveredAt: time.Now(),
	})
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	hxTrigger(w, "refreshDiscovered", nil)
	w.WriteHeader(http.StatusOK)
}

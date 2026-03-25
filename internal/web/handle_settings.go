package web

import (
	"log"
	"net/http"
	"strings"

	"lantern/internal/sysinfo"
)

func (s *Server) getSysinfo(w http.ResponseWriter, r *http.Request) {
	stats, err := sysinfo.Get()
	if err != nil {
		apiError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, stats)
}

func (s *Server) listDDNS(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"domains":   s.store.GetDDNSDomains(),
		"public_ip": s.store.GetPublicIP(),
	})
}

func (s *Server) addDDNS(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		errorTrigger(w, "invalid form data")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	domain := strings.ToLower(strings.TrimSpace(r.FormValue("domain")))
	if domain == "" {
		errorTrigger(w, "domain is required")
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	s.store.AddDDNSDomain(domain)

	// Immediately create/update record if we know the public IP.
	if ip := s.store.GetPublicIP(); ip != "" {
		if _, err := s.cf.CreateRecord(r.Context(), domain, ip); err != nil {
			log.Printf("web: ddns create record %s: %v", domain, err)
		}
	}
	if err := s.store.Save(); err != nil {
		log.Printf("web: save: %v", err)
	}
	renderTemplate(w, "ddns.html", ddnsFragData{
		Domains:  s.store.GetDDNSDomains(),
		PublicIP: s.store.GetPublicIP(),
	})
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
	w.WriteHeader(http.StatusOK)
}

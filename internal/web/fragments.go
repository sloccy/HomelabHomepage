package web

import (
	"bytes"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"lantern/internal/store"
	"lantern/internal/sysinfo"
)

func (s *Server) uniqueCategories() []string {
	return getUniqueCategories(s.store.GetAllServices(), s.store.GetAllBookmarks())
}

// ---- Fragment handlers (GET /fragments/*) -----------------------------------

func (s *Server) fragServicesGrid(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	services := s.store.GetAllServices()
	if q != "" {
		filtered := services[:0]
		for _, svc := range services {
			if strings.Contains(strings.ToLower(svc.Name), q) ||
				strings.Contains(strings.ToLower(svc.Subdomain), q) ||
				strings.Contains(strings.ToLower(svc.Target), q) {
				filtered = append(filtered, svc)
			}
		}
		services = filtered
	}
	renderTemplate(w, "services-grid.html", buildServicesGrid(services, s.cfg.Domain, s.healthSnapshot(), q != ""))
}

func (s *Server) fragBookmarksGrid(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	bookmarks := s.store.GetAllBookmarks()
	if q != "" {
		filtered := bookmarks[:0]
		for _, bm := range bookmarks {
			if strings.Contains(strings.ToLower(bm.Name), q) ||
				strings.Contains(strings.ToLower(bm.URL), q) {
				filtered = append(filtered, bm)
			}
		}
		bookmarks = filtered
	}
	renderTemplate(w, "bookmarks-grid.html", buildBookmarksGrid(bookmarks))
}

func (s *Server) fragSysinfo(w http.ResponseWriter, r *http.Request) {
	stats, err := sysinfo.Get()
	if err != nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	renderTemplate(w, "sysinfo.html", stats)
}

func (s *Server) fragStatus(w http.ResponseWriter, r *http.Request) {
	renderTemplate(w, "status.html", s.buildStatusData())
}

func (s *Server) fragTunnel(w http.ResponseWriter, r *http.Request) {
	data := s.buildTunnelFragData()
	if !data.Available {
		// No Cloudflare tunnel configured — tell htmx to delete the placeholder div.
		w.Header().Set("HX-Reswap", "delete")
		w.WriteHeader(http.StatusOK)
		return
	}
	renderTemplate(w, "tunnel.html", data)
}

func (s *Server) fragSubnets(w http.ResponseWriter, r *http.Request) {
	subnets := s.store.GetScanSubnets()
	if subnets == nil {
		subnets = []string{}
	}
	renderTemplate(w, "subnets.html", subnetsFragData{Subnets: subnets})
}

func (s *Server) fragServicesTable(w http.ResponseWriter, r *http.Request) {
	services := s.store.GetAllServices()
	sort.Slice(services, func(i, j int) bool {
		return strings.ToLower(services[i].Name) < strings.ToLower(services[j].Name)
	})
	renderTemplate(w, "services-table.html", servicesTableData{
		Services: services,
		Domain:   s.cfg.Domain,
	})
}

func (s *Server) fragServiceFormAdd(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("HX-Trigger-After-Swap", "openmodal")
	renderTemplate(w, "service-form.html", serviceFormData{
		Categories:    s.uniqueCategories(),
		Domain:        s.cfg.Domain,
		TunnelEnabled: s.cf.TunnelEnabled(),
	})
}

func (s *Server) fragServiceFormEdit(w http.ResponseWriter, r *http.Request) {
	svc := s.store.GetServiceByID(r.PathValue("id"))
	if svc == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("HX-Trigger-After-Swap", "openmodal")
	renderTemplate(w, "service-form.html", serviceFormData{
		Service:       svc,
		Categories:    s.uniqueCategories(),
		Domain:        s.cfg.Domain,
		TunnelEnabled: s.cf.TunnelEnabled(),
	})
}

func (s *Server) fragDiscovered(w http.ResponseWriter, r *http.Request) {
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
	renderTemplate(w, "discovered.html", discovered)
}

func (s *Server) fragIgnored(w http.ResponseWriter, r *http.Request) {
	renderTemplate(w, "ignored.html", s.store.GetIgnored())
}

func (s *Server) fragAssignForm(w http.ResponseWriter, r *http.Request) {
	disc := s.store.GetDiscoveredByID(r.PathValue("id"))
	if disc == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("HX-Trigger-After-Swap", "openmodal")
	renderTemplate(w, "assign-form.html", assignFormData{
		Discovered:    disc,
		Categories:    s.uniqueCategories(),
		Domain:        s.cfg.Domain,
		TunnelEnabled: s.cf.TunnelEnabled(),
	})
}

func (s *Server) fragBookmarksTable(w http.ResponseWriter, r *http.Request) {
	renderTemplate(w, "bookmarks-table.html", s.store.GetAllBookmarks())
}

func (s *Server) fragBookmarkFormAdd(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("HX-Trigger-After-Swap", "openmodal")
	renderTemplate(w, "bookmark-form.html", bookmarkFormData{
		Categories: s.uniqueCategories(),
	})
}

func (s *Server) fragBookmarkFormEdit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	bm := findBookmarkByID(s.store.GetAllBookmarks(), id)
	if bm == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("HX-Trigger-After-Swap", "openmodal")
	renderTemplate(w, "bookmark-form.html", bookmarkFormData{
		Bookmark:   bm,
		Categories: s.uniqueCategories(),
	})
}

func (s *Server) fragDDNS(w http.ResponseWriter, r *http.Request) {
	renderTemplate(w, "ddns.html", ddnsFragData{
		Domains:  s.store.GetDDNSDomains(),
		PublicIP: s.store.GetPublicIP(),
	})
}

// ---- Data helpers -----------------------------------------------------------

func (s *Server) buildStatusData() statusResponse {
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
		tunnelID = s.cfg.CFTunnelID[:8] + "…"
	}
	return statusResponse{
		Scanning:        scanning,
		LastScan:        last,
		NextScan:        next,
		ScanInterval:    s.cfg.ScanInterval.String(),
		PublicIP:        s.store.GetPublicIP(),
		Domain:          s.cfg.Domain,
		ServerIP:        s.cfg.ServerIP,
		TunnelAvailable: s.cf.TunnelAvailable(),
		TunnelEnabled:   s.cf.TunnelEnabled(),
		TunnelID:        tunnelID,
		TunnelRunning:   tunnelRunning,
		ScanLog:         scanLog,
	}
}

func (s *Server) buildTunnelFragData() tunnelFragData {
	data := tunnelFragData{Available: s.cf.TunnelAvailable()}
	if s.tunnel != nil {
		data.Status = s.tunnel.Status()
	}
	// Show tunnel section if CF creds exist OR if an external tunnel ID is set.
	if s.cf.TunnelEnabled() || (s.tunnel != nil && data.Status.TunnelID != "") {
		data.Available = true
	}
	return data
}

// ---- Move service (reorder by direction) ------------------------------------

func (s *Server) moveService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	direction := r.FormValue("direction")

	services := s.store.GetAllServices()
	sort.Slice(services, func(i, j int) bool {
		if services[i].Order != services[j].Order {
			return services[i].Order < services[j].Order
		}
		return services[i].Name < services[j].Name
	})

	ids := make([]string, len(services))
	idx := -1
	for i, svc := range services {
		ids[i] = svc.ID
		if svc.ID == id {
			idx = i
		}
	}
	if idx < 0 {
		http.NotFound(w, r)
		return
	}

	var swapIdx int
	switch direction {
	case "left":
		swapIdx = idx - 1
	case "right":
		swapIdx = idx + 1
	default:
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if swapIdx >= 0 && swapIdx < len(ids) {
		ids[idx], ids[swapIdx] = ids[swapIdx], ids[idx]
		s.store.ReorderServices(ids)
		if err := s.store.Save(); err != nil {
			log.Printf("web: save reorder: %v", err)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// ---- Move bookmark (reorder by direction) ------------------------------------

func (s *Server) moveBookmark(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	direction := r.FormValue("direction")

	bookmarks := s.store.GetAllBookmarks()
	sort.Slice(bookmarks, func(i, j int) bool {
		if bookmarks[i].Order != bookmarks[j].Order {
			return bookmarks[i].Order < bookmarks[j].Order
		}
		return bookmarks[i].Name < bookmarks[j].Name
	})

	ids := make([]string, len(bookmarks))
	idx := -1
	for i, bm := range bookmarks {
		ids[i] = bm.ID
		if bm.ID == id {
			idx = i
		}
	}
	if idx < 0 {
		http.NotFound(w, r)
		return
	}

	var swapIdx int
	switch direction {
	case "left":
		swapIdx = idx - 1
	case "right":
		swapIdx = idx + 1
	default:
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if swapIdx >= 0 && swapIdx < len(ids) {
		ids[idx], ids[swapIdx] = ids[swapIdx], ids[idx]
		s.store.ReorderBookmarks(ids)
		if err := s.store.Save(); err != nil {
			log.Printf("web: save reorder bookmarks: %v", err)
		}
	}

	w.WriteHeader(http.StatusNoContent)
}

// ---- Helpers ----------------------------------------------------------------

func findBookmarkByID(bms []*store.Bookmark, id string) *store.Bookmark {
	for _, bm := range bms {
		if bm.ID == id {
			return bm
		}
	}
	return nil
}


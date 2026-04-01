package web

import (
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"lantern/internal/store"
	"lantern/internal/sysinfo"
	"lantern/internal/util"
)

func (s *Server) uniqueCategories() []string {
	return getUniqueCategories(s.store.GetAllServices(), s.store.GetAllBookmarks())
}

// ---- Fragment handlers (GET /fragments/*) -----------------------------------

func (s *Server) fragServicesGrid(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	services := filterByQuery(s.store.GetAllServices(), q, func(svc *store.Service) string {
		return svc.Name + " " + svc.Subdomain + " " + svc.Target
	})
	renderTemplate(w, "services-grid.html", buildServicesGrid(services, s.cfg.Domain, s.healthSnapshot(), q != ""))
}

func (s *Server) fragBookmarksGrid(w http.ResponseWriter, r *http.Request) {
	q := strings.ToLower(r.URL.Query().Get("q"))
	bookmarks := filterByQuery(s.store.GetAllBookmarks(), q, func(bm *store.Bookmark) string {
		return bm.Name + " " + bm.URL
	})
	renderTemplate(w, "bookmarks-grid.html", buildBookmarksGrid(bookmarks))
}

// filterByQuery returns items whose text (lowercased) contains q (already lowercased).
// Returns all items unchanged when q is empty.
func filterByQuery[T any](items []*T, q string, text func(*T) string) []*T {
	if q == "" {
		return items
	}
	var out []*T
	for _, v := range items {
		if strings.Contains(strings.ToLower(text(v)), q) {
			out = append(out, v)
		}
	}
	return out
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
	renderTemplate(w, "subnets.html", subnetsFragData{Subnets: s.store.GetScanSubnets()})
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
	util.SortDiscoveredByIP(discovered)
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
	bm := s.store.GetBookmarkByID(id)
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

// ---- Move (reorder by direction) --------------------------------------------

type reorderItem struct {
	ID    string
	Order int
	Name  string
}

// doMove swaps the item with the given id one position left or right in the
// ordering, then calls reorder and save. Writes the HTTP response.
func doMove(w http.ResponseWriter, r *http.Request, items []reorderItem, reorder func([]string), save func() error) {
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	id := r.PathValue("id")
	direction := r.FormValue("direction")

	sort.Slice(items, func(i, j int) bool {
		if items[i].Order != items[j].Order {
			return items[i].Order < items[j].Order
		}
		return items[i].Name < items[j].Name
	})

	ids := make([]string, len(items))
	idx := -1
	for i, item := range items {
		ids[i] = item.ID
		if item.ID == id {
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
		reorder(ids)
		if err := save(); err != nil {
			log.Printf("web: save reorder: %v", err)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) moveService(w http.ResponseWriter, r *http.Request) {
	svcs := s.store.GetAllServices()
	items := make([]reorderItem, len(svcs))
	for i, svc := range svcs {
		items[i] = reorderItem{svc.ID, svc.Order, svc.Name}
	}
	doMove(w, r, items, s.store.ReorderServices, s.store.Save)
}

func (s *Server) moveBookmark(w http.ResponseWriter, r *http.Request) {
	bms := s.store.GetAllBookmarks()
	items := make([]reorderItem, len(bms))
	for i, bm := range bms {
		items[i] = reorderItem{bm.ID, bm.Order, bm.Name}
	}
	doMove(w, r, items, s.store.ReorderBookmarks, s.store.Save)
}

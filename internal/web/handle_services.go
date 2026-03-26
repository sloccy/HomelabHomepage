package web

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"lantern/internal/discovery"
	"lantern/internal/store"
	"lantern/internal/util"
)

func (s *Server) listServices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.GetAllServices())
}

type createServiceRequest struct {
	DiscoveredID string `json:"discovered_id"` // optional: assign from discovered
	Name         string `json:"name"`
	Subdomain    string `json:"subdomain"`
	Target       string `json:"target"` // required if not from discovered
	Category     string `json:"category"`
	Tunnel       bool   `json:"tunnel"`      // route via CF tunnel instead of A record
	DirectOnly   bool   `json:"direct_only"` // no subdomain/DNS; link directly to target
	SkipHealth   bool   `json:"skip_health"` // skip health check polling
}

func (s *Server) createService(w http.ResponseWriter, r *http.Request) {
	const maxUpload = 5 << 20 // 5 MB
	if err := r.ParseMultipartForm(maxUpload); err != nil {
		if err := r.ParseForm(); err != nil {
			errorResponse(w, http.StatusBadRequest, "invalid form data")
			return
		}
	}
	var req createServiceRequest
	req.DiscoveredID = r.FormValue("discovered_id")
	req.Name = r.FormValue("name")
	req.Subdomain = r.FormValue("subdomain")
	req.Target = r.FormValue("target")
	req.Category = r.FormValue("category")
	req.Tunnel = util.ParseFormBool(r.FormValue("tunnel"))
	req.DirectOnly = util.ParseFormBool(r.FormValue("direct_only"))
	req.SkipHealth = util.ParseFormBool(r.FormValue("skip_health"))

	// Read uploaded icon if provided (stored as a file after the service ID is known).
	var uploadedIconData []byte
	if r.MultipartForm != nil {
		if fh := r.MultipartForm.File["icon"]; len(fh) > 0 {
			f, err := fh[0].Open()
			if err == nil {
				defer f.Close()
				data, err := io.ReadAll(io.LimitReader(f, 512*1024))
				if err == nil && len(data) > 0 {
					uploadedIconData = data
				}
			}
		}
	}
	req.Subdomain = sanitiseSubdomain(req.Subdomain)
	if !req.DirectOnly {
		if req.Subdomain == "" {
			errorResponse(w, http.StatusBadRequest, "subdomain is required")
			return
		}
		if s.store.GetServiceBySubdomain(req.Subdomain) != nil {
			errorResponse(w, http.StatusConflict, "subdomain already assigned")
			return
		}
	}

	target := req.Target
	name := req.Name
	source := "manual"
	var containerID string
	var containerName string
	var discoveredIcon string

	if req.DiscoveredID != "" {
		disc := s.store.GetDiscoveredByID(req.DiscoveredID)
		if disc == nil {
			errorResponse(w, http.StatusNotFound, "discovered service not found")
			return
		}
		scheme := "http"
		if util.IsHTTPSPort(disc.Port) {
			scheme = "https"
		}
		target = fmt.Sprintf("%s://%s:%d", scheme, disc.IP, disc.Port)
		if name == "" {
			name = disc.Title
		}
		source = disc.Source
		containerID = disc.ContainerID
		containerName = disc.ContainerName
		discoveredIcon = disc.Icon
	}

	if target == "" {
		errorResponse(w, http.StatusBadRequest, "target is required")
		return
	}
	if name == "" {
		name = req.Subdomain
	}

	svcID := newID()
	subdomain := req.Subdomain
	if req.DirectOnly {
		subdomain = "_direct_" + svcID
	}

	maxOrder := 0
	for _, existing := range s.store.GetAllServices() {
		if existing.Order > maxOrder {
			maxOrder = existing.Order
		}
	}

	// Determine icon source: uploaded file > discovered icon > empty.
	icon := ""
	if len(uploadedIconData) > 0 {
		icon = "file" // written to disk below after svcID is set
	} else if discoveredIcon != "" {
		icon = discoveredIcon
	}

	svc := &store.Service{
		ID:            svcID,
		Name:          name,
		Subdomain:     subdomain,
		Target:        target,
		Icon:          icon,
		Category:      req.Category,
		Source:        source,
		ContainerID:   containerID,
		ContainerName: containerName,
		DirectOnly:    req.DirectOnly,
		SkipHealth:    req.SkipHealth,
		Order:         maxOrder + 1,
		CreatedAt:     time.Now(),
	}

	// Create tunnel route or DNS A record based on per-service choice.
	// reconcileDNS with a zero old state handles creation; direct-only is a no-op.
	want := dnsState{
		Subdomain:  subdomain,
		Target:     svc.Target,
		DirectOnly: req.DirectOnly,
		Tunnel:     req.Tunnel,
	}
	svc.DNSRecordID, svc.TunnelRouteID = s.reconcileDNS(r.Context(), dnsState{DirectOnly: true}, want)

	// Write uploaded icon file now that svcID is known.
	if len(uploadedIconData) > 0 {
		if err := s.store.WriteIcon(svcID, uploadedIconData); err != nil {
			log.Printf("web: write icon %s: %v", svcID, err)
			svc.Icon = "" // don't persist "file" marker if write failed
		}
	}

	s.store.AddService(svc)
	if req.DiscoveredID != "" {
		// Copy icon file from discovered service to new service.
		if discoveredIcon == "file" {
			if data, err := s.store.ReadIcon(req.DiscoveredID); err == nil {
				_ = s.store.WriteIcon(svcID, data)
			}
		}
		s.store.RemoveDiscovered(req.DiscoveredID)
	}
	s.save()

	// Asynchronously fetch favicon if no icon is set yet.
	if svc.Icon == "" {
		go func(id, target string) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if !fetchAndWriteFavicon(ctx, s.store, id, target) {
				return
			}
			if existing := s.store.GetServiceByID(id); existing != nil {
				updated := *existing
				updated.Icon = "file"
				s.store.UpdateService(id, &updated)
				s.save()
			}
		}(svc.ID, svc.Target)
	}

	toastTrigger(w, "Service added", "success", "refreshServicesTable", "refreshDiscovered")
	w.WriteHeader(http.StatusNoContent)
}

type updateServiceRequest struct {
	Name       string  `json:"name"`
	Subdomain  string  `json:"subdomain"`
	Target     string  `json:"target"`
	Category   string  `json:"category"`
	Icon       *string `json:"icon"`        // nil = keep existing; "" = clear; non-empty = set
	Tunnel     *bool   `json:"tunnel"`      // nil = keep existing; true/false = enable/disable
	SkipHealth *bool   `json:"skip_health"` // nil = keep existing; true/false = skip health check
	DirectOnly *bool   `json:"direct_only"` // nil = keep existing; true/false = direct link only
}

// dnsState captures the DNS/tunnel configuration of a service, used as input and output for reconcileDNS.
type dnsState struct {
	Subdomain     string
	Target        string
	DirectOnly    bool
	Tunnel        bool // true when routed via CF tunnel (TunnelRouteID != "")
	DNSRecordID   string
	TunnelRouteID string
}

// reconcileDNS computes the desired DNS/tunnel state given old and new configurations, applying
// the necessary Cloudflare API calls. It returns the resulting DNSRecordID and TunnelRouteID.
// When want.DirectOnly is true, any existing DNS/tunnel records are removed and zero values returned.
func (s *Server) reconcileDNS(ctx context.Context, old, want dnsState) (dnsID, tunnelRouteID string) {
	if want.DirectOnly {
		// Tear down any existing records; direct-only services have no DNS presence.
		if old.TunnelRouteID != "" {
			if err := s.cf.RemoveTunnelRoute(ctx, old.TunnelRouteID, old.DNSRecordID); err != nil {
				log.Printf("web: remove tunnel route %s: %v", old.Subdomain, err)
			}
		} else if old.DNSRecordID != "" {
			if err := s.cf.DeleteRecord(ctx, old.DNSRecordID); err != nil {
				log.Printf("web: delete DNS %s: %v", old.Subdomain, err)
			}
		}
		return "", ""
	}

	oldHostname := old.Subdomain + "." + s.cfg.Domain
	newHostname := want.Subdomain + "." + s.cfg.Domain

	if old.DirectOnly {
		// Switching from direct-only to a real subdomain: create records from scratch.
		if want.Tunnel && s.cf.TunnelEnabled() {
			id, err := s.cf.AddTunnelRoute(ctx, newHostname, want.Target)
			if err != nil {
				log.Printf("web: add tunnel route %s: %v", want.Subdomain, err)
			} else {
				return id, newHostname
			}
		} else if s.cfg.ServerIP != "" {
			id, err := s.cf.CreateRecord(ctx, newHostname, s.cfg.ServerIP)
			if err != nil {
				log.Printf("web: create DNS %s: %v", want.Subdomain, err)
			} else {
				return id, ""
			}
		}
		return "", ""
	}

	if want.Tunnel && s.cf.TunnelEnabled() {
		if old.Tunnel {
			// Already tunneled — re-route only if subdomain or target changed.
			if want.Subdomain != old.Subdomain || want.Target != old.Target {
				id, err := s.cf.ReplaceTunnelRoute(ctx, oldHostname, newHostname, want.Target, old.DNSRecordID)
				if err != nil {
					log.Printf("web: replace tunnel route %s→%s: %v", old.Subdomain, want.Subdomain, err)
					return old.DNSRecordID, old.TunnelRouteID
				}
				return id, newHostname
			}
			return old.DNSRecordID, old.TunnelRouteID
		}
		// Switching from A record to tunnel.
		if old.DNSRecordID != "" {
			if err := s.cf.DeleteRecord(ctx, old.DNSRecordID); err != nil {
				log.Printf("web: delete DNS %s: %v", old.Subdomain, err)
			}
		}
		id, err := s.cf.AddTunnelRoute(ctx, newHostname, want.Target)
		if err != nil {
			log.Printf("web: add tunnel route %s: %v", want.Subdomain, err)
			return "", ""
		}
		return id, newHostname
	}

	// Want A record (or tunnel not configured).
	if old.Tunnel {
		// Switching from tunnel to A record.
		if err := s.cf.RemoveTunnelRoute(ctx, old.TunnelRouteID, old.DNSRecordID); err != nil {
			log.Printf("web: remove tunnel route %s: %v", old.Subdomain, err)
		}
		if s.cfg.ServerIP != "" {
			id, err := s.cf.CreateRecord(ctx, newHostname, s.cfg.ServerIP)
			if err != nil {
				log.Printf("web: create DNS %s: %v", want.Subdomain, err)
			} else {
				return id, ""
			}
		}
		return "", ""
	}
	if want.Subdomain != old.Subdomain {
		// Subdomain changed — swap A records.
		if old.DNSRecordID != "" {
			if err := s.cf.DeleteRecord(ctx, old.DNSRecordID); err != nil {
				log.Printf("web: delete DNS %s: %v", old.Subdomain, err)
			}
		}
		if s.cfg.ServerIP != "" {
			id, err := s.cf.CreateRecord(ctx, newHostname, s.cfg.ServerIP)
			if err != nil {
				log.Printf("web: create DNS %s: %v", want.Subdomain, err)
			} else {
				return id, ""
			}
		}
		return "", ""
	}
	// Nothing changed — preserve existing record ID.
	return old.DNSRecordID, ""
}

func (s *Server) updateService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	svc := s.store.GetServiceByID(id)
	if svc == nil {
		errorResponse(w, http.StatusNotFound, "service not found")
		return
	}

	if err := r.ParseForm(); err != nil {
		errorResponse(w, http.StatusBadRequest, "invalid form data")
		return
	}
	var req updateServiceRequest
	req.Name = r.FormValue("name")
	req.Subdomain = r.FormValue("subdomain")
	req.Target = r.FormValue("target")
	req.Category = r.FormValue("category")
	// icon: present in form only when explicitly changed
	if r.FormValue("icon_present") == "1" {
		iconVal := r.FormValue("icon")
		req.Icon = &iconVal
	}
	// tunnel checkbox: only meaningful when the form includes a tunnel_present hidden field
	if r.FormValue("tunnel_present") == "1" {
		v := util.ParseFormBool(r.FormValue("tunnel"))
		req.Tunnel = &v
	}
	// skip_health checkbox: only meaningful when the form includes a skip_health_present hidden field
	if r.FormValue("skip_health_present") == "1" {
		v := util.ParseFormBool(r.FormValue("skip_health"))
		req.SkipHealth = &v
	}
	// direct_only checkbox: only meaningful when the form includes a direct_only_present hidden field
	if r.FormValue("direct_only_present") == "1" {
		v := util.ParseFormBool(r.FormValue("direct_only"))
		req.DirectOnly = &v
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
	directOnly := svc.DirectOnly
	if req.DirectOnly != nil {
		directOnly = *req.DirectOnly
	}
	newTarget := firstNonEmpty(req.Target, svc.Target)

	// Determine the new subdomain key.
	// When direct-only, use a synthetic key so the store map stays unique.
	// When switching off direct-only, the caller must supply a real subdomain.
	var newSub string
	if directOnly {
		// Preserve existing _direct_ key, or generate one if switching to direct.
		if svc.DirectOnly {
			newSub = oldSub // keep existing synthetic key
		} else {
			newSub = "_direct_" + svc.ID
		}
	} else {
		newSub = sanitiseSubdomain(req.Subdomain)
		if newSub == "" {
			newSub = oldSub
		}
	}

	updated := &store.Service{
		ID:            svc.ID,
		Name:          firstNonEmpty(req.Name, svc.Name),
		Subdomain:     newSub,
		Target:        newTarget,
		Icon:          icon,
		Category:      req.Category,
		Order:         svc.Order,
		Source:        svc.Source,
		ContainerID:   svc.ContainerID,
		ContainerName: svc.ContainerName,
		SkipHealth:    skipHealth,
		DirectOnly:    directOnly,
		CreatedAt:     svc.CreatedAt,
	}

	// Reconcile DNS/tunnel state based on old and new configurations.
	wantTunnel := oldTunnelRoute != ""
	if req.Tunnel != nil {
		wantTunnel = *req.Tunnel
	}
	old := dnsState{
		Subdomain:     oldSub,
		Target:        svc.Target,
		DirectOnly:    svc.DirectOnly,
		Tunnel:        oldTunnelRoute != "",
		DNSRecordID:   oldDNSID,
		TunnelRouteID: oldTunnelRoute,
	}
	want := dnsState{
		Subdomain:  newSub,
		Target:     newTarget,
		DirectOnly: directOnly,
		Tunnel:     wantTunnel,
	}
	updated.DNSRecordID, updated.TunnelRouteID = s.reconcileDNS(r.Context(), old, want)

	s.store.UpdateService(id, updated)
	s.save()
	toastTrigger(w, "Service updated", "success", "refreshServicesTable")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	svc := s.store.GetServiceByID(id)
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
	if svc != nil && svc.Source == "docker" && svc.ContainerID != "" {
		s.store.AddDiscovered(&store.DiscoveredService{
			ID:            newID(),
			IP:            "",
			Port:          0,
			Title:         svc.Name,
			Source:        "docker",
			ContainerID:   svc.ContainerID,
			ContainerName: svc.ContainerName,
			DiscoveredAt:  time.Now(),
		})
		hxTrigger(w, "refreshDiscovered", nil)
	}
	s.store.DeleteIcon(id)
	s.save()
	w.WriteHeader(http.StatusOK)
}

func (s *Server) reorderServices(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if err := readJSON(r, &req); err != nil || len(req.IDs) == 0 {
		apiError(w, http.StatusBadRequest, "ids array is required")
		return
	}
	s.store.ReorderServices(req.IDs)
	s.save()
	w.WriteHeader(http.StatusOK)
}

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
	file, _, err := r.FormFile("icon")
	if err != nil {
		apiError(w, http.StatusBadRequest, "icon file required")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 512*1024))
	if err != nil || len(data) == 0 {
		apiError(w, http.StatusBadRequest, "could not read file")
		return
	}
	if err := s.store.WriteIcon(id, data); err != nil {
		apiError(w, http.StatusInternalServerError, "could not save icon")
		return
	}
	s.applyServiceIcon(w, svc, "file")
}

func (s *Server) clearServiceIcon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	svc := s.store.GetServiceByID(id)
	if svc == nil {
		apiError(w, http.StatusNotFound, "service not found")
		return
	}
	s.store.DeleteIcon(id)
	s.applyServiceIcon(w, svc, "")
}

func (s *Server) pullServiceFavicon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	svc := s.store.GetServiceByID(id)
	if svc == nil {
		apiError(w, http.StatusNotFound, "service not found")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data := discovery.FetchFaviconForTarget(ctx, svc.Target)
	if len(data) == 0 {
		errorResponse(w, http.StatusUnprocessableEntity, "no favicon found")
		return
	}
	if err := s.store.WriteIcon(id, data); err != nil {
		errorResponse(w, http.StatusInternalServerError, "could not save favicon")
		return
	}
	s.applyServiceIcon(w, svc, "file")
}

// applyServiceIcon sets a service's Icon field, saves, and renders the icon preview.
func (s *Server) applyServiceIcon(w http.ResponseWriter, svc *store.Service, icon string) {
	updated := *svc
	updated.Icon = icon
	s.store.UpdateService(svc.ID, &updated)
	s.save()
	renderTemplate(w, "icon-preview.html", &updated)
}

// fetchAndWriteFavicon fetches the favicon for target and writes it to the store
// under id. Returns true if data was fetched and written successfully.
// The caller is responsible for updating the entity's Icon field.
func fetchAndWriteFavicon(ctx context.Context, st *store.Store, id, target string) bool {
	data := discovery.FetchFaviconForTarget(ctx, target)
	if len(data) == 0 {
		return false
	}
	return st.WriteIcon(id, data) == nil
}

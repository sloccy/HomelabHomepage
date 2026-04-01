package web

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"

	"lantern/internal/store"
	"lantern/internal/util"
)

func (s *Server) listServices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.store.GetAllServices())
}

type createServiceRequest struct {
	DiscoveredID string // optional: assign from discovered
	Name         string
	Subdomain    string
	Target       string // required if not from discovered
	Category     string
	Tunnel       bool // route via CF tunnel instead of A record
	DirectOnly   bool // no subdomain/DNS; link directly to target
	SkipHealth   bool // skip health check polling
}

func (s *Server) createService(w http.ResponseWriter, r *http.Request) {
	const maxUpload = 5 << 20 // 5 MB
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
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
				defer func() { _ = f.Close() }()
				data, err := io.ReadAll(io.LimitReader(f, 512*1024))
				if err == nil && len(data) > 0 {
					uploadedIconData = data
				}
			}
		}
	}
	req.Subdomain = util.SanitiseSubdomain(req.Subdomain)
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
	source := store.SourceManual
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

	svcID := util.NewID()
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
		icon = store.IconFile // written to disk below after svcID is set
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
		if discoveredIcon == store.IconFile {
			if data, err := s.store.ReadIcon(req.DiscoveredID); err == nil {
				_ = s.store.WriteIcon(svcID, data)
			}
		}
		s.store.RemoveDiscovered(req.DiscoveredID)
	}
	s.save()

	// Asynchronously fetch favicon if no icon is set yet.
	if svc.Icon == "" {
		go func(id string) { //nolint:gosec // intentional background task; request context would be cancelled before fetch completes
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if !util.FetchAndWriteFavicon(ctx, s.store, id) {
				return
			}
			if existing := s.store.GetServiceByID(id); existing != nil {
				updated := *existing
				updated.Icon = store.IconFile
				s.store.UpdateService(id, &updated)
				s.save()
			}
		}(svc.ID)
	}

	toastTrigger(w, "Service added", "refreshServicesTable", "refreshDiscovered")
	w.WriteHeader(http.StatusNoContent)
}

type updateServiceRequest struct {
	Name       string
	Subdomain  string
	Target     string
	Category   string
	Icon       *string // nil = keep existing; "" = clear; non-empty = set
	Tunnel     *bool   // nil = keep existing; true/false = enable/disable
	SkipHealth *bool   // nil = keep existing; true/false = skip health check
	DirectOnly *bool   // nil = keep existing; true/false = direct link only
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

type dnsMode int

const (
	modeDirect  dnsMode = iota // DirectOnly=true; no DNS record
	modeARecord                // A record pointing to ServerIP
	modeTunnel                 // CNAME routed via Cloudflare tunnel
)

func (d dnsState) mode() dnsMode {
	switch {
	case d.DirectOnly:
		return modeDirect
	case d.Tunnel:
		return modeTunnel
	default:
		return modeARecord
	}
}

type dnsAction int

const (
	dnsNoop          dnsAction = iota
	dnsCreateA                 // create a new A record
	dnsDeleteAll               // remove existing DNS record or tunnel route
	dnsSwapA                   // delete old A record, create new one
	dnsCreateTunnel            // add a new tunnel route
	dnsAToTunnel               // delete A record then add tunnel route
	dnsTunnelToA               // remove tunnel route then create A record
	dnsReplaceTunnel           // replace tunnel route (subdomain or target changed)
)

// planDNS is a pure function that maps old and new DNS states to the action needed.
func planDNS(old, want dnsState) dnsAction {
	switch old.mode() {
	case modeDirect:
		switch want.mode() {
		case modeDirect:
			return dnsNoop
		case modeARecord:
			return dnsCreateA
		case modeTunnel:
			return dnsCreateTunnel
		}
	case modeARecord:
		switch want.mode() {
		case modeDirect:
			return dnsDeleteAll
		case modeARecord:
			if want.Subdomain != old.Subdomain {
				return dnsSwapA
			}
			return dnsNoop
		case modeTunnel:
			return dnsAToTunnel
		}
	case modeTunnel:
		switch want.mode() {
		case modeDirect:
			return dnsDeleteAll
		case modeARecord:
			return dnsTunnelToA
		case modeTunnel:
			if want.Subdomain != old.Subdomain || want.Target != old.Target {
				return dnsReplaceTunnel
			}
			return dnsNoop
		}
	}
	return dnsNoop
}

// reconcileDNS applies the necessary Cloudflare API calls to transition from old to want.
// It returns the resulting DNSRecordID and TunnelRouteID to store on the service.
func (s *Server) reconcileDNS(ctx context.Context, old, want dnsState) (dnsID, tunnelRouteID string) {
	oldHostname := old.Subdomain + "." + s.cfg.Domain
	newHostname := want.Subdomain + "." + s.cfg.Domain

	switch planDNS(old, want) {
	case dnsNoop:
		return old.DNSRecordID, old.TunnelRouteID

	case dnsCreateA:
		if s.cfg.ServerIP != "" {
			id, err := s.cf.CreateRecord(ctx, newHostname, s.cfg.ServerIP)
			if err != nil {
				log.Printf("web: create DNS %s: %v", want.Subdomain, err)
			} else {
				return id, ""
			}
		}

	case dnsDeleteAll:
		if old.TunnelRouteID != "" {
			if err := s.cf.RemoveTunnelRoute(ctx, old.TunnelRouteID, old.DNSRecordID); err != nil {
				log.Printf("web: remove tunnel route %s: %v", old.Subdomain, err)
			}
		} else if old.DNSRecordID != "" {
			if err := s.cf.DeleteRecord(ctx, old.DNSRecordID); err != nil {
				log.Printf("web: delete DNS %s: %v", old.Subdomain, err)
			}
		}

	case dnsSwapA:
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

	case dnsCreateTunnel:
		if s.cf.TunnelEnabled() {
			id, err := s.cf.AddTunnelRoute(ctx, newHostname, want.Target)
			if err != nil {
				log.Printf("web: add tunnel route %s: %v", want.Subdomain, err)
			} else {
				return id, newHostname
			}
		}

	case dnsAToTunnel:
		if s.cf.TunnelEnabled() {
			if old.DNSRecordID != "" {
				if err := s.cf.DeleteRecord(ctx, old.DNSRecordID); err != nil {
					log.Printf("web: delete DNS %s: %v", old.Subdomain, err)
				}
			}
			id, err := s.cf.AddTunnelRoute(ctx, newHostname, want.Target)
			if err != nil {
				log.Printf("web: add tunnel route %s: %v", want.Subdomain, err)
			} else {
				return id, newHostname
			}
		}

	case dnsTunnelToA:
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

	case dnsReplaceTunnel:
		if s.cf.TunnelEnabled() {
			id, err := s.cf.ReplaceTunnelRoute(ctx, oldHostname, newHostname, want.Target, old.DNSRecordID)
			if err != nil {
				log.Printf("web: replace tunnel route %s→%s: %v", old.Subdomain, want.Subdomain, err)
				return old.DNSRecordID, old.TunnelRouteID
			}
			return id, newHostname
		}
	}

	return "", ""
}

func (s *Server) updateService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	svc := s.store.GetServiceByID(id)
	if svc == nil {
		errorResponse(w, http.StatusNotFound, "service not found")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
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
	newTarget := cmp.Or(req.Target, svc.Target)

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
		newSub = util.SanitiseSubdomain(req.Subdomain)
		if newSub == "" {
			newSub = oldSub
		}
	}

	updated := &store.Service{
		ID:            svc.ID,
		Name:          cmp.Or(req.Name, svc.Name),
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
	toastTrigger(w, "Service updated", "refreshServicesTable")
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteService(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	svc := s.store.GetServiceByID(id)
	if svc == nil {
		w.WriteHeader(http.StatusOK)
		return
	}
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
	if svc != nil && svc.Source == store.SourceDocker && svc.ContainerID != "" {
		s.store.AddDiscovered(&store.DiscoveredService{
			ID:            util.NewID(),
			IP:            "",
			Port:          0,
			Title:         svc.Name,
			Source:        store.SourceDocker,
			ContainerID:   svc.ContainerID,
			ContainerName: svc.ContainerName,
			DiscoveredAt:  time.Now(),
		})
		w.Header().Set("HX-Trigger", `{"refreshDiscovered":null}`)
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
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, 512*1024))
	if err != nil || len(data) == 0 {
		apiError(w, http.StatusBadRequest, "could not read file")
		return
	}
	if err := s.store.WriteIcon(id, data); err != nil {
		apiError(w, http.StatusInternalServerError, "could not save icon")
		return
	}
	s.applyServiceIcon(w, svc, store.IconFile)
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
	data := util.FetchFaviconForTarget(ctx, svc.Target)
	if len(data) == 0 {
		errorResponse(w, http.StatusUnprocessableEntity, "no favicon found")
		return
	}
	if err := s.store.WriteIcon(id, data); err != nil {
		errorResponse(w, http.StatusInternalServerError, "could not save favicon")
		return
	}
	s.applyServiceIcon(w, svc, store.IconFile)
}

// applyServiceIcon sets a service's Icon field, saves, and renders the icon preview.
func (s *Server) applyServiceIcon(w http.ResponseWriter, svc *store.Service, icon string) {
	updated := *svc
	updated.Icon = icon
	s.store.UpdateService(svc.ID, &updated)
	s.save()
	renderTemplate(w, "icon-preview.html", &updated)
}

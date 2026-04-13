package web

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	"lantern/internal/cf"
	"lantern/internal/config"
	"lantern/internal/store"
	"lantern/internal/tunnel"
	"lantern/internal/util"
)

// Scanner is the subset of discovery.Discoverer the web server needs.
type Scanner interface {
	ScanNow(ctx context.Context)
	Status() (scanning bool, last, next time.Time)
	ScanLog() []string
}

// Server serves the web GUI and REST API.
type Server struct {
	cfg          *config.Config
	store        *store.Store
	cf           *cf.Client
	scanner      Scanner
	tunnel       *tunnel.Manager
	mux          *http.ServeMux
	handler      http.Handler // composed middleware chain, built once in New
	version      string
	healthMu     sync.RWMutex
	health       map[string]string // service ID → "up" | "down"
	healthSem    chan struct{}     // limits concurrent health-check goroutines; allocated once
	faviconCache *ttlCache[faviconEntry]
}

func New(cfg *config.Config, st *store.Store, cfClient *cf.Client, version string) *Server {
	s := &Server{
		cfg: cfg, store: st, cf: cfClient, version: version,
		healthSem:    make(chan struct{}, healthConcurrency),
		faviconCache: newTTLCache[faviconEntry](100),
	}
	s.mux = http.NewServeMux()
	s.routes()
	s.handler = gzipHandler(securityHeaders(s.mux))
	return s
}

func (s *Server) SetScanner(sc Scanner)              { s.scanner = sc }
func (s *Server) SetTunnelManager(t *tunnel.Manager) { s.tunnel = t }
func (s *Server) Stop()                              {} // satisfies the lifecycle interface; no cleanup needed

// WarmFavicons pre-populates the favicon cache for all services and bookmarks
// that use auto-detected favicons (no custom icon set). It checks disk first
// and falls back to network fetches with bounded parallelism. Run as a
// background goroutine — does not block startup.
func (s *Server) WarmFavicons(ctx context.Context) {
	type entry struct {
		id     string
		target string
	}
	var entries []entry
	for _, svc := range s.store.GetAllServices() {
		if svc.Icon == "" && svc.Target != "" {
			entries = append(entries, entry{svc.ID, svc.Target})
		}
	}
	for _, bm := range s.store.GetAllBookmarks() {
		if bm.Icon == "" && bm.URL != "" {
			entries = append(entries, entry{bm.ID, bm.URL})
		}
	}

	const concurrency = 10
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(concurrency)
	for _, e := range entries {
		if gctx.Err() != nil {
			break
		}
		// Skip if already cached in memory.
		if _, ok := s.faviconCache.Get(e.id); ok {
			continue
		}
		// Check disk cache — populated by a prior successful fetch.
		if data, err := s.store.ReadIcon(e.id); err == nil && len(data) > 0 {
			ct := detectIconContentType(data)
			s.faviconCache.Set(e.id, faviconEntry{data: data, contentType: ct}, time.Hour)
			continue
		}
		id, target := e.id, e.target
		g.Go(func() error {
			fetchCtx, cancel := context.WithTimeout(gctx, 5*time.Second)
			defer cancel()
			data := util.FetchFaviconForTarget(fetchCtx, target)
			if len(data) == 0 {
				return nil
			}
			ct := detectIconContentType(data)
			s.faviconCache.Set(id, faviconEntry{data: data, contentType: ct}, time.Hour)
			_ = s.store.WriteIcon(id, data)
			return nil
		})
	}
	_ = g.Wait()
}

// save persists the store to disk, logging any error.
func (s *Server) save() { s.store.SaveLog("web") }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.handler.ServeHTTP(w, r)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

// gzipWriterPool reuses gzip.Writer instances across requests to avoid the
// per-request allocation of ~256 KB of internal gzip state.
var gzipWriterPool = sync.Pool{
	New: func() any {
		gz, _ := gzip.NewWriterLevel(io.Discard, gzip.DefaultCompression)
		return gz
	},
}

// gzipHandler compresses dynamic responses when the client accepts gzip.
// Static assets are already pre-compressed and served with an explicit
// Content-Encoding header, so they pass through without double-compression.
func gzipHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsEncoding(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gz, ok := gzipWriterPool.Get().(*gzip.Writer)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		gz.Reset(w)
		grw := &gzipResponseWriter{ResponseWriter: w, gz: gz}
		defer func() {
			if grw.active {
				_ = gz.Close()
			}
			gzipWriterPool.Put(gz)
		}()
		next.ServeHTTP(grw, r)
	})
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	active  bool // true once we've committed to gzip-compressing this response
	decided bool // true after first Write or WriteHeader determines compression mode
}

// sniff checks whether to use gzip for this response (only runs once).
func (g *gzipResponseWriter) sniff() {
	if g.decided {
		return
	}
	g.decided = true
	// If the inner handler already set Content-Encoding (e.g. pre-compressed
	// static assets), skip gzip to avoid double-compression.
	if g.ResponseWriter.Header().Get("Content-Encoding") != "" {
		return
	}
	g.active = true
	g.ResponseWriter.Header().Set("Content-Encoding", "gzip")
	g.ResponseWriter.Header().Set("Vary", "Accept-Encoding")
	g.ResponseWriter.Header().Del("Content-Length")
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	g.sniff()
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	g.sniff()
	if !g.active {
		return g.ResponseWriter.Write(b)
	}
	return g.gz.Write(b)
}

func (s *Server) routes() {
	// Static files — served from pre-compressed in-memory map.
	serveStaticFiles(s.mux)

	// Page templates (more specific than the file server catch-all).
	s.mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		renderPage(w, indexTmpl, pageData{
			Version:       s.version,
			ServicesHTML:  preRender("services-grid.html", buildServicesGrid(s.store.GetAllServices(), s.cfg.Domain, s.healthSnapshot(), false)),
			BookmarksHTML: preRender("bookmarks-grid.html", buildBookmarksGrid(s.store.GetAllBookmarks())),
		})
	})
	s.mux.HandleFunc("GET /manage", func(w http.ResponseWriter, r *http.Request) {
		renderPage(w, manageTmpl, pageData{Version: s.version})
	})

	// Fragment endpoints — return HTML for htmx.
	s.mux.HandleFunc("GET /fragments/services", s.fragServicesGrid)
	s.mux.HandleFunc("GET /fragments/bookmarks", s.fragBookmarksGrid)
	s.mux.HandleFunc("GET /fragments/sysinfo", s.fragSysinfo)
	s.mux.HandleFunc("GET /fragments/status", s.fragStatus)
	s.mux.HandleFunc("GET /fragments/tunnel", s.fragTunnel)
	s.mux.HandleFunc("GET /fragments/subnets", s.fragSubnets)
	s.mux.HandleFunc("GET /fragments/services-table", s.fragServicesTable)
	s.mux.HandleFunc("GET /fragments/service-form", s.fragServiceFormAdd)
	s.mux.HandleFunc("GET /fragments/service-form/{id}", s.fragServiceFormEdit)
	s.mux.HandleFunc("GET /fragments/discovered", s.fragDiscovered)
	s.mux.HandleFunc("GET /fragments/ignored", s.fragIgnored)
	s.mux.HandleFunc("GET /fragments/assign-form/{id}", s.fragAssignForm)
	s.mux.HandleFunc("GET /fragments/bookmarks-table", s.fragBookmarksTable)
	s.mux.HandleFunc("GET /fragments/bookmark-form", s.fragBookmarkFormAdd)
	s.mux.HandleFunc("GET /fragments/bookmark-form/{id}", s.fragBookmarkFormEdit)
	s.mux.HandleFunc("GET /fragments/ddns", s.fragDDNS)

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

	s.mux.HandleFunc("GET /api/favicon/{id}", s.getFavicon)
	s.mux.HandleFunc("POST /api/services/reorder", s.reorderHandler(s.store.ReorderServices))
	s.mux.HandleFunc("GET /api/icons/{id}", s.getIcon)
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
	s.mux.HandleFunc("POST /api/bookmarks/reorder", s.reorderHandler(s.store.ReorderBookmarks))

	s.mux.HandleFunc("GET /api/tunnel", s.getTunnel)
	s.mux.HandleFunc("POST /api/tunnel", s.createTunnel)
	s.mux.HandleFunc("DELETE /api/tunnel", s.deleteTunnel)
}

// ---- Helpers ----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func readJSON(r *http.Request, v any) error {
	return json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v)
}

func apiError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *Server) reorderHandler(reorderFn func([]string)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			IDs []string `json:"ids"`
		}
		if err := readJSON(r, &req); err != nil || len(req.IDs) == 0 {
			apiError(w, http.StatusBadRequest, "ids array is required")
			return
		}
		reorderFn(req.IDs)
		s.save()
		w.WriteHeader(http.StatusOK)
	}
}

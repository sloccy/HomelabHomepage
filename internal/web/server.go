package web

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"lantern/internal/cf"
	"lantern/internal/config"
	"lantern/internal/store"
	"lantern/internal/tunnel"
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
	faviconCache *ttlCache[faviconEntry]
}

func New(cfg *config.Config, st *store.Store, cfClient *cf.Client, version string) *Server {
	s := &Server{
		cfg: cfg, store: st, cf: cfClient, version: version,
		faviconCache: newTTLCache[faviconEntry](500),
	}
	s.mux = http.NewServeMux()
	s.routes()
	s.handler = gzipHandler(securityHeaders(s.mux))
	return s
}

func (s *Server) SetScanner(sc Scanner)              { s.scanner = sc }
func (s *Server) SetTunnelManager(t *tunnel.Manager) { s.tunnel = t }
func (s *Server) Stop()                              {}

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

// gzipHandler compresses dynamic responses when the client accepts gzip.
// Static assets are already pre-compressed and served with an explicit
// Content-Encoding header, so they pass through without double-compression.
func gzipHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsEncoding(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gz, err := gzip.NewWriterLevel(w, gzip.DefaultCompression)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		grw := &gzipResponseWriter{ResponseWriter: w, gz: gz}
		defer func() {
			if grw.active {
				_ = gz.Close() //nolint:errcheck // best-effort flush on response end
			}
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
	s.mux.HandleFunc("POST /api/services/{id}/move", s.moveService)

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
	s.mux.HandleFunc("POST /api/services/reorder", s.reorderServices)
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
	s.mux.HandleFunc("POST /api/bookmarks/{id}/move", s.moveBookmark)

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

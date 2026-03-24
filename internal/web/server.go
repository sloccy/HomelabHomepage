package web

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"
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
	cfg            *config.Config
	store          *store.Store
	cf             *cf.Client
	scanner        Scanner
	tunnel         *tunnel.Manager
	mux            *http.ServeMux
	version        string
	healthMu       sync.RWMutex
	health         map[string]string // service ID → "up" | "down"
	faviconCache   map[string]*faviconEntry
	faviconCacheMu sync.RWMutex
}

func New(cfg *config.Config, st *store.Store, cfClient *cf.Client, version string) *Server {
	s := &Server{cfg: cfg, store: st, cf: cfClient, version: version, faviconCache: make(map[string]*faviconEntry)}
	s.mux = http.NewServeMux()
	s.routes()
	return s
}

func (s *Server) SetScanner(sc Scanner)              { s.scanner = sc }
func (s *Server) SetTunnelManager(t *tunnel.Manager) { s.tunnel = t }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	gzipHandler(securityHeaders(s.mux)).ServeHTTP(w, r)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		next.ServeHTTP(w, r)
	})
}

var gzPool = sync.Pool{
	New: func() any {
		gz, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return gz
	},
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz          *gzip.Writer
	code        int
	wroteHeader bool
}

// WriteHeader buffers the status code; headers are committed in Write so that
// no-body responses (204, 304) never get a spurious Content-Encoding: gzip.
func (w *gzipResponseWriter) WriteHeader(code int) {
	w.code = code
}

func (w *gzipResponseWriter) writeHeader() {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	// Skip compression if the handler already set Content-Encoding (e.g., pre-compressed static assets).
	if w.Header().Get("Content-Encoding") == "" {
		ct := strings.TrimSpace(strings.ToLower(strings.SplitN(w.Header().Get("Content-Type"), ";", 2)[0]))
		if strings.HasPrefix(ct, "text/") || ct == "application/javascript" || ct == "application/json" || ct == "application/xml" || ct == "image/svg+xml" {
			w.Header().Del("Content-Length")
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Add("Vary", "Accept-Encoding")
			gz := gzPool.Get().(*gzip.Writer)
			gz.Reset(w.ResponseWriter)
			w.gz = gz
		}
	}
	if w.code != 0 {
		w.ResponseWriter.WriteHeader(w.code)
	}
}

func (w *gzipResponseWriter) Write(b []byte) (int, error) {
	w.writeHeader()
	if w.gz != nil {
		return w.gz.Write(b)
	}
	return w.ResponseWriter.Write(b)
}

func (w *gzipResponseWriter) Flush() {
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func acceptsGzip(header string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]), "gzip") {
			return true
		}
	}
	return false
}

func gzipHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !acceptsGzip(r.Header.Get("Accept-Encoding")) {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w}
		defer func() {
			if !gw.wroteHeader && gw.code != 0 {
				gw.ResponseWriter.WriteHeader(gw.code)
			}
			if gw.gz != nil {
				_ = gw.gz.Close()
				gzPool.Put(gw.gz)
			}
		}()
		next.ServeHTTP(gw, r)
	})
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

	s.mux.HandleFunc("GET /api/favicon", s.getFavicon)
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

	s.mux.HandleFunc("GET /api/settings", s.getSettings)
	s.mux.HandleFunc("PUT /api/settings", s.updateSettings)

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
	return json.NewDecoder(r.Body).Decode(v)
}

func apiError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

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

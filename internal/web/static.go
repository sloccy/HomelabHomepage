package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"lantern/internal/util"
)

// acceptsEncoding reports whether the Accept-Encoding header includes the given encoding token.
func acceptsEncoding(header, enc string) bool {
	for _, part := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(strings.SplitN(part, ";", 2)[0]), enc) {
			return true
		}
	}
	return false
}

func acceptsGzip(header string) bool { return acceptsEncoding(header, "gzip") }

//go:embed static
var staticFiles embed.FS

// staticAsset holds the pre-compressed bytes for a static file.
type staticAsset struct {
	plain []byte // raw bytes
	gzip  []byte // gzip-compressed; nil for binary formats
	ct    string // Content-Type
}

type faviconEntry struct {
	data        []byte // nil means negative cache (fetch failed)
	contentType string
}

var (
	staticAssetMap  map[string]*staticAsset
	staticAssetOnce sync.Once
)

func getStaticAssets() map[string]*staticAsset {
	staticAssetOnce.Do(func() {
		assets := make(map[string]*staticAsset)
		_ = fs.WalkDir(staticFiles, "static", func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return err
			}
			data, err := staticFiles.ReadFile(path)
			if err != nil {
				log.Printf("web: read static asset %s: %v", path, err)
				return nil
			}
			urlPath := "/" + strings.TrimPrefix(path, "static/")
			var ct string
			switch {
			case strings.HasSuffix(path, ".css"):
				ct = "text/css; charset=utf-8"
			case strings.HasSuffix(path, ".js"):
				ct = "application/javascript; charset=utf-8"
			case strings.HasSuffix(path, ".png"):
				ct = "image/png"
			case strings.HasSuffix(path, ".webp"):
				ct = "image/webp"
			default:
				ct = "application/octet-stream"
			}
			a := &staticAsset{ct: ct, plain: data}
			// Pre-compress text assets; images are already binary-compressed.
			if strings.HasPrefix(ct, "text/") || strings.Contains(ct, "javascript") {
				var buf bytes.Buffer
				gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
				_, _ = gz.Write(data)
				_ = gz.Close()
				a.gzip = buf.Bytes()
			}
			assets[urlPath] = a
			return nil
		})
		staticAssetMap = assets
	})
	return staticAssetMap
}

func serveStaticFiles(mux *http.ServeMux) {
	assets := getStaticAssets()
	mux.Handle("GET /", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a, ok := assets[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("Content-Type", a.ct)
		ae := r.Header.Get("Accept-Encoding")
		switch {
		case a.gzip != nil && acceptsGzip(ae):
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Vary", "Accept-Encoding")
			w.Header().Set("Content-Length", strconv.Itoa(len(a.gzip)))
			_, _ = w.Write(a.gzip)
		default:
			w.Header().Set("Content-Length", strconv.Itoa(len(a.plain)))
			_, _ = w.Write(a.plain)
		}
	}))
}

// detectIconContentType returns the MIME type of icon data.
// Go's http.DetectContentType does not recognise SVG, so we check for that
// explicitly before falling back to the standard sniffer. Non-image types are
// mapped to application/octet-stream to prevent stored XSS via uploaded icons.
func detectIconContentType(data []byte) string {
	trimmed := bytes.TrimSpace(data)
	if bytes.HasPrefix(trimmed, []byte("<svg")) {
		return "image/svg+xml"
	}
	if bytes.HasPrefix(trimmed, []byte("<?xml")) && bytes.Contains(trimmed[:min(512, len(trimmed))], []byte("<svg")) {
		return "image/svg+xml"
	}
	ct := http.DetectContentType(data)
	if strings.HasPrefix(ct, "image/") {
		return ct
	}
	return "application/octet-stream"
}

// getFavicon proxies a favicon for a dashboard entity (service or bookmark),
// avoiding mixed-content and CORS issues in the browser. The entity ID is used
// to look up the target URL from the store — the URL is never taken directly
// from the request, so it is always server-controlled. Results are cached
// server-side for 1 hour (positive) or 15 minutes (negative).
func (s *Server) getFavicon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isValidIconID(id) {
		http.NotFound(w, r)
		return
	}

	if item := s.faviconCache.Get(id); item != nil {
		e := item.Value()
		if e.data == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", e.contentType)
		w.Header().Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(e.data)
		return
	}

	target := s.store.GetTarget(id)
	if target == "" {
		s.faviconCache.Set(id, &faviconEntry{}, 15*time.Minute)
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	data := util.FetchFaviconForTarget(ctx, target)

	if len(data) == 0 {
		s.faviconCache.Set(id, &faviconEntry{}, 15*time.Minute)
		http.NotFound(w, r)
		return
	}
	ct := detectIconContentType(data)
	s.faviconCache.Set(id, &faviconEntry{data: data, contentType: ct}, time.Hour)

	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data) //nolint:gosec // binary icon data, not HTML; Content-Type is set explicitly
}

// isValidIconID reports whether id is a safe icon filename (hex chars only).
// NewID generates 16-char hex strings; anything else is rejected.
func isValidIconID(id string) bool {
	if id == "" || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

func (s *Server) getIcon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !isValidIconID(id) {
		http.NotFound(w, r)
		return
	}
	data, err := s.store.ReadIcon(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	hash := sha256.Sum256(data)
	etag := `"` + hex.EncodeToString(hash[:16]) + `"`
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	ct := detectIconContentType(data)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=3600, must-revalidate")
	w.Header().Set("ETag", etag)
	_, _ = w.Write(data) //nolint:gosec // binary icon data, not HTML; Content-Type is set explicitly
}

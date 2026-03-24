package web

import (
	"bytes"
	"compress/gzip"
	"context"
	"embed"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"lantern/internal/discovery"
)

//go:embed static
var staticFiles embed.FS

// staticAsset holds the pre-compressed bytes for a static file.
type staticAsset struct {
	plain      []byte // raw bytes
	compressed []byte // gzip BestCompression of plain; nil for binary formats
	ct         string // Content-Type
}

type faviconEntry struct {
	data        []byte
	contentType string
	fetchedAt   time.Time
	negative    bool // true if the fetch failed; cached to avoid repeated retries
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
			a := &staticAsset{plain: data, ct: ct}
			// Pre-compress text assets at BestCompression; images are already binary-compressed.
			if strings.HasPrefix(ct, "text/") || strings.Contains(ct, "javascript") {
				var buf bytes.Buffer
				gz, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
				_, _ = gz.Write(data)
				_ = gz.Close()
				a.compressed = buf.Bytes()
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
		if a.compressed != nil && acceptsGzip(r.Header.Get("Accept-Encoding")) {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Vary", "Accept-Encoding")
			w.Header().Set("Content-Length", strconv.Itoa(len(a.compressed)))
			_, _ = w.Write(a.compressed)
		} else {
			w.Header().Set("Content-Length", strconv.Itoa(len(a.plain)))
			_, _ = w.Write(a.plain)
		}
	}))
}

// getFavicon proxies a favicon from an internal service target, avoiding
// mixed-content and CORS issues in the browser. Results are cached server-side
// for 1 hour (positive) or 15 minutes (negative) to avoid repeated fetches.
// It parses the target page HTML to find the correct favicon URL, matching the
// behaviour of discovery.FetchFaviconForTarget.
func (s *Server) getFavicon(w http.ResponseWriter, r *http.Request) {
	rawURL := r.URL.Query().Get("url")
	if rawURL == "" {
		http.NotFound(w, r)
		return
	}
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		http.NotFound(w, r)
		return
	}
	cacheKey := u.Host

	s.faviconCacheMu.RLock()
	entry, ok := s.faviconCache[cacheKey]
	s.faviconCacheMu.RUnlock()
	if ok {
		ttl := time.Hour
		if entry.negative {
			ttl = 15 * time.Minute
		}
		if time.Since(entry.fetchedAt) < ttl {
			if entry.negative {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", entry.contentType)
			w.Header().Set("Cache-Control", "public, max-age=3600")
			_, _ = w.Write(entry.data)
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	data := discovery.FetchFaviconForTarget(ctx, rawURL)

	s.faviconCacheMu.Lock()
	s.evictFaviconCache()
	if len(data) == 0 {
		s.faviconCache[cacheKey] = &faviconEntry{negative: true, fetchedAt: time.Now()}
		s.faviconCacheMu.Unlock()
		http.NotFound(w, r)
		return
	}
	ct := http.DetectContentType(data)
	s.faviconCache[cacheKey] = &faviconEntry{data: data, contentType: ct, fetchedAt: time.Now()}
	s.faviconCacheMu.Unlock()

	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(data)
}

// evictFaviconCache removes stale entries and, if still at capacity, evicts
// the oldest entry. Must be called with faviconCacheMu held for writing.
func (s *Server) evictFaviconCache() {
	now := time.Now()
	for k, e := range s.faviconCache {
		ttl := time.Hour
		if e.negative {
			ttl = 15 * time.Minute
		}
		if now.Sub(e.fetchedAt) >= ttl {
			delete(s.faviconCache, k)
		}
	}
	if len(s.faviconCache) < 500 {
		return
	}
	// Still full — evict the single oldest entry.
	var oldest string
	var oldestTime time.Time
	for k, e := range s.faviconCache {
		if oldest == "" || e.fetchedAt.Before(oldestTime) {
			oldest = k
			oldestTime = e.fetchedAt
		}
	}
	if oldest != "" {
		delete(s.faviconCache, oldest)
	}
}

func (s *Server) getIcon(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	data, err := s.store.ReadIcon(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ct := http.DetectContentType(data)
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = w.Write(data)
}

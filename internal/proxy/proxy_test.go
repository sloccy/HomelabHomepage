package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lantern/internal/config"
	"lantern/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	return st
}

func newTestHandler(t *testing.T, st *store.Store, webHandler http.Handler) *Handler {
	t.Helper()
	cfg := &config.Config{Domain: "example.com"}
	if webHandler == nil {
		webHandler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
	}
	return New(cfg, st, webHandler)
}

func TestServeHTTP_LanternHost(t *testing.T) {
	webCalled := false
	web := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		webCalled = true
		w.WriteHeader(http.StatusOK)
	})
	h := newTestHandler(t, newTestStore(t), web)

	for _, host := range []string{"lantern.example.com", "example.com", ""} {
		webCalled = false
		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
		req.Host = host
		h.ServeHTTP(rec, req)
		if !webCalled {
			t.Errorf("host %q: expected webHandler to be called", host)
		}
	}
}

func TestServeHTTP_UnknownHost(t *testing.T) {
	h := newTestHandler(t, newTestStore(t), nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	req.Host = "unknown.other.com"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown host: got %d, want 404", rec.Code)
	}
}

func TestServeHTTP_MissingService(t *testing.T) {
	h := newTestHandler(t, newTestStore(t), nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	req.Host = "noservice.example.com"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Errorf("missing service: got %d, want 404", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("missing service: Content-Type = %q, want text/html", ct)
	}
}

func TestServeHTTP_ProxyToBackend(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
	}))
	defer backend.Close()

	st := newTestStore(t)
	st.AddService(&store.Service{
		ID:        "svc1",
		Name:      "Test",
		Subdomain: "test",
		Target:    backend.URL,
	})

	h := newTestHandler(t, st, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	req.Host = "test.example.com"
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusTeapot {
		t.Errorf("proxy: got %d, want 418", rec.Code)
	}
}

func TestServeHTTP_LocationRewrite(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backendHost := r.Host
		http.Redirect(w, r, "http://"+backendHost+"/newpath", http.StatusFound) //nolint:gosec // test handler uses a controlled in-process host, not user input
	}))
	defer backend.Close()

	st := newTestStore(t)
	st.AddService(&store.Service{
		ID:        "svc2",
		Name:      "Redirect",
		Subdomain: "redir",
		Target:    backend.URL,
	})

	h := newTestHandler(t, st, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	req.Host = "redir.example.com"
	h.ServeHTTP(rec, req)

	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "https://redir.example.com") {
		t.Errorf("Location rewrite: got %q, want https://redir.example.com/...", loc)
	}
}

func TestServeHTTP_XForwardedProto(t *testing.T) {
	var gotProto string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotProto = r.Header.Get("X-Forwarded-Proto")
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	st := newTestStore(t)
	st.AddService(&store.Service{
		ID:        "svc3",
		Name:      "Proto",
		Subdomain: "proto",
		Target:    backend.URL,
	})

	h := newTestHandler(t, st, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
	req.Host = "proto.example.com"
	req.Header.Set("X-Forwarded-Proto", "https")
	h.ServeHTTP(rec, req)

	if gotProto != "https" {
		t.Errorf("X-Forwarded-Proto: got %q, want https", gotProto)
	}
}

func TestRealIP(t *testing.T) {
	tests := []struct {
		name       string
		realIP     string
		forwarded  string
		remoteAddr string
		want       string
	}{
		{"X-Real-IP wins", "1.2.3.4", "5.6.7.8", "9.10.11.12:9999", "1.2.3.4"},
		{"X-Forwarded-For first", "", "5.6.7.8, 9.10.11.12", "13.14.15.16:9999", "5.6.7.8"},
		{"RemoteAddr fallback", "", "", "9.10.11.12:9999", "9.10.11.12:9999"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", http.NoBody)
			req.RemoteAddr = tt.remoteAddr
			if tt.realIP != "" {
				req.Header.Set("X-Real-IP", tt.realIP)
			}
			if tt.forwarded != "" {
				req.Header.Set("X-Forwarded-For", tt.forwarded)
			}
			if got := realIP(req); got != tt.want {
				t.Errorf("realIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestBufferPool(t *testing.T) {
	var pool bufferPool
	buf := pool.Get()
	if len(buf) != 32*1024 {
		t.Errorf("Get: len = %d, want %d", len(buf), 32*1024)
	}
	pool.Put(buf)
	buf2 := pool.Get()
	if len(buf2) != 32*1024 {
		t.Errorf("Get after Put: len = %d, want %d", len(buf2), 32*1024)
	}
}

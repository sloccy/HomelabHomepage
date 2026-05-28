package ddns

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"lantern/internal/cf"
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

// ---- getPublicIP -------------------------------------------------------------

func TestGetPublicIP_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "  1.2.3.4  ")
	}))
	defer srv.Close()
	orig := ipifyURL
	ipifyURL = srv.URL
	defer func() { ipifyURL = orig }()

	ip, err := getPublicIP(context.Background())
	if err != nil {
		t.Fatalf("getPublicIP: %v", err)
	}
	if ip != "1.2.3.4" {
		t.Errorf("getPublicIP = %q, want 1.2.3.4", ip)
	}
}

func TestGetPublicIP_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	orig := ipifyURL
	ipifyURL = srv.URL
	defer func() { ipifyURL = orig }()

	// Non-2xx does not cause an error from getPublicIP — it reads (empty) body.
	// The result will be empty string, which the caller handles.
	ip, err := getPublicIP(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip != "" {
		t.Errorf("expected empty ip on 500, got %q", ip)
	}
}

func TestGetPublicIP_Truncates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Write more than 64 bytes — should be truncated.
		_, _ = fmt.Fprint(w, "1.2.3.4")
		for range 100 {
			_, _ = fmt.Fprint(w, "x")
		}
	}))
	defer srv.Close()
	orig := ipifyURL
	ipifyURL = srv.URL
	defer func() { ipifyURL = orig }()

	ip, err := getPublicIP(context.Background())
	if err != nil {
		t.Fatalf("getPublicIP: %v", err)
	}
	if len(ip) > 64 {
		t.Errorf("ip not truncated: len=%d", len(ip))
	}
}

// ---- checkAndUpdate ----------------------------------------------------------

func TestCheckAndUpdate_SetsPublicIP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "5.6.7.8")
	}))
	defer srv.Close()
	orig := ipifyURL
	ipifyURL = srv.URL
	defer func() { ipifyURL = orig }()

	st := newTestStore(t)
	// Add a DDNS domain so checkAndUpdate doesn't early-exit.
	st.AddDDNSDomain("home.example.com")

	cfClient, _ := cf.New("", "", "", "") // noop client
	m := New(&config.Config{}, st, cfClient)
	m.checkAndUpdate(context.Background())

	if got := st.GetPublicIP(); got != "5.6.7.8" {
		t.Errorf("GetPublicIP() = %q, want 5.6.7.8", got)
	}
}

func TestCheckAndUpdate_SkipsWhenIPUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, "5.6.7.8")
	}))
	defer srv.Close()
	orig := ipifyURL
	ipifyURL = srv.URL
	defer func() { ipifyURL = orig }()

	st := newTestStore(t)
	st.AddDDNSDomain("home.example.com")
	st.SetPublicIP("5.6.7.8") // already up to date

	cfClient, _ := cf.New("", "", "", "")
	m := New(&config.Config{}, st, cfClient)

	// Run twice — second run should see same IP and not update the log.
	m.checkAndUpdate(context.Background())
	m.checkAndUpdate(context.Background())

	// IP should still be correct and no error should occur.
	if got := st.GetPublicIP(); got != "5.6.7.8" {
		t.Errorf("GetPublicIP() = %q, want 5.6.7.8", got)
	}
}

func TestCheckAndUpdate_NoDomains(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		_, _ = fmt.Fprint(w, "5.6.7.8")
	}))
	defer srv.Close()
	orig := ipifyURL
	ipifyURL = srv.URL
	defer func() { ipifyURL = orig }()

	st := newTestStore(t) // no DDNS domains
	cfClient, _ := cf.New("", "", "", "")
	m := New(&config.Config{}, st, cfClient)
	m.checkAndUpdate(context.Background())

	if called {
		t.Error("expected ipify not to be called when no DDNS domains registered")
	}
}

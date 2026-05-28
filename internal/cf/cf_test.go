package cf

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// cfServer stands up an httptest.Server and redirects cfBaseURL to it,
// restoring the original on cleanup.
func cfServer(t *testing.T, mux *http.ServeMux) {
	t.Helper()
	srv := httptest.NewServer(mux)
	orig := cfBaseURL
	cfBaseURL = srv.URL
	t.Cleanup(func() {
		srv.Close()
		cfBaseURL = orig
	})
}

func cfOK(result any) []byte {
	env := map[string]any{"success": true, "result": result}
	b, _ := json.Marshal(env)
	return b
}

func cfError(msg string) []byte {
	env := map[string]any{
		"success": false,
		"errors":  []map[string]any{{"message": msg}},
		"result":  nil,
	}
	b, _ := json.Marshal(env)
	return b
}

// ---- New / noop ---------------------------------------------------------------

func TestNew_NoopWhenEmptyToken(t *testing.T) {
	c, err := New("", "", "", "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !c.noop {
		t.Error("expected noop=true for empty token")
	}
}

func TestTunnelEnabled(t *testing.T) {
	tests := []struct {
		name      string
		noop      bool
		tunnelID  string
		accountID string
		want      bool
	}{
		{"noop", true, "tid", "aid", false},
		{"no tunnelID", false, "", "aid", false},
		{"no accountID", false, "tid", "", false},
		{"all set", false, "tid", "aid", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{noop: tt.noop, tunnelID: tt.tunnelID, accountID: tt.accountID}
			if got := c.TunnelEnabled(); got != tt.want {
				t.Errorf("TunnelEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestTunnelAvailable(t *testing.T) {
	tests := []struct {
		name      string
		noop      bool
		accountID string
		want      bool
	}{
		{"noop", true, "aid", false},
		{"no accountID", false, "", false},
		{"available", false, "aid", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := &Client{noop: tt.noop, accountID: tt.accountID}
			if got := c.TunnelAvailable(); got != tt.want {
				t.Errorf("TunnelAvailable() = %v, want %v", got, tt.want)
			}
		})
	}
}

// ---- removeHostname ----------------------------------------------------------

func TestRemoveHostname(t *testing.T) {
	rules := []ingressRule{
		{Hostname: "a.example.com", Service: "http://a"},
		{Hostname: "b.example.com", Service: "http://b"},
		{Hostname: "a.example.com", Service: "http://a2"},
	}
	got := removeHostname(rules, "a.example.com")
	if len(got) != 1 || got[0].Hostname != "b.example.com" {
		t.Errorf("removeHostname: got %+v", got)
	}
}

// ---- FindRecord ---------------------------------------------------------------

func TestFindRecord_Found(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/zones/zone1/dns_records", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(cfOK([]dnsRecord{{ID: "rec1", Content: "1.2.3.4"}}))
	})
	cfServer(t, mux)

	c, _ := New("tok", "zone1", "", "")
	id, ip, err := c.FindRecord(context.Background(), "test.example.com")
	if err != nil {
		t.Fatalf("FindRecord: %v", err)
	}
	if id != "rec1" || ip != "1.2.3.4" {
		t.Errorf("FindRecord = (%q, %q), want (rec1, 1.2.3.4)", id, ip)
	}
}

func TestFindRecord_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/zones/zone1/dns_records", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(cfOK([]dnsRecord{}))
	})
	cfServer(t, mux)

	c, _ := New("tok", "zone1", "", "")
	id, ip, err := c.FindRecord(context.Background(), "notfound.example.com")
	if err != nil {
		t.Fatalf("FindRecord: %v", err)
	}
	if id != "" || ip != "" {
		t.Errorf("FindRecord = (%q, %q), want empty", id, ip)
	}
}

// ---- EnsureARecord -----------------------------------------------------------

func TestEnsureARecord_Creates(t *testing.T) {
	var method string
	mux := http.NewServeMux()
	// FindRecord returns empty (no existing record).
	mux.HandleFunc("/zones/zone1/dns_records", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write(cfOK([]dnsRecord{}))
		} else {
			method = r.Method
			_, _ = w.Write(cfOK(dnsRecord{ID: "new1"}))
		}
	})
	cfServer(t, mux)

	c, _ := New("tok", "zone1", "", "")
	if err := c.EnsureARecord(context.Background(), "test.example.com", "1.2.3.4"); err != nil {
		t.Fatalf("EnsureARecord: %v", err)
	}
	if method != http.MethodPost {
		t.Errorf("expected POST to create record, got %s", method)
	}
}

func TestEnsureARecord_NoOp(t *testing.T) {
	var patchCalled bool
	mux := http.NewServeMux()
	mux.HandleFunc("/zones/zone1/dns_records", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write(cfOK([]dnsRecord{{ID: "rec1", Content: "1.2.3.4"}}))
		} else {
			patchCalled = true
			_, _ = w.Write(cfOK(dnsRecord{ID: "rec1"}))
		}
	})
	cfServer(t, mux)

	c, _ := New("tok", "zone1", "", "")
	if err := c.EnsureARecord(context.Background(), "test.example.com", "1.2.3.4"); err != nil {
		t.Fatalf("EnsureARecord: %v", err)
	}
	if patchCalled {
		t.Error("expected no PATCH when IP unchanged")
	}
}

func TestEnsureARecord_Updates(t *testing.T) {
	var patchCalled bool
	mux := http.NewServeMux()
	mux.HandleFunc("/zones/zone1/dns_records", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = w.Write(cfOK([]dnsRecord{{ID: "rec1", Content: "1.1.1.1"}}))
		}
	})
	mux.HandleFunc("/zones/zone1/dns_records/rec1", func(w http.ResponseWriter, r *http.Request) {
		patchCalled = true
		_, _ = w.Write(cfOK(dnsRecord{ID: "rec1"}))
	})
	cfServer(t, mux)

	c, _ := New("tok", "zone1", "", "")
	if err := c.EnsureARecord(context.Background(), "test.example.com", "2.2.2.2"); err != nil {
		t.Fatalf("EnsureARecord: %v", err)
	}
	if !patchCalled {
		t.Error("expected PATCH when IP changed")
	}
}

// ---- Error envelope ----------------------------------------------------------

func TestAPIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/zones/zone1/dns_records", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(cfError("Zone not found"))
	})
	cfServer(t, mux)

	c, _ := New("tok", "zone1", "", "")
	_, _, err := c.FindRecord(context.Background(), "test.example.com")
	if err == nil {
		t.Fatal("expected error from CF error envelope")
	}
	if got := err.Error(); got == "" {
		t.Error("expected non-empty error message")
	}
}

// ---- Noop operations ---------------------------------------------------------

func TestNoopOperations(t *testing.T) {
	c, _ := New("", "", "", "")
	ctx := context.Background()

	if err := c.EnsureARecord(ctx, "x", "1.2.3.4"); err != nil {
		t.Errorf("noop EnsureARecord: %v", err)
	}
	if _, _, err := c.FindRecord(ctx, "x"); err != nil {
		t.Errorf("noop FindRecord: %v", err)
	}
	if err := c.DeleteRecord(ctx, "id1"); err != nil {
		t.Errorf("noop DeleteRecord: %v", err)
	}
	if err := c.UpdateRecord(ctx, "id1", "1.2.3.4"); err != nil {
		t.Errorf("noop UpdateRecord: %v", err)
	}
	if cnameID, err := c.AddTunnelRoute(ctx, "h", "b"); err != nil || cnameID != "" {
		t.Errorf("noop AddTunnelRoute: id=%q err=%v", cnameID, err)
	}
	if err := c.RemoveTunnelRoute(ctx, "h", "id1"); err != nil {
		t.Errorf("noop RemoveTunnelRoute: %v", err)
	}
}

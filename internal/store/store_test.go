package store

import (
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func randomID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return st
}

// ---- Services ---------------------------------------------------------------

func TestServiceCRUD(t *testing.T) {
	st := newTestStore(t)

	svc := &Service{
		ID:        "svc1",
		Name:      "Plex",
		Subdomain: "plex",
		Target:    "http://10.0.0.1:32400",
		Source:    "manual",
		CreatedAt: time.Now(),
	}
	st.AddService(svc)

	got := st.GetServiceByID("svc1")
	if got == nil || got.Name != "Plex" {
		t.Fatalf("GetServiceByID: got %v", got)
	}
	if st.GetServiceBySubdomain("plex") == nil {
		t.Error("GetServiceBySubdomain: nil")
	}

	updated := *svc
	updated.Name = "Plex Updated"
	st.UpdateService("svc1", &updated)
	if st.GetServiceByID("svc1").Name != "Plex Updated" {
		t.Error("UpdateService: name not updated")
	}

	st.DeleteService("svc1")
	if st.GetServiceByID("svc1") != nil {
		t.Error("DeleteService: still present")
	}
}

func TestServiceContainerIndexes(t *testing.T) {
	st := newTestStore(t)
	svc := &Service{
		ID:            "svc2",
		Name:          "Nginx",
		Subdomain:     "nginx",
		Source:        "docker",
		ContainerID:   "abc123",
		ContainerName: "nginx-container",
		CreatedAt:     time.Now(),
	}
	st.AddService(svc)

	if st.GetServiceByContainerID("abc123") == nil {
		t.Error("GetServiceByContainerID: nil")
	}
	if st.GetServiceByContainerName("nginx-container") == nil {
		t.Error("GetServiceByContainerName: nil")
	}

	st.ClearContainerID("abc123")
	if st.GetServiceByContainerID("abc123") != nil {
		t.Error("ClearContainerID: still indexed")
	}
}

// ---- Bookmarks --------------------------------------------------------------

func TestBookmarkCRUD(t *testing.T) {
	st := newTestStore(t)

	bm := &Bookmark{ID: "bm1", Name: "GitHub", URL: "https://github.com"}
	st.AddBookmark(bm)

	got := st.GetBookmarkByID("bm1")
	if got == nil || got.URL != "https://github.com" {
		t.Fatalf("GetBookmarkByID: got %v", got)
	}
	if len(st.GetAllBookmarks()) != 1 {
		t.Error("GetAllBookmarks: expected 1")
	}

	updated := &Bookmark{ID: "bm1", Name: "GitHub Updated", URL: "https://github.com"}
	if !st.UpdateBookmark("bm1", updated) {
		t.Error("UpdateBookmark: returned false")
	}
	if st.GetBookmarkByID("bm1").Name != "GitHub Updated" {
		t.Error("UpdateBookmark: name not updated")
	}
	// Index should point to the new struct
	if st.bookmarkIdx["bm1"] != updated {
		t.Error("bookmarkIdx: still points to old struct after update")
	}

	if !st.DeleteBookmark("bm1") {
		t.Error("DeleteBookmark: returned false")
	}
	if st.GetBookmarkByID("bm1") != nil {
		t.Error("DeleteBookmark: still present")
	}
	if st.bookmarkIdx["bm1"] != nil {
		t.Error("bookmarkIdx: not cleaned up after delete")
	}
}

func TestReorderBookmarks(t *testing.T) {
	st := newTestStore(t)
	st.AddBookmark(&Bookmark{ID: "a", Name: "A"})
	st.AddBookmark(&Bookmark{ID: "b", Name: "B"})
	st.AddBookmark(&Bookmark{ID: "c", Name: "C"})

	st.ReorderBookmarks([]string{"c", "a", "b"})

	if st.GetBookmarkByID("c").Order != 0 {
		t.Errorf("c.Order = %d, want 0", st.GetBookmarkByID("c").Order)
	}
	if st.GetBookmarkByID("a").Order != 1 {
		t.Errorf("a.Order = %d, want 1", st.GetBookmarkByID("a").Order)
	}
	if st.GetBookmarkByID("b").Order != 2 {
		t.Errorf("b.Order = %d, want 2", st.GetBookmarkByID("b").Order)
	}
}

// ---- Discovered -------------------------------------------------------------

func TestDiscoveredCRUD(t *testing.T) {
	st := newTestStore(t)

	d := &DiscoveredService{ID: "d1", IP: "10.0.0.5", Port: 8080, Source: "network", DiscoveredAt: time.Now()}
	st.AddDiscovered(d)

	if st.GetDiscoveredByID("d1") == nil {
		t.Error("GetDiscoveredByID: nil")
	}

	// Dedup by IP+port
	dup := &DiscoveredService{ID: "d2", IP: "10.0.0.5", Port: 8080, Source: "network", DiscoveredAt: time.Now()}
	st.AddDiscovered(dup)
	if len(st.GetAllDiscovered()) != 1 {
		t.Errorf("dedup failed: got %d discovered", len(st.GetAllDiscovered()))
	}

	st.RemoveDiscovered("d1")
	if st.GetDiscoveredByID("d1") != nil {
		t.Error("RemoveDiscovered: still in index")
	}
	if len(st.GetAllDiscovered()) != 0 {
		t.Error("RemoveDiscovered: still in slice")
	}
}

func TestDiscoveredContainerIndex(t *testing.T) {
	st := newTestStore(t)
	d := &DiscoveredService{ID: "d1", IP: "10.0.0.2", Port: 80, Source: "docker", ContainerID: "cid1", DiscoveredAt: time.Now()}
	st.AddDiscovered(d)

	if st.GetDiscoveredByContainerID("cid1") == nil {
		t.Error("GetDiscoveredByContainerID: nil")
	}

	// Dedup by container ID
	dup := &DiscoveredService{ID: "d2", IP: "10.0.0.3", Port: 81, Source: "docker", ContainerID: "cid1", DiscoveredAt: time.Now()}
	st.AddDiscovered(dup)
	if len(st.GetAllDiscovered()) != 1 {
		t.Errorf("container dedup failed: got %d", len(st.GetAllDiscovered()))
	}

	st.RemoveDiscoveredByContainerID("cid1")
	if st.GetDiscoveredByContainerID("cid1") != nil {
		t.Error("RemoveDiscoveredByContainerID: still in index")
	}
}

func TestClearNetworkDiscovered(t *testing.T) {
	st := newTestStore(t)
	st.AddDiscovered(&DiscoveredService{ID: "n1", IP: "10.0.0.1", Port: 80, Source: "network", DiscoveredAt: time.Now()})
	st.AddDiscovered(&DiscoveredService{ID: "d1", IP: "10.0.0.2", Port: 80, Source: "docker", DiscoveredAt: time.Now()})

	st.ClearNetworkDiscovered()

	if st.GetDiscoveredByID("n1") != nil {
		t.Error("ClearNetworkDiscovered: network entry still in index")
	}
	if st.GetDiscoveredByID("d1") == nil {
		t.Error("ClearNetworkDiscovered: docker entry removed unexpectedly")
	}
	if len(st.GetAllDiscovered()) != 1 {
		t.Errorf("ClearNetworkDiscovered: got %d entries, want 1", len(st.GetAllDiscovered()))
	}
}

func TestUpsertNetworkDiscovered(t *testing.T) {
	st := newTestStore(t)
	d := &DiscoveredService{ID: "u1", IP: "10.0.0.3", Port: 9000, Source: "network", Title: "Original", DiscoveredAt: time.Now()}
	id := st.UpsertNetworkDiscovered(d)
	if id != "u1" {
		t.Errorf("UpsertNetworkDiscovered insert: id = %q, want u1", id)
	}

	// Update existing
	updated := &DiscoveredService{ID: "u2", IP: "10.0.0.3", Port: 9000, Source: "network", Title: "Updated", DiscoveredAt: time.Now()}
	id2 := st.UpsertNetworkDiscovered(updated)
	if id2 != "u1" {
		t.Errorf("UpsertNetworkDiscovered update: id = %q, want u1 (preserved)", id2)
	}
	if st.GetDiscoveredByID("u1").Title != "Updated" {
		t.Error("UpsertNetworkDiscovered: title not updated")
	}
	if len(st.GetAllDiscovered()) != 1 {
		t.Errorf("UpsertNetworkDiscovered: got %d entries, want 1", len(st.GetAllDiscovered()))
	}
}

func TestUpdateDiscoveredIcon(t *testing.T) {
	st := newTestStore(t)
	st.AddDiscovered(&DiscoveredService{ID: "ic1", IP: "10.0.0.4", Port: 80, Source: "network", DiscoveredAt: time.Now()})
	st.UpdateDiscoveredIcon("ic1", "file")
	if st.GetDiscoveredByID("ic1").Icon != "file" {
		t.Error("UpdateDiscoveredIcon: not updated")
	}
}

// ---- Ignored ----------------------------------------------------------------

func TestIgnoreAndUnignore(t *testing.T) {
	st := newTestStore(t)
	disc := &DiscoveredService{ID: "ig1", IP: "10.0.0.5", Port: 9090, Source: "network", Title: "Something", DiscoveredAt: time.Now()}
	st.AddDiscovered(disc)

	if err := st.IgnoreDiscovered("ig1"); err != nil {
		t.Fatalf("IgnoreDiscovered: %v", err)
	}

	// Should no longer be in discovered
	if st.GetDiscoveredByID("ig1") != nil {
		t.Error("IgnoreDiscovered: still in discovered index")
	}
	if len(st.GetAllDiscovered()) != 0 {
		t.Error("IgnoreDiscovered: still in discovered slice")
	}

	// Should be ignored
	if !st.IsIgnored("10.0.0.5", 9090) {
		t.Error("IsIgnored: should be true")
	}

	// Unignore
	removed, err := st.UnignoreService("ig1")
	if err != nil {
		t.Fatalf("UnignoreService: %v", err)
	}
	if removed.IP != "10.0.0.5" {
		t.Errorf("UnignoreService: ip = %q, want 10.0.0.5", removed.IP)
	}
	if st.IsIgnored("10.0.0.5", 9090) {
		t.Error("IsIgnored: should be false after unignore")
	}
	if len(st.GetIgnored()) != 0 {
		t.Error("UnignoreService: still in ignored slice")
	}
}

func TestIgnoreDiscoveredNotFound(t *testing.T) {
	st := newTestStore(t)
	if err := st.IgnoreDiscovered("nonexistent"); err == nil {
		t.Error("IgnoreDiscovered: expected error for missing id")
	}
}

// ---- Persistence ------------------------------------------------------------

func TestPersistenceRoundTrip(t *testing.T) {
	dir := t.TempDir()
	st, _ := New(dir)

	st.AddService(&Service{ID: "s1", Name: "Test", Subdomain: "test", Source: "manual", CreatedAt: time.Now()})
	st.AddBookmark(&Bookmark{ID: "b1", Name: "Go", URL: "https://go.dev"})
	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// Reload from disk
	st2, err := New(dir)
	if err != nil {
		t.Fatalf("New (reload): %v", err)
	}
	if st2.GetServiceByID("s1") == nil {
		t.Error("reload: service not found")
	}
	if st2.GetBookmarkByID("b1") == nil {
		t.Error("reload: bookmark not found")
	}
}

func TestFreshStoreIndexesInitialised(t *testing.T) {
	// New store with no data file must have initialised (non-nil) index maps.
	dir := t.TempDir()
	st, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// These would panic if the maps are nil
	st.AddService(&Service{ID: "x", Subdomain: "x", Source: "manual", CreatedAt: time.Now()})
	st.AddBookmark(&Bookmark{ID: "y", Name: "Y"})
	st.AddDiscovered(&DiscoveredService{ID: "z", IP: "1.2.3.4", Port: 80, Source: "network", DiscoveredAt: time.Now()})
}

// ---- Icon storage -----------------------------------------------------------

func TestIconWriteReadDelete(t *testing.T) {
	st := newTestStore(t)
	data := []byte{0x89, 0x50, 0x4E, 0x47} // PNG magic bytes

	if err := st.WriteIcon("ico1", data); err != nil {
		t.Fatalf("WriteIcon: %v", err)
	}
	got, err := st.ReadIcon("ico1")
	if err != nil {
		t.Fatalf("ReadIcon: %v", err)
	}
	if string(got) != string(data) {
		t.Error("ReadIcon: data mismatch")
	}
	st.DeleteIcon("ico1")
	if _, err := os.ReadFile(filepath.Join(st.iconDir, "ico1")); !os.IsNotExist(err) {
		t.Error("DeleteIcon: file still exists")
	}
}

// ---- Concurrent access ------------------------------------------------------

func TestConcurrentAccess(t *testing.T) {
	st := newTestStore(t)
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(_ int) {
			defer wg.Done()
			id := randomID()
			sub := "sub" + id
			st.AddService(&Service{ID: id, Subdomain: sub, Source: "manual", CreatedAt: time.Now()})
			st.GetServiceByID(id)
			st.GetAllServices()
		}(i)
	}
	wg.Wait()
}

// ---- Helpers ----------------------------------------------------------------

func TestFilterSlice(t *testing.T) {
	s := []*Service{{ID: "a"}, {ID: "b"}, {ID: "c"}}
	filterSlice(&s, func(x *Service) bool { return x.ID != "b" })
	if len(s) != 2 || s[0].ID != "a" || s[1].ID != "c" {
		t.Errorf("filterSlice: got %v", s)
	}
}

func TestAddUniqueAndRemoveString(t *testing.T) {
	s := []string{"a", "b"}
	addUnique(&s, "c")
	addUnique(&s, "a") // duplicate, should not be added
	if len(s) != 3 {
		t.Errorf("addUnique: len = %d, want 3", len(s))
	}
	removeString(&s, "b")
	if len(s) != 2 || s[0] != "a" || s[1] != "c" {
		t.Errorf("removeString: got %v", s)
	}
}

func TestIgnoredKey(t *testing.T) {
	k := ignoredKey("10.0.0.1", 8080)
	if k != "10.0.0.1:8080" {
		t.Errorf("ignoredKey = %q, want 10.0.0.1:8080", k)
	}
}

func TestUpdateServiceSubdomainChange(t *testing.T) {
	st := newTestStore(t)
	svc := &Service{ID: "s1", Name: "Old", Subdomain: "old", DNSRecordID: "dns1", Source: "manual", CreatedAt: time.Now()}
	st.AddService(svc)

	updated := *svc
	updated.Subdomain = "new"
	updated.DNSRecordID = "dns2"
	oldSub, oldDNSID := st.UpdateService("s1", &updated)

	if oldSub != "old" {
		t.Errorf("UpdateService: oldSub = %q, want old", oldSub)
	}
	if oldDNSID != "dns1" {
		t.Errorf("UpdateService: oldDNSID = %q, want dns1", oldDNSID)
	}
	if st.GetServiceBySubdomain("old") != nil {
		t.Error("UpdateService: old subdomain still present in map")
	}
	if st.GetServiceBySubdomain("new") == nil {
		t.Error("UpdateService: new subdomain not in map")
	}
	if st.GetServiceByID("s1").Subdomain != "new" {
		t.Error("UpdateService: ID index not updated")
	}
}

func TestDeleteServiceReturnValues(t *testing.T) {
	st := newTestStore(t)
	svc := &Service{
		ID:            "s2",
		Subdomain:     "svc2",
		DNSRecordID:   "rec1",
		TunnelRouteID: "tun1",
		Source:        "manual",
		CreatedAt:     time.Now(),
	}
	st.AddService(svc)

	sub, dnsID, tunnelRoute := st.DeleteService("s2")
	if sub != "svc2" {
		t.Errorf("DeleteService: sub = %q, want svc2", sub)
	}
	if dnsID != "rec1" {
		t.Errorf("DeleteService: dnsID = %q, want rec1", dnsID)
	}
	if tunnelRoute != "tun1" {
		t.Errorf("DeleteService: tunnelRoute = %q, want tun1", tunnelRoute)
	}
}

func TestReorderServices(t *testing.T) {
	st := newTestStore(t)
	st.AddService(&Service{ID: "a", Subdomain: "a", Source: "manual", CreatedAt: time.Now()})
	st.AddService(&Service{ID: "b", Subdomain: "b", Source: "manual", CreatedAt: time.Now()})
	st.AddService(&Service{ID: "c", Subdomain: "c", Source: "manual", CreatedAt: time.Now()})

	st.ReorderServices([]string{"c", "a", "b"})

	if st.GetServiceByID("c").Order != 0 {
		t.Errorf("c.Order = %d, want 0", st.GetServiceByID("c").Order)
	}
	if st.GetServiceByID("a").Order != 1 {
		t.Errorf("a.Order = %d, want 1", st.GetServiceByID("a").Order)
	}
	if st.GetServiceByID("b").Order != 2 {
		t.Errorf("b.Order = %d, want 2", st.GetServiceByID("b").Order)
	}
}

func TestClearContainerIDPreservesNameIndex(t *testing.T) {
	// containerNameIdx is intentionally preserved after ClearContainerID to
	// support docker container reconnect-by-name on restart.
	st := newTestStore(t)
	svc := &Service{
		ID:            "s3",
		Subdomain:     "svc3",
		Source:        "docker",
		ContainerID:   "cid3",
		ContainerName: "my-container",
		CreatedAt:     time.Now(),
	}
	st.AddService(svc)

	st.ClearContainerID("cid3")

	if st.GetServiceByContainerID("cid3") != nil {
		t.Error("ClearContainerID: containerIDIdx not cleared")
	}
	if st.GetServiceByContainerName("my-container") == nil {
		t.Error("ClearContainerID: containerNameIdx cleared unexpectedly (required for reconnect-by-name)")
	}
}

func TestUpsertNetworkDiscoveredUpdatesSuggestedSubdomain(t *testing.T) {
	st := newTestStore(t)
	d := &DiscoveredService{
		ID: "u1", IP: "10.0.0.10", Port: 9000, Source: "network",
		SuggestedSubdomain: "old-name", DiscoveredAt: time.Now(),
	}
	st.UpsertNetworkDiscovered(d)

	updated := &DiscoveredService{
		ID: "u2", IP: "10.0.0.10", Port: 9000, Source: "network",
		SuggestedSubdomain: "new-name", DiscoveredAt: time.Now(),
	}
	st.UpsertNetworkDiscovered(updated)

	got := st.GetDiscoveredByID("u1")
	if got == nil {
		t.Fatal("UpsertNetworkDiscovered: original entry not found")
	}
	if got.SuggestedSubdomain != "new-name" {
		t.Errorf("UpsertNetworkDiscovered: SuggestedSubdomain = %q, want new-name", got.SuggestedSubdomain)
	}
}

func TestPersistenceRoundTripDiscoveredAndIgnored(t *testing.T) {
	dir := t.TempDir()
	st, _ := New(dir)

	st.AddDiscovered(&DiscoveredService{ID: "d1", IP: "10.0.0.1", Port: 80, Source: "network", DiscoveredAt: time.Now()})
	if err := st.IgnoreDiscovered("d1"); err != nil {
		t.Fatalf("IgnoreDiscovered: %v", err)
	}
	st.AddDiscovered(&DiscoveredService{ID: "d2", IP: "10.0.0.2", Port: 443, Source: "network", DiscoveredAt: time.Now()})

	if err := st.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	st2, err := New(dir)
	if err != nil {
		t.Fatalf("New (reload): %v", err)
	}
	if !st2.IsIgnored("10.0.0.1", 80) {
		t.Error("reload: ignored entry not restored")
	}
	if st2.GetDiscoveredByID("d2") == nil {
		t.Error("reload: discovered entry not restored")
	}
	if st2.GetDiscoveredByID("d1") != nil {
		t.Error("reload: ignored entry incorrectly in discovered index")
	}
}

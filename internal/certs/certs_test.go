package certs

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"lantern/internal/config"
)

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	dir := t.TempDir()
	cfg := &config.Config{Domain: "test.example.com", DataDir: dir}
	m, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

func writeSelfSignedPEM(t *testing.T, certFile, keyFile string, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{"*.test.example.com"},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     notAfter,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	keyDER, _ := x509.MarshalECPrivateKey(key)
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("write cert: %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("write key: %v", err)
	}
}

// ---- tryLoadExisting ---------------------------------------------------------

func TestTryLoadExisting_Valid(t *testing.T) {
	m := newTestManager(t)
	writeSelfSignedPEM(t, m.certFile, m.keyFile, time.Now().Add(60*24*time.Hour))

	if !m.tryLoadExisting() {
		t.Error("tryLoadExisting: expected true for valid non-expiring cert")
	}
	if m.cert == nil {
		t.Error("tryLoadExisting: cert not loaded")
	}
}

func TestTryLoadExisting_Expired(t *testing.T) {
	m := newTestManager(t)
	writeSelfSignedPEM(t, m.certFile, m.keyFile, time.Now().Add(10*24*time.Hour)) // < 30 days

	if m.tryLoadExisting() {
		t.Error("tryLoadExisting: expected false for soon-expiring cert")
	}
}

func TestTryLoadExisting_Missing(t *testing.T) {
	m := newTestManager(t)
	if m.tryLoadExisting() {
		t.Error("tryLoadExisting: expected false when cert files absent")
	}
}

func TestTryLoadExisting_Invalid(t *testing.T) {
	m := newTestManager(t)
	_ = os.WriteFile(m.certFile, []byte("not a pem"), 0o600)
	_ = os.WriteFile(m.keyFile, []byte("not a pem"), 0o600)
	if m.tryLoadExisting() {
		t.Error("tryLoadExisting: expected false for corrupt PEM")
	}
}

// ---- useSelfSigned -----------------------------------------------------------

func TestUseSelfSigned(t *testing.T) {
	m := newTestManager(t)
	if err := m.useSelfSigned(); err != nil {
		t.Fatalf("useSelfSigned: %v", err)
	}
	if m.cert == nil {
		t.Fatal("useSelfSigned: cert is nil")
	}
	tlsCert := m.cert
	if len(tlsCert.Certificate) == 0 {
		t.Fatal("useSelfSigned: no DER blocks")
	}
	leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}

	// Check it's ECDSA P-256.
	if _, ok := leaf.PublicKey.(*ecdsa.PublicKey); !ok {
		t.Error("useSelfSigned: expected ECDSA public key")
	}
	pk, ok := tlsCert.PrivateKey.(*ecdsa.PrivateKey)
	if !ok {
		t.Error("useSelfSigned: expected ECDSA private key")
	}
	if pk.Curve != elliptic.P256() {
		t.Error("useSelfSigned: expected P-256 curve")
	}

	// Check SANs contain domain and wildcard.
	wantNames := map[string]bool{
		"*.test.example.com": true,
		"test.example.com":   true,
	}
	for _, name := range leaf.DNSNames {
		delete(wantNames, name)
	}
	if len(wantNames) > 0 {
		t.Errorf("useSelfSigned: missing SANs: %v", wantNames)
	}

	// Check ~90 day validity.
	validity := leaf.NotAfter.Sub(leaf.NotBefore)
	if validity < 89*24*time.Hour || validity > 91*24*time.Hour {
		t.Errorf("useSelfSigned: validity = %v, want ~90d", validity)
	}
}

// ---- GetCertificate ----------------------------------------------------------

func TestGetCertificate_NilWhenEmpty(t *testing.T) {
	m := newTestManager(t)
	_, err := m.GetCertificate(nil)
	if err == nil {
		t.Error("GetCertificate: expected error when no cert loaded")
	}
}

func TestGetCertificate_ReturnsLoaded(t *testing.T) {
	m := newTestManager(t)
	if err := m.useSelfSigned(); err != nil {
		t.Fatalf("useSelfSigned: %v", err)
	}
	cert, err := m.GetCertificate(nil)
	if err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
	if cert == nil {
		t.Error("GetCertificate: expected non-nil cert")
	}
}

// ---- loadOrCreateUser / saveUser --------------------------------------------

func TestLoadOrCreateUser_CreatesKey(t *testing.T) {
	dir := t.TempDir()
	user, err := loadOrCreateUser(dir, "example.com")
	if err != nil {
		t.Fatalf("loadOrCreateUser: %v", err)
	}
	if user.GetEmail() != "admin@example.com" {
		t.Errorf("email = %q, want admin@example.com", user.GetEmail())
	}
	if user.GetPrivateKey() == nil {
		t.Error("expected non-nil private key")
	}
	if _, err := os.Stat(filepath.Join(dir, "account.key")); err != nil {
		t.Error("account.key not created")
	}
}

func TestLoadOrCreateUser_LoadsExistingKey(t *testing.T) {
	dir := t.TempDir()
	user1, err := loadOrCreateUser(dir, "example.com")
	if err != nil {
		t.Fatalf("first load: %v", err)
	}
	user2, err := loadOrCreateUser(dir, "example.com")
	if err != nil {
		t.Fatalf("second load: %v", err)
	}

	k1, ok1 := user1.GetPrivateKey().(*ecdsa.PrivateKey)
	k2, ok2 := user2.GetPrivateKey().(*ecdsa.PrivateKey)
	if !ok1 || !ok2 {
		t.Fatal("expected ECDSA private key")
	}
	if k1.D.Cmp(k2.D) != 0 {
		t.Error("loadOrCreateUser: key changed between loads")
	}
}

func TestSaveAndLoadUser_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	user, _ := loadOrCreateUser(dir, "example.com")

	// Simulate a registration by writing a valid JSON blob directly.
	regJSON := []byte(`{"uri":"https://acme.example.com/acct/1","body":{}}`)
	if err := os.WriteFile(filepath.Join(dir, "account.json"), regJSON, 0o600); err != nil {
		t.Fatalf("write account.json: %v", err)
	}

	user2, err := loadOrCreateUser(dir, "example.com")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if user2.Registration == nil {
		t.Error("registration not loaded from account.json")
	}
	// Key identity should be preserved.
	k1, ok1 := user.GetPrivateKey().(*ecdsa.PrivateKey)
	k2, ok2 := user2.GetPrivateKey().(*ecdsa.PrivateKey)
	if !ok1 || !ok2 {
		t.Fatal("expected ECDSA private key")
	}
	if k1.D.Cmp(k2.D) != 0 {
		t.Error("key changed after round-trip")
	}
}

// ---- GetCertificate concurrent access (race detector) -----------------------

func TestGetCertificate_ConcurrentSafe(t *testing.T) {
	m := newTestManager(t)
	_ = m.useSelfSigned()

	done := make(chan struct{})
	for range 10 {
		go func() {
			_, _ = m.GetCertificate(nil)
			done <- struct{}{}
		}()
	}
	for range 10 {
		<-done
	}
}

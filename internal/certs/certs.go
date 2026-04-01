package certs

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/go-acme/lego/v4/certificate"
	"github.com/go-acme/lego/v4/lego"
	legocf "github.com/go-acme/lego/v4/providers/dns/cloudflare"
	"github.com/go-acme/lego/v4/registration"

	"lantern/internal/config"
)

// Manager handles TLS certificate lifecycle via Let's Encrypt.
type Manager struct {
	cfg      *config.Config
	mu       sync.RWMutex
	cert     *tls.Certificate
	acmeDir  string
	certFile string
	keyFile  string
	resFile  string
}

func New(cfg *config.Config) (*Manager, error) {
	acmeDir := filepath.Join(cfg.DataDir, "acme")
	certDir := filepath.Join(cfg.DataDir, "certs")
	if err := os.MkdirAll(acmeDir, 0o700); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		return nil, err
	}
	return &Manager{
		cfg:      cfg,
		acmeDir:  acmeDir,
		certFile: filepath.Join(certDir, "cert.pem"),
		keyFile:  filepath.Join(certDir, "key.pem"),
		resFile:  filepath.Join(certDir, "resource.json"),
	}, nil
}

// GetCertificate implements tls.Config.GetCertificate.
func (m *Manager) GetCertificate(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if m.cert == nil {
		return nil, fmt.Errorf("no certificate available")
	}
	return m.cert, nil
}

// EnsureCert loads an existing valid cert or obtains a new one from Let's Encrypt.
// Falls back to a self-signed certificate if Let's Encrypt provisioning fails.
func (m *Manager) EnsureCert() error {
	if m.tryLoadExisting() {
		log.Println("certs: loaded existing certificate")
		return nil
	}
	log.Println("certs: obtaining certificate from Let's Encrypt...")
	if err := m.obtain(); err != nil {
		log.Printf("certs: Let's Encrypt failed: %v — using self-signed fallback", err)
		return m.useSelfSigned()
	}
	return nil
}

// RenewLoop checks for cert expiry every 12 hours and renews when < 30 days remain.
func (m *Manager) RenewLoop() {
	ticker := time.NewTicker(12 * time.Hour)
	defer ticker.Stop()
	for range ticker.C {
		m.mu.RLock()
		cert := m.cert
		m.mu.RUnlock()
		if cert == nil {
			_ = m.EnsureCert()
			continue
		}
		leaf := cert.Leaf
		if leaf == nil {
			if parsed, err := x509.ParseCertificate(cert.Certificate[0]); err == nil {
				leaf = parsed
			}
		}
		if leaf != nil && time.Until(leaf.NotAfter) < 30*24*time.Hour {
			log.Printf("certs: certificate expires %s, renewing...", leaf.NotAfter.Format(time.RFC3339))
			if err := m.renew(); err != nil {
				log.Printf("certs: renewal failed: %v", err)
			}
		}
	}
}

func (m *Manager) tryLoadExisting() bool {
	certPEM, err1 := os.ReadFile(m.certFile)
	keyPEM, err2 := os.ReadFile(m.keyFile)
	if err1 != nil || err2 != nil {
		return false
	}
	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return false
	}
	// Parse leaf to check expiry.
	if len(tlsCert.Certificate) > 0 {
		leaf, err := x509.ParseCertificate(tlsCert.Certificate[0])
		if err == nil {
			tlsCert.Leaf = leaf
			if time.Until(leaf.NotAfter) < 30*24*time.Hour {
				return false // needs renewal
			}
		}
	}
	m.mu.Lock()
	m.cert = &tlsCert
	m.mu.Unlock()
	return true
}

func (m *Manager) buildLegoClient() (*lego.Client, error) {
	user, err := loadOrCreateUser(m.acmeDir, m.cfg.Domain)
	if err != nil {
		return nil, fmt.Errorf("acme user: %w", err)
	}

	legoConfig := lego.NewConfig(user)
	legoConfig.CADirURL = lego.LEDirectoryProduction

	client, err := lego.NewClient(legoConfig)
	if err != nil {
		return nil, fmt.Errorf("lego client: %w", err)
	}

	cfConfig := legocf.NewDefaultConfig()
	cfConfig.AuthToken = m.cfg.CFAPIToken
	provider, err := legocf.NewDNSProviderConfig(cfConfig)
	if err != nil {
		return nil, fmt.Errorf("cloudflare provider: %w", err)
	}
	if err := client.Challenge.SetDNS01Provider(provider); err != nil {
		return nil, err
	}

	if user.Registration == nil {
		reg, err := client.Registration.Register(registration.RegisterOptions{TermsOfServiceAgreed: true})
		if err != nil {
			return nil, fmt.Errorf("register: %w", err)
		}
		user.Registration = reg
		if err := saveUser(m.acmeDir, user); err != nil {
			log.Printf("certs: save user: %v", err)
		}
	}
	return client, nil
}

func (m *Manager) obtain() error {
	client, err := m.buildLegoClient()
	if err != nil {
		return err
	}
	domains := []string{"*." + m.cfg.Domain, m.cfg.Domain}
	res, err := client.Certificate.Obtain(certificate.ObtainRequest{
		Domains: domains,
		Bundle:  true,
	})
	if err != nil {
		return fmt.Errorf("obtain cert: %w", err)
	}
	return m.saveCert(res)
}

func (m *Manager) renew() error {
	raw, err := os.ReadFile(m.resFile)
	if err != nil {
		// No saved resource — re-obtain.
		return m.obtain()
	}
	var res certificate.Resource
	if err := json.Unmarshal(raw, &res); err != nil {
		return m.obtain()
	}
	client, err := m.buildLegoClient()
	if err != nil {
		return err
	}
	renewed, err := client.Certificate.RenewWithOptions(res, &certificate.RenewOptions{Bundle: true})
	if err != nil {
		return fmt.Errorf("renew cert: %w", err)
	}
	return m.saveCert(renewed)
}

func (m *Manager) saveCert(res *certificate.Resource) error {
	if err := os.WriteFile(m.certFile, res.Certificate, 0o600); err != nil {
		return err
	}
	if err := os.WriteFile(m.keyFile, res.PrivateKey, 0o600); err != nil {
		return err
	}
	resJSON, _ := json.Marshal(res)
	_ = os.WriteFile(m.resFile, resJSON, 0o600)

	tlsCert, err := tls.X509KeyPair(res.Certificate, res.PrivateKey)
	if err != nil {
		return err
	}
	if len(tlsCert.Certificate) > 0 {
		if leaf, err := x509.ParseCertificate(tlsCert.Certificate[0]); err == nil {
			tlsCert.Leaf = leaf
		}
	}
	m.mu.Lock()
	m.cert = &tlsCert
	m.mu.Unlock()
	log.Println("certs: certificate saved and loaded")
	return nil
}

func (m *Manager) useSelfSigned() error {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		DNSNames:     []string{"*." + m.cfg.Domain, m.cfg.Domain},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.cert = &tlsCert
	m.mu.Unlock()
	log.Println("certs: using self-signed certificate (Let's Encrypt will retry)")
	return nil
}

// ---- ACME user --------------------------------------------------------------

type acmeUser struct {
	Email        string                 `json:"email"`
	Registration *registration.Resource `json:"registration,omitempty"`
	key          crypto.PrivateKey
}

func (u *acmeUser) GetEmail() string                        { return u.Email }
func (u *acmeUser) GetRegistration() *registration.Resource { return u.Registration }
func (u *acmeUser) GetPrivateKey() crypto.PrivateKey        { return u.key }

func loadOrCreateUser(dir, domain string) (*acmeUser, error) {
	keyPath := filepath.Join(dir, "account.key")
	regPath := filepath.Join(dir, "account.json")

	var key *ecdsa.PrivateKey
	if raw, err := os.ReadFile(keyPath); err == nil { //nolint:gosec // path constructed from trusted config dir
		block, _ := pem.Decode(raw)
		if block != nil {
			if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
				key = k
			}
		}
	}
	if key == nil {
		var err error
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			return nil, err
		}
		der, _ := x509.MarshalECPrivateKey(key)
		_ = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600)
	}

	user := &acmeUser{
		Email: "admin@" + domain,
		key:   key,
	}
	if raw, err := os.ReadFile(regPath); err == nil { //nolint:gosec // path constructed from trusted config dir
		var reg registration.Resource
		if json.Unmarshal(raw, &reg) == nil {
			user.Registration = &reg
		}
	}
	return user, nil
}

func saveUser(dir string, user *acmeUser) error {
	if user.Registration == nil {
		return nil
	}
	raw, err := json.Marshal(user.Registration)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "account.json"), raw, 0o600)
}

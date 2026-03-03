package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Service is a subdomain-assigned service (shown on homepage).
type Service struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Subdomain   string    `json:"subdomain"`
	Target      string    `json:"target"` // e.g. http://10.0.0.5:8080
	Icon        string    `json:"icon,omitempty"`
	Source      string    `json:"source"` // "docker" | "network" | "manual"
	ContainerID string    `json:"container_id,omitempty"`
	DNSRecordID string    `json:"dns_record_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// DiscoveredService is a network/docker service not yet assigned a subdomain.
type DiscoveredService struct {
	ID            string    `json:"id"`
	IP            string    `json:"ip"`
	Port          int       `json:"port"`
	Title         string    `json:"title"`
	Icon          string    `json:"icon,omitempty"`
	Source        string    `json:"source"` // "docker" | "network"
	ContainerName string    `json:"container_name,omitempty"`
	ContainerID   string    `json:"container_id,omitempty"`
	DiscoveredAt  time.Time `json:"discovered_at"`
}

type data struct {
	Services     map[string]*Service   `json:"services"`
	Discovered   []*DiscoveredService  `json:"discovered"`
	DDNSDomains  []string              `json:"ddns_domains"`
	ScanSubnets  []string              `json:"scan_subnets"`
	LastScan     time.Time             `json:"last_scan"`
	PublicIP     string                `json:"public_ip"`
}

// Store is a thread-safe, JSON-backed persistence layer.
type Store struct {
	mu   sync.RWMutex
	d    data
	path string
}

func New(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	s := &Store{
		path: filepath.Join(dataDir, "config.json"),
		d: data{
			Services:    make(map[string]*Service),
			Discovered:  []*DiscoveredService{},
			DDNSDomains: []string{},
			ScanSubnets: []string{},
		},
	}
	if err := s.load(); err != nil && !os.IsNotExist(err) {
		return nil, fmt.Errorf("load store: %w", err)
	}
	return s, nil
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, &s.d)
}

// Save flushes the store to disk. Safe to call concurrently.
func (s *Store) Save() error {
	s.mu.RLock()
	raw, err := json.MarshalIndent(s.d, "", "  ")
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ---- Services ---------------------------------------------------------------

func (s *Store) GetAllServices() []*Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Service, 0, len(s.d.Services))
	for _, svc := range s.d.Services {
		out = append(out, svc)
	}
	return out
}

func (s *Store) GetServiceBySubdomain(sub string) *Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.d.Services[sub]
}

func (s *Store) GetServiceByID(id string) *Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, svc := range s.d.Services {
		if svc.ID == id {
			return svc
		}
	}
	return nil
}

func (s *Store) GetServiceByContainerID(cid string) *Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, svc := range s.d.Services {
		if svc.ContainerID == cid {
			return svc
		}
	}
	return nil
}

func (s *Store) AddService(svc *Service) {
	s.mu.Lock()
	s.d.Services[svc.Subdomain] = svc
	s.mu.Unlock()
}

// UpdateService replaces a service and returns the old DNS record ID (if subdomain changed).
func (s *Store) UpdateService(id string, updated *Service) (oldSub, oldDNSID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for sub, svc := range s.d.Services {
		if svc.ID == id {
			oldSub = sub
			oldDNSID = svc.DNSRecordID
			delete(s.d.Services, sub)
			s.d.Services[updated.Subdomain] = updated
			return
		}
	}
	return
}

func (s *Store) DeleteService(id string) (sub, dnsID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, svc := range s.d.Services {
		if svc.ID == id {
			sub = k
			dnsID = svc.DNSRecordID
			delete(s.d.Services, k)
			return
		}
	}
	return
}

// ---- Discovered -------------------------------------------------------------

func (s *Store) GetAllDiscovered() []*DiscoveredService {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*DiscoveredService, len(s.d.Discovered))
	copy(out, s.d.Discovered)
	return out
}

func (s *Store) GetDiscoveredByID(id string) *DiscoveredService {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, d := range s.d.Discovered {
		if d.ID == id {
			return d
		}
	}
	return nil
}

func (s *Store) GetDiscoveredByContainerID(cid string) *DiscoveredService {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, d := range s.d.Discovered {
		if d.ContainerID == cid {
			return d
		}
	}
	return nil
}

func (s *Store) AddDiscovered(d *DiscoveredService) {
	s.mu.Lock()
	// Deduplicate by IP+port or ContainerID
	for _, existing := range s.d.Discovered {
		if existing.ContainerID != "" && existing.ContainerID == d.ContainerID {
			s.mu.Unlock()
			return
		}
		if existing.IP == d.IP && existing.Port == d.Port {
			s.mu.Unlock()
			return
		}
	}
	s.d.Discovered = append(s.d.Discovered, d)
	s.mu.Unlock()
}

func (s *Store) RemoveDiscovered(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	filtered := s.d.Discovered[:0]
	for _, d := range s.d.Discovered {
		if d.ID != id {
			filtered = append(filtered, d)
		}
	}
	s.d.Discovered = filtered
}

func (s *Store) RemoveDiscoveredByContainerID(cid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	filtered := s.d.Discovered[:0]
	for _, d := range s.d.Discovered {
		if d.ContainerID != cid {
			filtered = append(filtered, d)
		}
	}
	s.d.Discovered = filtered
}

// ReplaceNetworkDiscovered replaces all network-discovered entries with a new list,
// preserving any Docker-discovered entries.
func (s *Store) ReplaceNetworkDiscovered(services []*DiscoveredService) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Collect Docker entries into a new slice (avoid aliasing the original).
	var dockerOnly []*DiscoveredService
	for _, d := range s.d.Discovered {
		if d.Source == "docker" {
			dockerOnly = append(dockerOnly, d)
		}
	}
	s.d.Discovered = append(dockerOnly, services...)
}

// ---- DDNS -------------------------------------------------------------------

func (s *Store) GetDDNSDomains() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.d.DDNSDomains))
	copy(out, s.d.DDNSDomains)
	return out
}

func (s *Store) AddDDNSDomain(domain string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.d.DDNSDomains {
		if d == domain {
			return
		}
	}
	s.d.DDNSDomains = append(s.d.DDNSDomains, domain)
}

func (s *Store) RemoveDDNSDomain(domain string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	filtered := s.d.DDNSDomains[:0]
	for _, d := range s.d.DDNSDomains {
		if d != domain {
			filtered = append(filtered, d)
		}
	}
	s.d.DDNSDomains = filtered
}

// ---- Scan subnets -----------------------------------------------------------

func (s *Store) GetScanSubnets() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, len(s.d.ScanSubnets))
	copy(out, s.d.ScanSubnets)
	return out
}

func (s *Store) AddScanSubnet(cidr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.d.ScanSubnets {
		if c == cidr {
			return
		}
	}
	s.d.ScanSubnets = append(s.d.ScanSubnets, cidr)
}

func (s *Store) RemoveScanSubnet(cidr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	filtered := s.d.ScanSubnets[:0]
	for _, c := range s.d.ScanSubnets {
		if c != cidr {
			filtered = append(filtered, c)
		}
	}
	s.d.ScanSubnets = filtered
}

// ---- Scan status / public IP ------------------------------------------------

func (s *Store) SetLastScan(t time.Time) {
	s.mu.Lock()
	s.d.LastScan = t
	s.mu.Unlock()
}

func (s *Store) GetLastScan() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.d.LastScan
}

func (s *Store) SetPublicIP(ip string) {
	s.mu.Lock()
	s.d.PublicIP = ip
	s.mu.Unlock()
}

func (s *Store) GetPublicIP() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.d.PublicIP
}

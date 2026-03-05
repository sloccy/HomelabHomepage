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
	Category    string    `json:"category,omitempty"`
	Order       int       `json:"order,omitempty"`
	Source      string    `json:"source"` // "docker" | "network" | "manual"
	ContainerID string    `json:"container_id,omitempty"`
	DNSRecordID string    `json:"dns_record_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// Bookmark is a plain external link shown on the homepage.
type Bookmark struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Icon     string `json:"icon,omitempty"`
	Category string `json:"category,omitempty"`
}

// IgnoredService is a discovered service the user has chosen to suppress.
type IgnoredService struct {
	ID        string    `json:"id"`
	IP        string    `json:"ip"`
	Port      int       `json:"port"`
	Title     string    `json:"title,omitempty"`
	IgnoredAt time.Time `json:"ignored_at"`
}

// DiscoveredService is a network/docker service not yet assigned a subdomain.
type DiscoveredService struct {
	ID            string    `json:"id"`
	IP            string    `json:"ip"`
	Port          int       `json:"port"`
	Title         string    `json:"title"`
	Icon          string    `json:"icon,omitempty"`
	ServiceName   string    `json:"service_name,omitempty"`
	Confidence    float32   `json:"confidence,omitempty"`
	Source        string    `json:"source"` // "docker" | "network"
	ContainerName string    `json:"container_name,omitempty"`
	ContainerID   string    `json:"container_id,omitempty"`
	DiscoveredAt  time.Time `json:"discovered_at"`
}

type Settings struct {
	Background string `json:"background"` // CSS value: gradient or image URL
}

type data struct {
	Services     map[string]*Service   `json:"services"`
	Discovered   []*DiscoveredService  `json:"discovered"`
	Ignored      []*IgnoredService     `json:"ignored"`
	Bookmarks    []*Bookmark           `json:"bookmarks"`
	Settings     Settings              `json:"settings"`
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
			Ignored:     []*IgnoredService{},
			Bookmarks:   []*Bookmark{},
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

// ReorderServices sets the Order field on each service based on the provided
// slice of IDs (index 0 = first). Services not in the list keep their
// current Order value.
func (s *Store) ReorderServices(ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, id := range ids {
		for _, svc := range s.d.Services {
			if svc.ID == id {
				svc.Order = i
				break
			}
		}
	}
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

// ClearNetworkDiscovered removes all network-source discovered entries,
// preserving Docker-discovered entries. Called at the start of each scan.
func (s *Store) ClearNetworkDiscovered() {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keep []*DiscoveredService
	for _, d := range s.d.Discovered {
		if d.Source != "network" {
			keep = append(keep, d)
		}
	}
	s.d.Discovered = keep
}

// UpsertNetworkDiscovered updates an existing network entry by IP+port if found,
// otherwise appends it. Preserves the existing ID to avoid UI flicker.
func (s *Store) UpsertNetworkDiscovered(svc *DiscoveredService) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.d.Discovered {
		if existing.Source == "network" && existing.IP == svc.IP && existing.Port == svc.Port {
			existing.Title = svc.Title
			existing.Icon = svc.Icon
			existing.ServiceName = svc.ServiceName
			existing.Confidence = svc.Confidence
			existing.DiscoveredAt = svc.DiscoveredAt
			return
		}
	}
	s.d.Discovered = append(s.d.Discovered, svc)
}

// ---- Ignored ----------------------------------------------------------------

// IgnoreDiscovered moves a discovered entry into the ignored list.
func (s *Store) IgnoreDiscovered(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found *DiscoveredService
	filtered := s.d.Discovered[:0]
	for _, d := range s.d.Discovered {
		if d.ID == id {
			found = d
		} else {
			filtered = append(filtered, d)
		}
	}
	if found == nil {
		return fmt.Errorf("discovered service %q not found", id)
	}
	s.d.Discovered = filtered
	s.d.Ignored = append(s.d.Ignored, &IgnoredService{
		ID:        found.ID,
		IP:        found.IP,
		Port:      found.Port,
		Title:     found.Title,
		IgnoredAt: time.Now(),
	})
	return nil
}

func (s *Store) GetIgnored() []*IgnoredService {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*IgnoredService, len(s.d.Ignored))
	copy(out, s.d.Ignored)
	return out
}

// UnignoreService removes an entry from the ignored list by ID.
func (s *Store) UnignoreService(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	filtered := s.d.Ignored[:0]
	found := false
	for _, ig := range s.d.Ignored {
		if ig.ID == id {
			found = true
		} else {
			filtered = append(filtered, ig)
		}
	}
	if !found {
		return fmt.Errorf("ignored service %q not found", id)
	}
	s.d.Ignored = filtered
	return nil
}

// IsIgnored reports whether the given IP:port pair is in the ignored list.
func (s *Store) IsIgnored(ip string, port int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, ig := range s.d.Ignored {
		if ig.IP == ip && ig.Port == port {
			return true
		}
	}
	return false
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

// ---- Bookmarks --------------------------------------------------------------

func (s *Store) GetAllBookmarks() []*Bookmark {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Bookmark, len(s.d.Bookmarks))
	copy(out, s.d.Bookmarks)
	return out
}

func (s *Store) AddBookmark(b *Bookmark) {
	s.mu.Lock()
	s.d.Bookmarks = append(s.d.Bookmarks, b)
	s.mu.Unlock()
}

func (s *Store) UpdateBookmark(id string, updated *Bookmark) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, b := range s.d.Bookmarks {
		if b.ID == id {
			s.d.Bookmarks[i] = updated
			return true
		}
	}
	return false
}

func (s *Store) DeleteBookmark(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, b := range s.d.Bookmarks {
		if b.ID == id {
			s.d.Bookmarks = append(s.d.Bookmarks[:i], s.d.Bookmarks[i+1:]...)
			return true
		}
	}
	return false
}

// ---- Settings ---------------------------------------------------------------

func (s *Store) GetSettings() Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.d.Settings
}

func (s *Store) UpdateSettings(settings Settings) {
	s.mu.Lock()
	s.d.Settings = settings
	s.mu.Unlock()
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

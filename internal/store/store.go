package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
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
	Source         string    `json:"source"` // "docker" | "network" | "manual"
	ContainerID    string    `json:"container_id,omitempty"`
	ContainerName  string    `json:"container_name,omitempty"`
	DNSRecordID    string    `json:"dns_record_id,omitempty"`
	TunnelRouteID  string    `json:"tunnel_route_id,omitempty"` // hostname routed via CF tunnel
	SkipHealth     bool      `json:"skip_health,omitempty"`
	DirectOnly     bool      `json:"direct_only,omitempty"` // link directly to target, no subdomain/DNS
	CreatedAt      time.Time `json:"created_at"`
}

// Bookmark is a plain external link shown on the homepage.
type Bookmark struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Icon     string `json:"icon,omitempty"`
	Category string `json:"category,omitempty"`
	Order    int    `json:"order,omitempty"`
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
	ContainerName      string    `json:"container_name,omitempty"`
	ContainerID        string    `json:"container_id,omitempty"`
	SuggestedSubdomain string    `json:"suggested_subdomain,omitempty"`
	DiscoveredAt       time.Time `json:"discovered_at"`
}


// TunnelInfo holds the persisted Cloudflare Tunnel credentials managed by Lantern.
type TunnelInfo struct {
	TunnelID  string    `json:"tunnel_id"`
	Token     string    `json:"token"`
	CreatedAt time.Time `json:"created_at"`
}

type data struct {
	Services     map[string]*Service   `json:"services"`
	Discovered   []*DiscoveredService  `json:"discovered"`
	Ignored      []*IgnoredService     `json:"ignored"`
	Bookmarks    []*Bookmark           `json:"bookmarks"`
	DDNSDomains  []string              `json:"ddns_domains"`
	ScanSubnets  []string              `json:"scan_subnets"`
	LastScan     time.Time             `json:"last_scan"`
	PublicIP     string                `json:"public_ip"`
	Tunnel       *TunnelInfo           `json:"tunnel,omitempty"`
}

// Store is a thread-safe, JSON-backed persistence layer.
type Store struct {
	mu               sync.RWMutex
	saveMu           sync.Mutex // serialises concurrent disk writes
	d                data
	path             string
	iconDir          string
	idIdx            map[string]*Service // service ID -> *Service
	containerIDIdx   map[string]*Service // container ID -> *Service
	containerNameIdx map[string]*Service // docker container name -> *Service
}

func New(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	iconDir := filepath.Join(dataDir, "icons")
	if err := os.MkdirAll(iconDir, 0o755); err != nil {
		return nil, fmt.Errorf("create icon dir: %w", err)
	}
	s := &Store{
		path:    filepath.Join(dataDir, "config.json"),
		iconDir: iconDir,
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
	s.migrateIcons()
	return s, nil
}

func (s *Store) load() error {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(raw, &s.d); err != nil {
		return err
	}
	s.rebuildIndexes()
	return nil
}

// rebuildIndexes reconstructs the secondary lookup maps from s.d.Services.
// Must be called with the write lock held (or during initialization before the
// store is shared).
func (s *Store) rebuildIndexes() {
	s.idIdx = make(map[string]*Service, len(s.d.Services))
	s.containerIDIdx = make(map[string]*Service, len(s.d.Services))
	s.containerNameIdx = make(map[string]*Service, len(s.d.Services))
	for _, svc := range s.d.Services {
		s.idIdx[svc.ID] = svc
		if svc.ContainerID != "" {
			s.containerIDIdx[svc.ContainerID] = svc
		}
		if svc.Source == "docker" && svc.ContainerName != "" {
			s.containerNameIdx[svc.ContainerName] = svc
		}
	}
}

// Save flushes the store to disk. Safe to call concurrently.
func (s *Store) Save() error {
	s.mu.RLock()
	raw, err := json.Marshal(s.d)
	s.mu.RUnlock()
	if err != nil {
		return err
	}
	s.saveMu.Lock()
	defer s.saveMu.Unlock()
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// ---- Icon file storage ------------------------------------------------------

// iconPath returns the filesystem path for an entity's icon file.
func (s *Store) iconPath(id string) string {
	return filepath.Join(s.iconDir, id)
}

// WriteIcon writes raw icon bytes to disk for the given entity ID.
func (s *Store) WriteIcon(id string, data []byte) error {
	return os.WriteFile(s.iconPath(id), data, 0o644)
}

// ReadIcon reads the raw icon bytes for the given entity ID.
func (s *Store) ReadIcon(id string) ([]byte, error) {
	return os.ReadFile(s.iconPath(id))
}

// DeleteIcon removes the icon file for the given entity ID (best-effort).
func (s *Store) DeleteIcon(id string) {
	_ = os.Remove(s.iconPath(id))
}

// HasIcon reports whether an icon file exists for the given entity ID.
func (s *Store) HasIcon(id string) bool {
	_, err := os.Stat(s.iconPath(id))
	return err == nil
}

// migrateIcons converts any base64 data URIs stored in the Icon field to on-disk
// files and clears the field. Called once at startup after loading the JSON store.
func (s *Store) migrateIcons() {
	migrated := 0
	for _, svc := range s.d.Services {
		if strings.HasPrefix(svc.Icon, "data:") {
			if writeDataURI(s.iconPath(svc.ID), svc.Icon) {
				svc.Icon = "file"
				migrated++
			}
		}
	}
	for _, bm := range s.d.Bookmarks {
		if strings.HasPrefix(bm.Icon, "data:") {
			if writeDataURI(s.iconPath(bm.ID), bm.Icon) {
				bm.Icon = "file"
				migrated++
			}
		}
	}
	for _, d := range s.d.Discovered {
		if strings.HasPrefix(d.Icon, "data:") {
			if writeDataURI(s.iconPath(d.ID), d.Icon) {
				d.Icon = "file"
				migrated++
			}
		}
	}
	if migrated > 0 {
		log.Printf("store: migrated %d base64 icons to files", migrated)
		if err := s.Save(); err != nil {
			log.Printf("store: save after icon migration: %v", err)
		}
	}
}

// writeDataURI decodes a base64 data URI and writes the bytes to path.
// Returns true on success.
func writeDataURI(path, dataURI string) bool {
	// Format: data:<mime>;base64,<data>
	comma := strings.IndexByte(dataURI, ',')
	if comma < 0 {
		return false
	}
	data, err := base64.StdEncoding.DecodeString(dataURI[comma+1:])
	if err != nil || len(data) == 0 {
		return false
	}
	return os.WriteFile(path, data, 0o644) == nil
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
	return s.idIdx[id]
}

func (s *Store) GetServiceByContainerID(cid string) *Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.containerIDIdx[cid]
}

func (s *Store) GetServiceByContainerName(name string) *Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.containerNameIdx[name]
}

// ClearContainerID detaches a running container from its service entry without
// deleting the service. This preserves user customisations across restarts.
func (s *Store) ClearContainerID(cid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if svc, ok := s.containerIDIdx[cid]; ok {
		svc.ContainerID = ""
		s.rebuildIndexes()
	}
}

func (s *Store) AddService(svc *Service) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Services[svc.Subdomain] = svc
	s.rebuildIndexes()
}

// UpdateService replaces a service and returns the old DNS record ID (if subdomain changed).
func (s *Store) UpdateService(id string, updated *Service) (oldSub, oldDNSID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if svc := s.idIdx[id]; svc != nil {
		for sub, sv := range s.d.Services {
			if sv == svc {
				oldSub = sub
				break
			}
		}
		oldDNSID = svc.DNSRecordID
		delete(s.d.Services, oldSub)
		s.d.Services[updated.Subdomain] = updated
		s.rebuildIndexes()
	}
	return
}

func (s *Store) DeleteService(id string) (sub, dnsID, tunnelRoute string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if svc := s.idIdx[id]; svc != nil {
		for k, sv := range s.d.Services {
			if sv == svc {
				sub = k
				break
			}
		}
		dnsID = svc.DNSRecordID
		tunnelRoute = svc.TunnelRouteID
		delete(s.d.Services, sub)
		s.rebuildIndexes()
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
		if svc := s.idIdx[id]; svc != nil {
			svc.Order = i
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
	defer s.mu.Unlock()
	// Deduplicate by IP+port or ContainerID
	for _, existing := range s.d.Discovered {
		if existing.ContainerID != "" && existing.ContainerID == d.ContainerID {
			return
		}
		if existing.IP == d.IP && existing.Port == d.Port {
			return
		}
	}
	s.d.Discovered = append(s.d.Discovered, d)
}

func (s *Store) RemoveDiscovered(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	orig := s.d.Discovered
	filtered := orig[:0]
	for _, d := range orig {
		if d.ID != id {
			filtered = append(filtered, d)
		}
	}
	for i := len(filtered); i < len(orig); i++ {
		orig[i] = nil
	}
	s.d.Discovered = filtered
}

func (s *Store) RemoveDiscoveredByContainerID(cid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	orig := s.d.Discovered
	filtered := orig[:0]
	for _, d := range orig {
		if d.ContainerID != cid {
			filtered = append(filtered, d)
		}
	}
	for i := len(filtered); i < len(orig); i++ {
		orig[i] = nil
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
// Returns the ID of the entry that was created or updated.
func (s *Store) UpsertNetworkDiscovered(svc *DiscoveredService) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.d.Discovered {
		if existing.Source == "network" && existing.IP == svc.IP && existing.Port == svc.Port {
			existing.Title = svc.Title
			existing.Icon = svc.Icon
			existing.ServiceName = svc.ServiceName
			existing.Confidence = svc.Confidence
			existing.DiscoveredAt = svc.DiscoveredAt
			return existing.ID
		}
	}
	s.d.Discovered = append(s.d.Discovered, svc)
	return svc.ID
}

// UpdateDiscoveredIcon sets the icon for a discovered service by ID.
func (s *Store) UpdateDiscoveredIcon(id, icon string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, d := range s.d.Discovered {
		if d.ID == id {
			d.Icon = icon
			return
		}
	}
}

// ---- Ignored ----------------------------------------------------------------

// IgnoreDiscovered moves a discovered entry into the ignored list.
func (s *Store) IgnoreDiscovered(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var found *DiscoveredService
	orig := s.d.Discovered
	filtered := orig[:0]
	for _, d := range orig {
		if d.ID == id {
			found = d
		} else {
			filtered = append(filtered, d)
		}
	}
	if found == nil {
		return fmt.Errorf("discovered service %q not found", id)
	}
	for i := len(filtered); i < len(orig); i++ {
		orig[i] = nil
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

// UnignoreService removes an entry from the ignored list by ID and returns it.
func (s *Store) UnignoreService(id string) (*IgnoredService, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	orig := s.d.Ignored
	filtered := orig[:0]
	var removed *IgnoredService
	for _, ig := range orig {
		if ig.ID == id {
			removed = ig
		} else {
			filtered = append(filtered, ig)
		}
	}
	if removed == nil {
		return nil, fmt.Errorf("ignored service %q not found", id)
	}
	for i := len(filtered); i < len(orig); i++ {
		orig[i] = nil
	}
	s.d.Ignored = filtered
	return removed, nil
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

func (s *Store) GetBookmarkByID(id string) *Bookmark {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, b := range s.d.Bookmarks {
		if b.ID == id {
			return b
		}
	}
	return nil
}

func (s *Store) GetAllBookmarks() []*Bookmark {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*Bookmark, len(s.d.Bookmarks))
	copy(out, s.d.Bookmarks)
	return out
}

func (s *Store) AddBookmark(b *Bookmark) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Bookmarks = append(s.d.Bookmarks, b)
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

// ReorderBookmarks sets the Order field on each bookmark based on the provided
// slice of IDs (index 0 = first). Bookmarks not in the list keep their
// current Order value.
func (s *Store) ReorderBookmarks(ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, id := range ids {
		for _, bm := range s.d.Bookmarks {
			if bm.ID == id {
				bm.Order = i
				break
			}
		}
	}
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

// ---- Scan status / public IP ------------------------------------------------

func (s *Store) SetLastScan(t time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.LastScan = t
}

func (s *Store) GetLastScan() time.Time {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.d.LastScan
}

func (s *Store) SetPublicIP(ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.PublicIP = ip
}

func (s *Store) GetPublicIP() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.d.PublicIP
}

// ---- Tunnel -----------------------------------------------------------------

func (s *Store) GetTunnel() *TunnelInfo {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.d.Tunnel
}

func (s *Store) SetTunnel(info *TunnelInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Tunnel = info
}

func (s *Store) ClearTunnel() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Tunnel = nil
}

package store

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// Source values for Service.Source and DiscoveredService.Source.
const (
	SourceDocker  = "docker"
	SourceNetwork = "network"
	SourceManual  = "manual"
)

// IconFile is the sentinel value for Icon fields meaning the icon is stored
// as a file on disk under the entity's ID.
const IconFile = "file"

// Service is a subdomain-assigned service (shown on homepage).
type Service struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	Subdomain     string    `json:"subdomain"`
	Target        string    `json:"target"` // e.g. http://10.0.0.5:8080
	Icon          string    `json:"icon,omitempty"`
	Category      string    `json:"category,omitempty"`
	Order         int       `json:"order,omitempty"`
	Source        string    `json:"source"` // "docker" | "network" | "manual"
	ContainerID   string    `json:"container_id,omitempty"`
	ContainerName string    `json:"container_name,omitempty"`
	DNSRecordID   string    `json:"dns_record_id,omitempty"`
	TunnelRouteID string    `json:"tunnel_route_id,omitempty"` // hostname routed via CF tunnel
	SkipHealth    bool      `json:"skip_health,omitempty"`
	DirectOnly    bool      `json:"direct_only,omitempty"` // link directly to target, no subdomain/DNS
	CreatedAt     time.Time `json:"created_at"`
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
	ID                 string    `json:"id"`
	IP                 string    `json:"ip"`
	Port               int       `json:"port"`
	Title              string    `json:"title"`
	Icon               string    `json:"icon,omitempty"`
	ServiceName        string    `json:"service_name,omitempty"`
	Confidence         float32   `json:"confidence,omitempty"`
	Source             string    `json:"source"` // "docker" | "network"
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
	Services    map[string]*Service  `json:"services"`
	Discovered  []*DiscoveredService `json:"discovered"`
	Ignored     []*IgnoredService    `json:"ignored"`
	Bookmarks   []*Bookmark          `json:"bookmarks"`
	DDNSDomains []string             `json:"ddns_domains"`
	ScanSubnets []string             `json:"scan_subnets"`
	LastScan    time.Time            `json:"last_scan"`
	PublicIP    string               `json:"public_ip"`
	Tunnel      *TunnelInfo          `json:"tunnel,omitempty"`
}

type Store struct {
	mu                     sync.RWMutex
	saveMu                 sync.Mutex // serialises concurrent disk writes
	d                      data
	path                   string
	iconDir                string
	idIdx                  map[string]*Service           // service ID -> *Service
	containerIDIdx         map[string]*Service           // container ID -> *Service
	containerNameIdx       map[string]*Service           // docker container name -> *Service
	bookmarkIdx            map[string]*Bookmark          // bookmark ID -> *Bookmark
	discoveredIdx          map[string]*DiscoveredService // discovered ID -> *DiscoveredService
	discoveredContainerIdx map[string]*DiscoveredService // container ID -> *DiscoveredService
	ignoredIdx             map[string]*IgnoredService    // ignored ID -> *IgnoredService
	ignoredIPPortIdx       map[string]struct{}           // "ip:port" -> present
}

func New(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o750); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	iconDir := filepath.Join(dataDir, "icons")
	if err := os.MkdirAll(iconDir, 0o750); err != nil {
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
		idIdx:                  make(map[string]*Service),
		containerIDIdx:         make(map[string]*Service),
		containerNameIdx:       make(map[string]*Service),
		bookmarkIdx:            make(map[string]*Bookmark),
		discoveredIdx:          make(map[string]*DiscoveredService),
		discoveredContainerIdx: make(map[string]*DiscoveredService),
		ignoredIdx:             make(map[string]*IgnoredService),
		ignoredIPPortIdx:       make(map[string]struct{}),
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

// rebuildIndexes reconstructs all secondary lookup maps from s.d.
// Must be called with the write lock held (or during initialization before the
// store is shared).
func (s *Store) rebuildIndexes() {
	s.idIdx = make(map[string]*Service, len(s.d.Services))
	s.containerIDIdx = make(map[string]*Service, len(s.d.Services))
	s.containerNameIdx = make(map[string]*Service, len(s.d.Services))
	for _, svc := range s.d.Services {
		s.indexService(svc)
	}
	s.bookmarkIdx = make(map[string]*Bookmark, len(s.d.Bookmarks))
	for _, b := range s.d.Bookmarks {
		s.indexBookmark(b)
	}
	s.discoveredIdx = make(map[string]*DiscoveredService, len(s.d.Discovered))
	s.discoveredContainerIdx = make(map[string]*DiscoveredService)
	for _, d := range s.d.Discovered {
		s.indexDiscovered(d)
	}
	s.ignoredIdx = make(map[string]*IgnoredService, len(s.d.Ignored))
	s.ignoredIPPortIdx = make(map[string]struct{}, len(s.d.Ignored))
	for _, ig := range s.d.Ignored {
		s.indexIgnored(ig)
	}
}

// indexService adds a service to all secondary indexes. Must be called with the write lock held.
func (s *Store) indexService(svc *Service) {
	s.idIdx[svc.ID] = svc
	if svc.ContainerID != "" {
		s.containerIDIdx[svc.ContainerID] = svc
	}
	if svc.Source == SourceDocker && svc.ContainerName != "" {
		s.containerNameIdx[svc.ContainerName] = svc
	}
}

// unindexService removes a service from all secondary indexes. Must be called with the write lock held.
func (s *Store) unindexService(svc *Service) {
	delete(s.idIdx, svc.ID)
	delete(s.containerIDIdx, svc.ContainerID)
	delete(s.containerNameIdx, svc.ContainerName)
}

func (s *Store) indexBookmark(b *Bookmark)   { s.bookmarkIdx[b.ID] = b }
func (s *Store) unindexBookmark(b *Bookmark) { delete(s.bookmarkIdx, b.ID) }

func (s *Store) indexDiscovered(d *DiscoveredService) {
	s.discoveredIdx[d.ID] = d
	if d.ContainerID != "" {
		s.discoveredContainerIdx[d.ContainerID] = d
	}
}
func (s *Store) unindexDiscovered(d *DiscoveredService) {
	delete(s.discoveredIdx, d.ID)
	if d.ContainerID != "" {
		delete(s.discoveredContainerIdx, d.ContainerID)
	}
}

func ignoredKey(ip string, port int) string { return fmt.Sprintf("%s:%d", ip, port) }
func (s *Store) indexIgnored(ig *IgnoredService) {
	s.ignoredIdx[ig.ID] = ig
	s.ignoredIPPortIdx[ignoredKey(ig.IP, ig.Port)] = struct{}{}
}
func (s *Store) unindexIgnored(ig *IgnoredService) {
	delete(s.ignoredIdx, ig.ID)
	delete(s.ignoredIPPortIdx, ignoredKey(ig.IP, ig.Port))
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
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// SaveLog calls Save and logs any error with the given context prefix.
func (s *Store) SaveLog(ctx string) {
	if err := s.Save(); err != nil {
		log.Printf("%s: save: %v", ctx, err)
	}
}

// ---- Icon file storage ------------------------------------------------------

func (s *Store) iconPath(id string) string {
	return filepath.Join(s.iconDir, id)
}

// safeIconPath returns the icon file path for id after validating that id is a
// clean, single-component name that stays within iconDir. This prevents path
// traversal even if callers forget to validate the id themselves.
func (s *Store) safeIconPath(id string) (string, error) {
	if strings.ContainsAny(id, "/\\") || strings.Contains(id, "..") || id == "" {
		return "", fmt.Errorf("invalid icon id: %q", id)
	}
	p := filepath.Join(s.iconDir, filepath.Clean(id))
	if !strings.HasPrefix(p, s.iconDir) {
		return "", fmt.Errorf("icon path escapes icon directory: %q", id)
	}
	return p, nil
}

func (s *Store) WriteIcon(id string, data []byte) error {
	p, err := s.safeIconPath(id)
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o600)
}

func (s *Store) ReadIcon(id string) ([]byte, error) {
	p, err := s.safeIconPath(id)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p) //nolint:gosec // path validated by safeIconPath
}

func (s *Store) DeleteIcon(id string) {
	p, err := s.safeIconPath(id)
	if err != nil {
		return
	}
	_ = os.Remove(p)
}

// migrateIcons converts any base64 data URIs stored in the Icon field to on-disk
// files and clears the field. Called once at startup after loading the JSON store.
func (s *Store) migrateIcons() {
	migrated := 0
	for _, svc := range s.d.Services {
		if strings.HasPrefix(svc.Icon, "data:") {
			if writeDataURI(s.iconPath(svc.ID), svc.Icon) {
				svc.Icon = IconFile
				migrated++
			}
		}
	}
	for _, bm := range s.d.Bookmarks {
		if strings.HasPrefix(bm.Icon, "data:") {
			if writeDataURI(s.iconPath(bm.ID), bm.Icon) {
				bm.Icon = IconFile
				migrated++
			}
		}
	}
	for _, d := range s.d.Discovered {
		if strings.HasPrefix(d.Icon, "data:") {
			if writeDataURI(s.iconPath(d.ID), d.Icon) {
				d.Icon = IconFile
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
	return os.WriteFile(path, data, 0o600) == nil
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
// Note: containerNameIdx is intentionally NOT cleared here. The name association
// survives container stop so that docker.go can reattach the service by name
// when the container restarts with a new container ID.
func (s *Store) ClearContainerID(cid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if svc, ok := s.containerIDIdx[cid]; ok {
		delete(s.containerIDIdx, cid)
		svc.ContainerID = ""
	}
}

func (s *Store) AddService(svc *Service) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.d.Services[svc.Subdomain] = svc
	s.indexService(svc)
}

// UpdateService replaces a service and returns the old DNS record ID (if subdomain changed).
func (s *Store) UpdateService(id string, updated *Service) (oldSub, oldDNSID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if svc := s.idIdx[id]; svc != nil {
		oldSub = svc.Subdomain
		oldDNSID = svc.DNSRecordID
		s.unindexService(svc)
		delete(s.d.Services, oldSub)
		s.d.Services[updated.Subdomain] = updated
		s.indexService(updated)
	}
	return
}

func (s *Store) DeleteService(id string) (sub, dnsID, tunnelRoute string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if svc := s.idIdx[id]; svc != nil {
		sub = svc.Subdomain
		dnsID = svc.DNSRecordID
		tunnelRoute = svc.TunnelRouteID
		s.unindexService(svc)
		delete(s.d.Services, sub)
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

// ---- Slice helpers ----------------------------------------------------------

// filterSlice removes elements in place.
func filterSlice[T any](s *[]*T, keep func(*T) bool) {
	*s = slices.DeleteFunc(*s, func(v *T) bool { return !keep(v) })
}

// removeString removes the first occurrence of val from s in place.
func removeString(s *[]string, val string) {
	*s = slices.DeleteFunc(*s, func(v string) bool { return v == val })
}

// addUnique appends val to s only if it is not already present.
func addUnique(s *[]string, val string) {
	if !slices.Contains(*s, val) {
		*s = append(*s, val)
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
	return s.discoveredIdx[id]
}

func (s *Store) GetDiscoveredByContainerID(cid string) *DiscoveredService {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.discoveredContainerIdx[cid]
}

func (s *Store) AddDiscovered(d *DiscoveredService) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// Deduplicate by ContainerID (O(1)) or IP+port (O(n) fallback)
	if d.ContainerID != "" {
		if _, exists := s.discoveredContainerIdx[d.ContainerID]; exists {
			return
		}
	}
	for _, existing := range s.d.Discovered {
		if existing.IP == d.IP && existing.Port == d.Port {
			return
		}
	}
	s.d.Discovered = append(s.d.Discovered, d)
	s.indexDiscovered(d)
}

func (s *Store) RemoveDiscovered(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.discoveredIdx[id]; d != nil {
		s.unindexDiscovered(d)
	}
	filterSlice(&s.d.Discovered, func(d *DiscoveredService) bool { return d.ID != id })
}

func (s *Store) RemoveDiscoveredByContainerID(cid string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.discoveredContainerIdx[cid]; d != nil {
		s.unindexDiscovered(d)
	}
	filterSlice(&s.d.Discovered, func(d *DiscoveredService) bool { return d.ContainerID != cid })
}

// ClearNetworkDiscovered removes all network-source discovered entries,
// preserving Docker-discovered entries. Called at the start of each scan.
func (s *Store) ClearNetworkDiscovered() {
	s.mu.Lock()
	defer s.mu.Unlock()
	var keep []*DiscoveredService
	for _, d := range s.d.Discovered {
		if d.Source == SourceNetwork {
			s.unindexDiscovered(d)
		} else {
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
		if existing.Source == SourceNetwork && existing.IP == svc.IP && existing.Port == svc.Port {
			existing.Title = svc.Title
			existing.Icon = svc.Icon
			existing.ServiceName = svc.ServiceName
			existing.SuggestedSubdomain = svc.SuggestedSubdomain
			existing.Confidence = svc.Confidence
			existing.DiscoveredAt = svc.DiscoveredAt
			// Index already points to existing; no re-indexing needed.
			return existing.ID
		}
	}
	s.d.Discovered = append(s.d.Discovered, svc)
	s.indexDiscovered(svc)
	return svc.ID
}

// UpdateDiscoveredIcon sets the icon for a discovered service by ID.
func (s *Store) UpdateDiscoveredIcon(id, icon string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if d := s.discoveredIdx[id]; d != nil {
		d.Icon = icon
	}
}

// ---- Ignored ----------------------------------------------------------------

// IgnoreDiscovered moves a discovered entry into the ignored list.
func (s *Store) IgnoreDiscovered(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	found := s.discoveredIdx[id]
	if found == nil {
		return fmt.Errorf("discovered service %q not found", id)
	}
	s.unindexDiscovered(found)
	filterSlice(&s.d.Discovered, func(d *DiscoveredService) bool { return d.ID != id })
	ig := &IgnoredService{
		ID:        found.ID,
		IP:        found.IP,
		Port:      found.Port,
		Title:     found.Title,
		IgnoredAt: time.Now(),
	}
	s.d.Ignored = append(s.d.Ignored, ig)
	s.indexIgnored(ig)
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
	removed := s.ignoredIdx[id]
	if removed == nil {
		return nil, fmt.Errorf("ignored service %q not found", id)
	}
	s.unindexIgnored(removed)
	filterSlice(&s.d.Ignored, func(ig *IgnoredService) bool { return ig.ID != id })
	return removed, nil
}

// IsIgnored reports whether the given IP:port pair is in the ignored list.
func (s *Store) IsIgnored(ip string, port int) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.ignoredIPPortIdx[ignoredKey(ip, port)]
	return ok
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
	addUnique(&s.d.DDNSDomains, domain)
}

func (s *Store) RemoveDDNSDomain(domain string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	removeString(&s.d.DDNSDomains, domain)
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
	addUnique(&s.d.ScanSubnets, cidr)
}

func (s *Store) RemoveScanSubnet(cidr string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	removeString(&s.d.ScanSubnets, cidr)
}

// ---- Bookmarks --------------------------------------------------------------

func (s *Store) GetBookmarkByID(id string) *Bookmark {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bookmarkIdx[id]
}

// GetTarget returns the target URL for the service or bookmark with the given
// id, or an empty string if no matching entity is found.
func (s *Store) GetTarget(id string) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if svc, ok := s.idIdx[id]; ok {
		return svc.Target
	}
	if bm, ok := s.bookmarkIdx[id]; ok {
		return bm.URL
	}
	return ""
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
	s.indexBookmark(b)
}

func (s *Store) UpdateBookmark(id string, updated *Bookmark) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, b := range s.d.Bookmarks {
		if b.ID == id {
			s.unindexBookmark(b)
			s.d.Bookmarks[i] = updated
			s.indexBookmark(updated)
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
		if bm := s.bookmarkIdx[id]; bm != nil {
			bm.Order = i
		}
	}
}

func (s *Store) DeleteBookmark(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, b := range s.d.Bookmarks {
		if b.ID == id {
			s.unindexBookmark(b)
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

package discovery

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	dockerevents "github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
	"github.com/docker/go-connections/nat"

	"lantern/internal/store"
	"lantern/internal/util"
)

// Lantern Docker label keys.
const (
	labelEnable    = "lantern.enable"
	labelName      = "lantern.name"
	labelURL       = "lantern.url"
	labelSubdomain = "lantern.subdomain"
	labelPort      = "lantern.port"
	labelScheme    = "lantern.scheme"
)

// DockerWatch connects to the Docker socket and watches for container start/stop events.
// On start:    resolves config from labels, auto-assigns subdomain, creates DNS record.
// On stop/die: removes from services or discovered.
//
// Label reference (set on the container):
//
//	lantern.enable=false          — opt this container out entirely
//	lantern.name=Plex             — display name override
//	lantern.subdomain=plex        — subdomain override (default: container name)
//	lantern.port=32400            — port to use instead of the published port
//	lantern.scheme=https          — force https for the backend target
//	lantern.url=http://10.0.0.5:32400 — fully explicit target (overrides all above)
//
// Traefik v2/v3 labels are also understood as a fallback:
//
//	traefik.http.routers.<name>.rule=Host(`plex.example.com`)
//	traefik.http.services.<name>.loadbalancer.server.port=32400
func (d *Discoverer) DockerWatch(ctx context.Context) {
	dc, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		log.Printf("discovery: Docker client unavailable (%v) — skipping Docker discovery", err)
		return
	}
	defer func() { _ = dc.Close() }()

	// Verify connectivity.
	if _, err := dc.Ping(ctx); err != nil {
		log.Printf("discovery: Docker socket unavailable (%v) — skipping Docker discovery", err)
		return
	}

	d.syncContainers(ctx, dc)

	msgCh, errCh := dockerEvents(ctx, dc)

	log.Println("discovery: watching Docker events")
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-errCh:
			if ctx.Err() != nil {
				return
			}
			log.Printf("discovery: Docker events error: %v — reconnecting in 10s", err)
			select {
			case <-time.After(10 * time.Second):
			case <-ctx.Done():
				return
			}
			msgCh, errCh = dockerEvents(ctx, dc)
		case msg, ok := <-msgCh:
			if !ok {
				return
			}
			d.handleDockerEvent(ctx, dc, msg)
		}
	}
}

// dockerEvents subscribes to container events and returns message/error channels.
func dockerEvents(ctx context.Context, dc *client.Client) (msgs <-chan dockerevents.Message, errs <-chan error) {
	f := filters.NewArgs()
	f.Add("type", "container")
	return dc.Events(ctx, dockerevents.ListOptions{Filters: f})
}

func (d *Discoverer) syncContainers(ctx context.Context, dc *client.Client) {
	containers, err := dc.ContainerList(ctx, container.ListOptions{})
	if err != nil {
		log.Printf("discovery: list containers: %v", err)
		return
	}
	for _, c := range containers {
		if len(c.Names) == 0 {
			continue
		}
		name := strings.TrimPrefix(c.Names[0], "/")
		if name != "" {
			d.upsertContainerWithLabels(ctx, c.ID, name, c.Ports, c.Labels)
		}
	}
	log.Printf("discovery: synced %d running containers", len(containers))
}

func (d *Discoverer) handleDockerEvent(ctx context.Context, dc *client.Client, msg dockerevents.Message) {
	switch string(msg.Action) {
	case "start":
		select { // brief delay for container to fully start
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
			return
		}
		c, err := dc.ContainerInspect(ctx, msg.Actor.ID)
		if err != nil {
			return
		}
		name := strings.TrimPrefix(c.Name, "/")
		if name != "" {
			d.upsertContainerWithLabels(ctx, c.ID, name, portsFromNat(c.NetworkSettings.Ports), c.Config.Labels)
		}

	case "die", "stop", "destroy", "kill":
		d.detachContainer(msg.Actor.ID)
	}
}

// containerInfo holds the resolved display name, subdomain and backend target.
type containerInfo struct {
	name      string
	subdomain string
	target    string
}

// detachContainer clears the ContainerID from a service (preserving user
// customisations) and removes any discovered entry for that container.
// It does NOT delete the service — the entry stays on the homepage as offline.
func (d *Discoverer) detachContainer(id string) {
	d.store.ClearContainerID(id)
	d.store.RemoveDiscoveredByContainerID(id)
	d.store.SaveLog("discovery")
}

// upsertContainerWithLabels resolves a container's configuration from Docker labels,
// then creates or updates the service entry.
func (d *Discoverer) upsertContainerWithLabels(ctx context.Context, id, name string, ports []container.Port, labels map[string]string) {
	if name == "" || name == "lantern" {
		return
	}
	// lantern.enable=false → opt out.
	if labels[labelEnable] == "false" {
		return
	}
	// Skip if already tracked by this exact container ID.
	if d.store.GetServiceByContainerID(id) != nil {
		return
	}

	info := d.resolveContainer(ctx, name, ports, labels)
	if info == nil {
		return // no usable port/target
	}

	// Reattach: same container name as an existing docker service (e.g. restart/recreate).
	if existing := d.store.GetServiceByContainerName(name); existing != nil {
		updated := *existing
		updated.ContainerID = id
		updated.Target = preserveScheme(existing.Target, info.target)
		d.store.UpdateService(existing.ID, &updated)
		d.store.SaveLog("discovery")
		log.Printf("discovery: reattached %q → %s (%s)", name, existing.Subdomain, id)
		return
	}

	// Subdomain collision: if the existing service is docker-sourced, reattach
	// (handles pre-fix records with no ContainerName set, and renamed containers).
	// Only send to discovered if it's a manual/network service with the same subdomain.
	if existing := d.store.GetServiceBySubdomain(info.subdomain); existing != nil {
		if existing.Source == store.SourceDocker {
			updated := *existing
			updated.ContainerID = id
			updated.ContainerName = name // backfill for pre-fix records
			updated.Target = preserveScheme(existing.Target, info.target)
			d.store.UpdateService(existing.ID, &updated)
			d.store.SaveLog("discovery")
			log.Printf("discovery: reattached %q → %s (%s)", name, existing.Subdomain, id)
			return
		}
		d.addDockerDiscovered(id, info.name, info.target, info.subdomain) //nolint:contextcheck // favicon fetch is a background task with its own timeout
		return
	}

	// New container — send to Discovered for user review instead of auto-assigning.
	d.addDockerDiscovered(id, info.name, info.target, info.subdomain) //nolint:contextcheck // favicon fetch is a background task with its own timeout
}

// resolveContainer determines the display name, subdomain and target URL for a container
// by checking labels in priority order:
//  1. lantern.* labels
//  2. Traefik v2/v3 labels
//  3. Published ports (bestPort heuristic)
func (d *Discoverer) resolveContainer(ctx context.Context, name string, ports []container.Port, labels map[string]string) *containerInfo {
	info := &containerInfo{
		name:      name,
		subdomain: util.SanitiseSubdomain(name),
	}

	// Display name override.
	if n := labels[labelName]; n != "" {
		info.name = n
	}

	// Explicit target URL — takes full precedence over port logic.
	if u := labels[labelURL]; u != "" {
		info.target = u
		if s := labels[labelSubdomain]; s != "" {
			info.subdomain = util.SanitiseSubdomain(s)
		}
		return info
	}

	// Subdomain: lantern label > traefik rule > container name.
	if s := labels[labelSubdomain]; s != "" {
		info.subdomain = util.SanitiseSubdomain(s)
	} else if sub := traefikSubdomain(labels, d.cfg.Domain); sub != "" {
		info.subdomain = sub
	}

	// Port: lantern.port > traefik service port > bestPort(published).
	port := 0
	if p := labels[labelPort]; p != "" {
		port, _ = strconv.Atoi(p)
	}
	if port == 0 {
		port = traefikPort(labels)
	}
	if port == 0 {
		port = bestPort(ports)
	}
	if port == 0 {
		return nil
	}

	// Scheme: explicit label > live probe (falls back to port heuristic if unreachable).
	var scheme string
	if s := labels[labelScheme]; s == "https" || s == "http" {
		scheme = s
	} else {
		scheme = detectScheme(ctx, d.cfg.ServerIP, port)
	}

	info.target = fmt.Sprintf("%s://%s:%d", scheme, d.cfg.ServerIP, port)
	return info
}

func (d *Discoverer) addDockerDiscovered(id, name, target, suggestedSub string) {
	if d.store.GetDiscoveredByContainerID(id) != nil {
		return
	}
	ip, port := splitTarget(target)
	disc := &store.DiscoveredService{
		ID:                 util.NewID(),
		IP:                 ip,
		Port:               port,
		Title:              name,
		Source:             store.SourceDocker,
		ContainerName:      name,
		ContainerID:        id,
		SuggestedSubdomain: suggestedSub,
		DiscoveredAt:       time.Now(),
	}
	d.store.AddDiscovered(disc)
	d.store.SaveLog("discovery")

	go fetchAndStoreDiscoveredFavicon(d.store, disc.ID, target)
}

// fetchAndStoreDiscoveredFavicon fetches the favicon for target (a
// system-generated URL, not user input), writes it to the store under id, and
// updates the discovered service's Icon field.
func fetchAndStoreDiscoveredFavicon(st *store.Store, id, target string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data := util.FetchFaviconForTarget(ctx, target)
	if len(data) == 0 {
		return
	}
	if st.WriteIcon(id, data) != nil {
		return
	}
	st.UpdateDiscoveredIcon(id, store.IconFile)
	st.SaveLog("discovery")
}

// ── Traefik label helpers ─────────────────────────────────────────────────────

var reTraefikHost = regexp.MustCompile("(?i)Host\\(`([^`]+)`\\)")

// traefikSubdomain extracts the subdomain from a Traefik router rule label.
// Handles: traefik.http.routers.<name>.rule = Host(`sub.domain.com`)
func traefikSubdomain(labels map[string]string, domain string) string {
	for k, v := range labels {
		if !strings.HasPrefix(k, "traefik.http.routers.") || !strings.HasSuffix(k, ".rule") {
			continue
		}
		m := reTraefikHost.FindStringSubmatch(v)
		if len(m) < 2 {
			continue
		}
		host := strings.ToLower(m[1])
		if domain != "" && strings.HasSuffix(host, "."+domain) {
			return strings.TrimSuffix(host, "."+domain)
		}
		return util.SanitiseSubdomain(host)
	}
	return ""
}

// traefikPort extracts the backend port from a Traefik service label.
// Handles: traefik.http.services.<name>.loadbalancer.server.port = 32400
func traefikPort(labels map[string]string) int {
	for k, v := range labels {
		if !strings.HasPrefix(k, "traefik.http.services.") {
			continue
		}
		if !strings.HasSuffix(k, ".loadbalancer.server.port") {
			continue
		}
		port, _ := strconv.Atoi(v)
		return port
	}
	return 0
}

// ── Port / target helpers ─────────────────────────────────────────────────────

// bestPort picks the most useful published TCP port, preferring common web UI ports.
func bestPort(ports []container.Port) int {
	preferred := []int{80, 8080, 3000, 5000, 9443, 9000, 8096, 8123, 443, 8443, 8000}
	portSet := make(map[int]bool)
	for _, p := range ports {
		if p.Type == "tcp" && p.PublicPort > 0 {
			portSet[int(p.PublicPort)] = true
		}
	}
	for _, pp := range preferred {
		if portSet[pp] {
			return pp
		}
	}
	// Fall back to the first published TCP port (covers Plex:32400 etc.).
	for _, p := range ports {
		if p.Type == "tcp" && p.PublicPort > 0 {
			return int(p.PublicPort)
		}
	}
	return 0
}

// splitTarget splits "http://ip:port" into (ip, port).
func splitTarget(target string) (ip string, port int) {
	s := strings.TrimPrefix(target, "http://")
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimSuffix(s, "/")
	idx := strings.LastIndexByte(s, ':')
	if idx < 0 {
		return s, 0
	}
	port, _ = strconv.Atoi(s[idx+1:])
	return s[:idx], port
}

// ── Port / nat helpers ────────────────────────────────────────────────────────

// portsFromNat converts a nat.PortMap (from ContainerInspect) to the []types.Port
// slice expected by upsertContainerWithLabels / bestPort.
func portsFromNat(pm nat.PortMap) []container.Port {
	ports := make([]container.Port, 0, len(pm))
	for p, bindings := range pm {
		priv, err := strconv.ParseUint(p.Port(), 10, 16)
		if err != nil {
			continue
		}
		for _, b := range bindings {
			pub, err := strconv.ParseUint(b.HostPort, 10, 16)
			if err != nil {
				continue
			}
			ports = append(ports, container.Port{
				Type:        p.Proto(),
				PrivatePort: uint16(priv),
				PublicPort:  uint16(pub),
			})
		}
	}
	return ports
}

// preserveScheme keeps the scheme from oldTarget and replaces the rest with
// the host:port from newTarget. This ensures a user-corrected scheme (e.g.
// changing https→http) survives container restarts.
func preserveScheme(oldTarget, newTarget string) string {
	if before, _, ok := strings.Cut(oldTarget, "://"); ok {
		if j := strings.Index(newTarget, "://"); j >= 0 {
			return before + newTarget[j:]
		}
	}
	return newTarget
}

// ── String helpers ────────────────────────────────────────────────────────────

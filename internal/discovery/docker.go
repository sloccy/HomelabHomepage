package discovery

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"lantern/internal/store"
)

// ── Docker API types ----------------------------------------------------------

type dockerPort struct {
	Type        string `json:"Type"`
	PublicPort  int    `json:"PublicPort"`
}

type dockerContainer struct {
	ID     string            `json:"Id"`
	Names  []string          `json:"Names"`
	Ports  []dockerPort      `json:"Ports"`
	Labels map[string]string `json:"Labels"`
}

type dockerEvent struct {
	Action string      `json:"Action"`
	Actor  dockerActor `json:"Actor"`
}

type dockerActor struct {
	ID string `json:"ID"`
}

// ── Docker HTTP client --------------------------------------------------------

type dockerClient struct {
	hc   *http.Client
	base string
}

func newDockerClient() (*dockerClient, error) {
	socketPath := "/var/run/docker.sock"
	if host := os.Getenv("DOCKER_HOST"); strings.HasPrefix(host, "unix://") {
		socketPath = strings.TrimPrefix(host, "unix://")
	}
	if _, err := os.Stat(socketPath); err != nil {
		return nil, fmt.Errorf("Docker socket not available at %s: %w", socketPath, err)
	}
	hc := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
			},
		},
	}
	return &dockerClient{hc: hc, base: "http://localhost"}, nil
}

// listContainers returns all running containers.
func (c *dockerClient) listContainers(ctx context.Context) ([]dockerContainer, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/containers/json", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var containers []dockerContainer
	if err := json.NewDecoder(resp.Body).Decode(&containers); err != nil {
		return nil, err
	}
	return containers, nil
}

// events returns channels for container events and errors.
// The goroutine exits when ctx is cancelled or the stream closes.
func (c *dockerClient) events(ctx context.Context) (<-chan dockerEvent, <-chan error) {
	msgCh := make(chan dockerEvent, 16)
	errCh := make(chan error, 1)
	go func() {
		defer close(msgCh)
		params := url.Values{"filters": {`{"type":["container"]}`}}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			c.base+"/events?"+params.Encode(), nil)
		if err != nil {
			errCh <- err
			return
		}
		resp, err := c.hc.Do(req)
		if err != nil {
			errCh <- err
			return
		}
		defer resp.Body.Close()
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Bytes()
			if len(line) == 0 {
				continue
			}
			var ev dockerEvent
			if err := json.Unmarshal(line, &ev); err != nil {
				continue
			}
			select {
			case msgCh <- ev:
			case <-ctx.Done():
				return
			}
		}
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			errCh <- err
		}
	}()
	return msgCh, errCh
}

// ── DockerWatch ---------------------------------------------------------------

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
	dc, err := newDockerClient()
	if err != nil {
		log.Printf("discovery: Docker socket unavailable (%v) — skipping Docker discovery", err)
		return
	}

	d.syncContainers(ctx, dc)

	msgCh, errCh := dc.events(ctx)

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
			time.Sleep(10 * time.Second)
			msgCh, errCh = dc.events(ctx)
		case msg, ok := <-msgCh:
			if !ok {
				return
			}
			d.handleDockerEvent(ctx, dc, msg)
		}
	}
}

func (d *Discoverer) syncContainers(ctx context.Context, dc *dockerClient) {
	containers, err := dc.listContainers(ctx)
	if err != nil {
		log.Printf("discovery: list containers: %v", err)
		return
	}
	for _, c := range containers {
		name := containerName(c.Names)
		if name != "" {
			d.upsertContainerWithLabels(ctx, c.ID, name, c.Ports, c.Labels)
		}
	}
	log.Printf("discovery: synced %d running containers", len(containers))
}

func (d *Discoverer) handleDockerEvent(ctx context.Context, dc *dockerClient, msg dockerEvent) {
	switch msg.Action {
	case "start":
		time.Sleep(2 * time.Second) // brief delay for container to fully start
		containers, err := dc.listContainers(ctx)
		if err != nil {
			return
		}
		for _, c := range containers {
			if c.ID == msg.Actor.ID || strings.HasPrefix(c.ID, msg.Actor.ID) {
				name := containerName(c.Names)
				if name != "" {
					d.upsertContainerWithLabels(ctx, c.ID, name, c.Ports, c.Labels)
				}
				return
			}
		}

	case "die", "stop", "destroy", "kill":
		d.detachContainer(ctx, msg.Actor.ID)
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
func (d *Discoverer) detachContainer(ctx context.Context, id string) {
	d.store.ClearContainerID(id)
	d.store.RemoveDiscoveredByContainerID(id)
	_ = d.store.Save()
}

// upsertContainerWithLabels resolves a container's configuration from Docker labels,
// then creates or updates the service entry.
func (d *Discoverer) upsertContainerWithLabels(ctx context.Context, id, name string, ports []dockerPort, labels map[string]string) {
	if name == "" || name == "lantern" {
		return
	}
	// lantern.enable=false → opt out.
	if labels["lantern.enable"] == "false" {
		return
	}
	// Skip if already tracked by this exact container ID.
	if d.store.GetServiceByContainerID(id) != nil {
		return
	}

	info := d.resolveContainer(name, ports, labels)
	if info == nil {
		return // no usable port/target
	}

	// Reattach: same container name as an existing docker service (e.g. restart/recreate).
	if existing := d.store.GetServiceByContainerName(name); existing != nil {
		existing.ContainerID = id
		existing.Target = info.target
		_ = d.store.Save()
		log.Printf("discovery: reattached %q → %s (%s)", name, existing.Subdomain, id)
		return
	}

	// Subdomain collision: if the existing service is docker-sourced, reattach
	// (handles pre-fix records with no ContainerName set, and renamed containers).
	// Only send to discovered if it's a manual/network service with the same subdomain.
	if existing := d.store.GetServiceBySubdomain(info.subdomain); existing != nil {
		if existing.Source == "docker" {
			existing.ContainerID = id
			existing.ContainerName = name // backfill for pre-fix records
			existing.Target = info.target
			_ = d.store.Save()
			log.Printf("discovery: reattached %q → %s (%s)", name, existing.Subdomain, id)
			return
		}
		d.addDockerDiscovered(id, info.name, info.target, info.subdomain)
		return
	}

	// New container — send to Discovered for user review instead of auto-assigning.
	d.addDockerDiscovered(id, info.name, info.target, info.subdomain)
}

// resolveContainer determines the display name, subdomain and target URL for a container
// by checking labels in priority order:
//  1. lantern.* labels
//  2. Traefik v2/v3 labels
//  3. Published ports (bestPort heuristic)
func (d *Discoverer) resolveContainer(name string, ports []dockerPort, labels map[string]string) *containerInfo {
	info := &containerInfo{
		name:      name,
		subdomain: sanitiseSubdomain(name),
	}

	// Display name override.
	if n := labels["lantern.name"]; n != "" {
		info.name = n
	}

	// Explicit target URL — takes full precedence over port logic.
	if u := labels["lantern.url"]; u != "" {
		info.target = u
		if s := labels["lantern.subdomain"]; s != "" {
			info.subdomain = sanitiseSubdomain(s)
		}
		return info
	}

	// Subdomain: lantern label > traefik rule > container name.
	if s := labels["lantern.subdomain"]; s != "" {
		info.subdomain = sanitiseSubdomain(s)
	} else if sub := traefikSubdomain(labels, d.cfg.Domain); sub != "" {
		info.subdomain = sub
	}

	// Port: lantern.port > traefik service port > bestPort(published).
	port := 0
	if p := labels["lantern.port"]; p != "" {
		fmt.Sscanf(p, "%d", &port)
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

	// Scheme: explicit label > port heuristic.
	scheme := "http"
	if s := labels["lantern.scheme"]; s == "https" || s == "http" {
		scheme = s
	} else if port == 443 || port == 8443 || port == 9443 {
		scheme = "https"
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
		ID:                 newID(),
		IP:                 ip,
		Port:               port,
		Title:              name,
		Source:             "docker",
		ContainerName:      name,
		ContainerID:        id,
		SuggestedSubdomain: suggestedSub,
		DiscoveredAt:       time.Now(),
	}
	d.store.AddDiscovered(disc)
	_ = d.store.Save()

	go func(id, target string) {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		icon := FetchFaviconForTarget(ctx, target)
		if icon == "" {
			return
		}
		d.store.UpdateDiscoveredIcon(id, icon)
		_ = d.store.Save()
	}(disc.ID, target)
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
		return sanitiseSubdomain(host)
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
		var port int
		fmt.Sscanf(v, "%d", &port)
		return port
	}
	return 0
}

// ── Port / target helpers ─────────────────────────────────────────────────────

// bestPort picks the most useful published TCP port, preferring common web UI ports.
func bestPort(ports []dockerPort) int {
	preferred := []int{80, 8080, 3000, 5000, 9443, 9000, 8096, 8123, 443, 8443, 8000}
	portSet := make(map[int]bool)
	for _, p := range ports {
		if p.Type == "tcp" && p.PublicPort > 0 {
			portSet[p.PublicPort] = true
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
			return p.PublicPort
		}
	}
	return 0
}

// splitTarget splits "http://ip:port" into (ip, port).
func splitTarget(target string) (string, int) {
	s := strings.TrimPrefix(target, "http://")
	s = strings.TrimPrefix(s, "https://")
	s = strings.TrimSuffix(s, "/")
	idx := strings.LastIndexByte(s, ':')
	if idx < 0 {
		return s, 0
	}
	var port int
	fmt.Sscanf(s[idx+1:], "%d", &port)
	return s[:idx], port
}

// ── String helpers ────────────────────────────────────────────────────────────

func containerName(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.TrimPrefix(names[0], "/")
}

func sanitiseSubdomain(name string) string {
	name = strings.ToLower(name)
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else if r == '_' || r == '.' || r == ' ' {
			b.WriteRune('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "service"
	}
	return s
}

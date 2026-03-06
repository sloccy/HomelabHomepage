package discovery

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	dockerclient "github.com/docker/docker/client"

	"lantern/internal/store"
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
	cli, err := dockerclient.NewClientWithOpts(
		dockerclient.FromEnv,
		dockerclient.WithAPIVersionNegotiation(),
	)
	if err != nil {
		log.Printf("discovery: Docker socket unavailable (%v) — skipping Docker discovery", err)
		return
	}
	defer cli.Close()

	d.syncContainers(ctx, cli)

	f := filters.NewArgs()
	f.Add("type", "container")
	msgCh, errCh := cli.Events(ctx, events.ListOptions{Filters: f})

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
			msgCh, errCh = cli.Events(ctx, events.ListOptions{Filters: f})
		case msg := <-msgCh:
			d.handleDockerEvent(ctx, cli, msg)
		}
	}
}

func (d *Discoverer) syncContainers(ctx context.Context, cli *dockerclient.Client) {
	containers, err := cli.ContainerList(ctx, container.ListOptions{})
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

func (d *Discoverer) handleDockerEvent(ctx context.Context, cli *dockerclient.Client, msg events.Message) {
	switch msg.Action {
	case "start":
		time.Sleep(2 * time.Second) // brief delay for container to fully start
		containers, err := cli.ContainerList(ctx, container.ListOptions{})
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
func (d *Discoverer) upsertContainerWithLabels(ctx context.Context, id, name string, ports []container.Port, labels map[string]string) {
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
		d.addDockerDiscovered(id, info.name, info.target)
		return
	}

	svc := &store.Service{
		ID:            newID(),
		Name:          info.name,
		Subdomain:     info.subdomain,
		Target:        info.target,
		Source:        "docker",
		ContainerID:   id,
		ContainerName: name,
		CreatedAt:     time.Now(),
	}

	hostname := info.subdomain + "." + d.cfg.Domain
	if labels["lantern.tunnel"] == "true" && d.cf.TunnelEnabled() {
		cnameID, err := d.cf.AddTunnelRoute(ctx, hostname, info.target)
		if err != nil {
			log.Printf("discovery: add tunnel route for %s: %v", info.subdomain, err)
		} else {
			svc.DNSRecordID = cnameID
			svc.TunnelRouteID = hostname
		}
	} else if d.cfg.ServerIP != "" {
		dnsID, err := d.cf.CreateRecord(ctx, hostname, d.cfg.ServerIP)
		if err != nil {
			log.Printf("discovery: create DNS for %s: %v", info.subdomain, err)
		} else {
			svc.DNSRecordID = dnsID
		}
	}

	d.store.AddService(svc)
	if err := d.store.Save(); err != nil {
		log.Printf("discovery: save store: %v", err)
	}
	log.Printf("discovery: auto-assigned %q → %s.%s (%s)", info.name, info.subdomain, d.cfg.Domain, info.target)
}

// resolveContainer determines the display name, subdomain and target URL for a container
// by checking labels in priority order:
//  1. lantern.* labels
//  2. Traefik v2/v3 labels
//  3. Published ports (bestPort heuristic)
func (d *Discoverer) resolveContainer(name string, ports []container.Port, labels map[string]string) *containerInfo {
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

func (d *Discoverer) addDockerDiscovered(id, name, target string) {
	if d.store.GetDiscoveredByContainerID(id) != nil {
		return
	}
	ip, port := splitTarget(target)
	disc := &store.DiscoveredService{
		ID:            newID(),
		IP:            ip,
		Port:          port,
		Title:         name,
		Source:        "docker",
		ContainerName: name,
		ContainerID:   id,
		DiscoveredAt:  time.Now(),
	}
	d.store.AddDiscovered(disc)
	_ = d.store.Save()
}

func (d *Discoverer) removeContainer(ctx context.Context, id string) {
	if svc := d.store.GetServiceByContainerID(id); svc != nil {
		_, dnsID, tunnelRoute := d.store.DeleteService(svc.ID)
		if tunnelRoute != "" {
			if err := d.cf.RemoveTunnelRoute(ctx, tunnelRoute, dnsID); err != nil {
				log.Printf("discovery: remove tunnel route for %s: %v", svc.Subdomain, err)
			}
		} else if dnsID != "" {
			if err := d.cf.DeleteRecord(ctx, dnsID); err != nil {
				log.Printf("discovery: delete DNS for %s: %v", svc.Subdomain, err)
			}
		}
		_ = d.store.Save()
		log.Printf("discovery: removed container service %q", svc.Name)
		return
	}
	d.store.RemoveDiscoveredByContainerID(id)
	_ = d.store.Save()
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
func bestPort(ports []container.Port) int {
	preferred := []uint16{80, 8080, 3000, 5000, 9443, 9000, 8096, 8123, 443, 8443, 8000}
	portSet := make(map[uint16]bool)
	for _, p := range ports {
		if p.Type == "tcp" && p.PublicPort > 0 {
			portSet[p.PublicPort] = true
		}
	}
	for _, pp := range preferred {
		if portSet[pp] {
			return int(pp)
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

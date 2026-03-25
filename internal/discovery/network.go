package discovery

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lantern/internal/util"
)

var (
	httpClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			DisableKeepAlives:   true,
			IdleConnTimeout:     5 * time.Second,
			TLSHandshakeTimeout: 3 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}

	reTitleTag        = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reFaviconHref     = regexp.MustCompile(`(?i)<link[^>]+rel=["'][^"']*icon[^"']*["'][^>]+href=["']([^"']+)["']`)
	reFaviconHref2    = regexp.MustCompile(`(?i)<link[^>]+href=["']([^"']+)["'][^>]+rel=["'][^"']*icon[^"']*["']`)
	reAppleTouchIcon  = regexp.MustCompile(`(?i)<link[^>]+rel=["']apple-touch-icon["'][^>]+href=["']([^"']+)["']`)
	reAppleTouchIcon2 = regexp.MustCompile(`(?i)<link[^>]+href=["']([^"']+)["'][^>]+rel=["']apple-touch-icon["']`)
)

// allPorts is the full 1-65535 range used during TCP sweeps.
// Initialised once at startup to avoid a 512 KB allocation per scan.
var allPorts = func() []int {
	p := make([]int, 65535)
	for i := range p {
		p[i] = i + 1
	}
	return p
}()

// resolveURLToPort parses a URL and resolves the host to an IP, returning the
// resulting openPort. Used by SSDP and WS-Discovery to normalise device addresses.
func resolveURLToPort(rawURL string) (openPort, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return openPort{}, false
	}
	host := u.Hostname()
	if net.ParseIP(host) == nil {
		addrs, err := net.LookupHost(host)
		if err != nil || len(addrs) == 0 {
			return openPort{}, false
		}
		host = addrs[0]
	}
	port := 80
	if p := u.Port(); p != "" {
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		}
	} else if strings.EqualFold(u.Scheme, "https") {
		port = 443
	}
	return openPort{ip: host, port: port}, true
}

// ── Stage 3: Fingerprint engine ───────────────────────────────────────────────

type signature struct {
	name       string
	confidence float32
	icon       string
	match      func(h http.Header, body, title string) bool
}

var signatures = []signature{
	// ── Infrastructure ───────────────────────────────────────────────────────
	{"Proxmox VE", 0.99, "🖥️", func(h http.Header, b, _ string) bool {
		return strings.Contains(h.Get("Server"), "pve-api-daemon") ||
			strings.Contains(b, "Proxmox Virtual Environment")
	}},
	{"Cockpit", 0.92, "🖥️", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Cockpit") || strings.Contains(b, "cockpit")
	}},
	{"Webmin", 0.90, "🖥️", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Webmin") || strings.Contains(b, "webmin")
	}},
	{"Synology DSM", 0.95, "💾", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Synology") ||
			strings.Contains(b, "Synology DiskStation") || strings.Contains(b, "SYNO.")
	}},
	{"TrueNAS", 0.95, "💾", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "TrueNAS") || strings.Contains(b, "TrueNAS")
	}},
	{"UniFi", 0.92, "📡", func(_ http.Header, b, t string) bool {
		return strings.Contains(b, "UniFi") || strings.Contains(t, "UniFi")
	}},
	{"OpenWrt", 0.92, "📡", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "OpenWrt") || strings.Contains(b, "OpenWrt")
	}},

	// ── Monitoring / Observability ───────────────────────────────────────────
	{"Grafana", 0.99, "📊", func(h http.Header, b, t string) bool {
		return h.Get("X-Grafana-Version") != "" ||
			strings.Contains(t, "Grafana") || strings.Contains(b, "grafana")
	}},
	{"Prometheus", 0.95, "📈", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Prometheus") || strings.Contains(b, "prometheus_")
	}},
	{"Netdata", 0.99, "📉", func(h http.Header, b, t string) bool {
		return h.Get("X-Netdata-Version") != "" ||
			strings.Contains(b, "netdataRoot") || strings.Contains(t, "Netdata")
	}},
	{"Uptime Kuma", 0.95, "🟢", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Uptime Kuma") || strings.Contains(b, "uptimekuma")
	}},
	{"Scrutiny", 0.95, "💾", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Scrutiny") || strings.Contains(b, "scrutiny")
	}},
	{"Healthchecks", 0.90, "✅", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Healthchecks") || strings.Contains(b, "healthchecks")
	}},
	{"Dozzle", 0.95, "📋", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Dozzle") || strings.Contains(b, "dozzle")
	}},

	// ── Container management ─────────────────────────────────────────────────
	{"Portainer", 0.95, "🐳", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Portainer") || strings.Contains(b, "Portainer")
	}},
	{"Yacht", 0.90, "⛵", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Yacht") || strings.Contains(b, "yacht-app")
	}},

	// ── Reverse proxies ──────────────────────────────────────────────────────
	{"Nginx Proxy Manager", 0.95, "🔀", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Nginx Proxy Manager") || strings.Contains(b, "Nginx Proxy Manager")
	}},
	{"Traefik", 0.92, "🔀", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Traefik") || strings.Contains(b, "traefik")
	}},

	// ── Home automation ──────────────────────────────────────────────────────
	{"Home Assistant", 0.95, "🏠", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Home Assistant") || strings.Contains(b, "Home Assistant")
	}},
	{"Node-RED", 0.97, "🔴", func(h http.Header, b, t string) bool {
		return strings.Contains(h.Get("X-Powered-By"), "node-red") ||
			strings.Contains(t, "Node-RED") || strings.Contains(b, "node-red")
	}},
	{"Frigate", 0.95, "📹", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Frigate") || strings.Contains(b, "Frigate NVR")
	}},
	{"Zigbee2MQTT", 0.90, "📡", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Zigbee2MQTT") || strings.Contains(b, "zigbee2mqtt")
	}},

	// ── Media servers ────────────────────────────────────────────────────────
	{"Jellyfin", 0.99, "🎬", func(h http.Header, b, t string) bool {
		return h.Get("X-Jellyfin-Version") != "" ||
			strings.Contains(b, "Jellyfin") || strings.Contains(t, "Jellyfin")
	}},
	{"Plex", 0.99, "🎬", func(h http.Header, b, t string) bool {
		return h.Get("X-Plex-Protocol") != "" ||
			strings.Contains(b, "Plex Media Server") || strings.Contains(t, "Plex")
	}},
	{"Emby", 0.95, "🎬", func(h http.Header, b, t string) bool {
		return h.Get("X-Emby-Server-Id") != "" ||
			strings.Contains(t, "Emby") || strings.Contains(b, "Emby Server")
	}},
	{"Navidrome", 0.95, "🎵", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Navidrome") || strings.Contains(b, "Navidrome")
	}},
	{"Audiobookshelf", 0.95, "🎧", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Audiobookshelf") || strings.Contains(b, "audiobookshelf")
	}},

	// ── Media request / management ───────────────────────────────────────────
	{"Overseerr", 0.95, "🎭", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Overseerr") || strings.Contains(b, "Overseerr")
	}},
	{"Jellyseerr", 0.95, "🎭", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Jellyseerr") || strings.Contains(b, "Jellyseerr")
	}},
	{"Ombi", 0.90, "🎭", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Ombi") || strings.Contains(b, "Ombi")
	}},
	{"Tautulli", 0.95, "📊", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Tautulli") || strings.Contains(b, "tautulli")
	}},

	// ── *arr suite ───────────────────────────────────────────────────────────
	{"Sonarr", 0.95, "📺", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Sonarr") || strings.Contains(b, "Sonarr")
	}},
	{"Radarr", 0.95, "🎥", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Radarr") || strings.Contains(b, "Radarr")
	}},
	{"Lidarr", 0.92, "🎵", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Lidarr") || strings.Contains(b, "Lidarr")
	}},
	{"Readarr", 0.92, "📚", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Readarr") || strings.Contains(b, "Readarr")
	}},
	{"Prowlarr", 0.95, "🔍", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Prowlarr") || strings.Contains(b, "Prowlarr")
	}},
	{"Bazarr", 0.95, "💬", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Bazarr") || strings.Contains(b, "Bazarr")
	}},
	{"Jackett", 0.95, "🔍", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Jackett") || strings.Contains(b, "Jackett")
	}},

	// ── Download clients ─────────────────────────────────────────────────────
	{"Transmission", 0.92, "⬇️", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Transmission") || strings.Contains(b, "Transmission Web")
	}},
	{"qBittorrent", 0.92, "⬇️", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "qBittorrent") || strings.Contains(b, "qBittorrent")
	}},
	{"Deluge", 0.90, "⬇️", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Deluge") || strings.Contains(b, "Deluge Web")
	}},
	{"ruTorrent", 0.90, "⬇️", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "ruTorrent") || strings.Contains(b, "ruTorrent")
	}},
	{"Flood", 0.90, "⬇️", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Flood") || strings.Contains(b, "flood-app")
	}},
	{"SABnzbd", 0.95, "📥", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "SABnzbd") || strings.Contains(b, "sabnzbd")
	}},
	{"NZBGet", 0.95, "📥", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "NZBGet") || strings.Contains(b, "nzbget")
	}},

	// ── Git / CI ─────────────────────────────────────────────────────────────
	{"Gitea", 0.99, "🦊", func(h http.Header, b, t string) bool {
		return h.Get("X-Gitea-Version") != "" ||
			strings.Contains(t, "Gitea") || strings.Contains(b, "Gitea")
	}},
	{"Forgejo", 0.99, "🦊", func(h http.Header, b, t string) bool {
		return h.Get("X-Forgejo-Version") != "" ||
			strings.Contains(t, "Forgejo") || strings.Contains(b, "Forgejo")
	}},
	{"Woodpecker CI", 0.90, "🚀", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Woodpecker") || strings.Contains(b, "woodpecker-ci")
	}},
	{"Drone CI", 0.90, "🚀", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Drone") || strings.Contains(b, "drone.io")
	}},
	{"Harbor", 0.92, "🗃️", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Harbor") || strings.Contains(b, "harbor-app")
	}},

	// ── Auth / Identity ──────────────────────────────────────────────────────
	{"Vaultwarden", 0.95, "🔐", func(_ http.Header, b, t string) bool {
		return strings.Contains(b, "Vaultwarden") ||
			strings.Contains(t, "Vaultwarden") || strings.Contains(b, "bitwarden")
	}},
	{"Authelia", 0.95, "🔐", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Authelia") || strings.Contains(b, "Authelia")
	}},
	{"Authentik", 0.97, "🔐", func(h http.Header, b, t string) bool {
		return strings.Contains(h.Get("X-Powered-By"), "authentik") ||
			strings.Contains(b, "ak-flow") || strings.Contains(t, "authentik")
	}},
	{"Keycloak", 0.95, "🔑", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Keycloak") || strings.Contains(b, "Keycloak")
	}},
	{"HashiCorp Vault", 0.97, "🔒", func(h http.Header, b, t string) bool {
		return h.Get("X-Vault-Request") != "" ||
			strings.Contains(b, "hashicorp-vault") || strings.Contains(t, "Vault")
	}},

	// ── Networking / VPN ─────────────────────────────────────────────────────
	{"Pi-hole", 0.95, "🕳️", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Pi-hole") || strings.Contains(b, "Pi-hole")
	}},
	{"AdGuard Home", 0.95, "🛡️", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "AdGuard") || strings.Contains(b, "AdGuard Home")
	}},
	{"Technitium DNS", 0.95, "🌐", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Technitium") || strings.Contains(b, "TechnitiumDNS")
	}},
	{"WireGuard Easy", 0.95, "🔒", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "WG Easy") || strings.Contains(b, "wg-easy")
	}},
	{"Headscale", 0.90, "🔒", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Headscale") || strings.Contains(b, "headscale")
	}},

	// ── Notifications ────────────────────────────────────────────────────────
	{"Gotify", 0.95, "🔔", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Gotify") || strings.Contains(b, "gotify")
	}},
	{"ntfy", 0.95, "🔔", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "ntfy") || strings.Contains(b, "ntfy.sh")
	}},

	// ── Photos / Files ───────────────────────────────────────────────────────
	{"Immich", 0.95, "📷", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Immich") || strings.Contains(b, "Immich")
	}},
	{"PhotoPrism", 0.95, "📸", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "PhotoPrism") || strings.Contains(b, "PhotoPrism")
	}},
	{"Nextcloud", 0.97, "☁️", func(h http.Header, b, t string) bool {
		return h.Get("X-Nextcloud-Request-ID") != "" ||
			strings.Contains(b, "Nextcloud") || strings.Contains(t, "Nextcloud")
	}},
	{"Syncthing", 0.97, "🔄", func(h http.Header, b, t string) bool {
		return h.Get("X-Syncthing-Id") != "" ||
			strings.Contains(t, "Syncthing") || strings.Contains(b, "syncthing")
	}},
	{"MinIO", 0.95, "🗄️", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "MinIO") || strings.Contains(b, "minio")
	}},
	{"Seafile", 0.90, "🗄️", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Seafile") || strings.Contains(b, "seafile")
	}},

	// ── Reading / Documents ──────────────────────────────────────────────────
	{"Calibre-Web", 0.95, "📚", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Calibre-Web") || strings.Contains(b, "calibre-web")
	}},
	{"Komga", 0.95, "📚", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Komga") || strings.Contains(b, "Komga")
	}},
	{"Kavita", 0.95, "📚", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Kavita") || strings.Contains(b, "kavita")
	}},
	{"Paperless-ngx", 0.95, "📄", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Paperless") || strings.Contains(b, "paperless")
	}},
	{"Stirling-PDF", 0.95, "📄", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Stirling") || strings.Contains(b, "stirling-pdf")
	}},
	{"BookStack", 0.90, "📖", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "BookStack") || strings.Contains(b, "BookStack")
	}},
	{"Wallabag", 0.90, "📰", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Wallabag") || strings.Contains(b, "wallabag")
	}},
	{"FreshRSS", 0.90, "📰", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "FreshRSS") || strings.Contains(b, "FreshRSS")
	}},
	{"Miniflux", 0.90, "📰", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Miniflux") || strings.Contains(b, "miniflux")
	}},

	// ── Homelab dashboards ───────────────────────────────────────────────────
	{"Homarr", 0.95, "🏠", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Homarr") || strings.Contains(b, "Homarr")
	}},
	{"Homer", 0.90, "🏠", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Homer") || strings.Contains(b, "homer-app")
	}},
	{"Flame", 0.90, "🔥", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Flame") || strings.Contains(b, "flame-app")
	}},
	{"Dashy", 0.90, "📊", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Dashy") || strings.Contains(b, "dashy")
	}},
	{"Heimdall", 0.90, "🛡️", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Heimdall") || strings.Contains(b, "heimdall")
	}},
	{"Organizr", 0.90, "📁", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Organizr") || strings.Contains(b, "organizr")
	}},

	// ── Food / Life ──────────────────────────────────────────────────────────
	{"Mealie", 0.95, "🍽️", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Mealie") || strings.Contains(b, "mealie")
	}},
	{"Grocy", 0.95, "🛒", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "grocy") || strings.Contains(b, "grocy")
	}},
	{"Tandoor", 0.90, "🍽️", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Tandoor") || strings.Contains(b, "tandoor")
	}},

	// ── Bookmarks / Links ────────────────────────────────────────────────────
	{"Linkding", 0.95, "🔗", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "linkding") || strings.Contains(b, "linkding")
	}},
	{"Shlink", 0.90, "🔗", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Shlink") || strings.Contains(b, "shlink")
	}},

	// ── Automation / Workflow ────────────────────────────────────────────────
	{"n8n", 0.95, "⚙️", func(_ http.Header, b, t string) bool {
		return strings.EqualFold(t, "n8n") || strings.Contains(b, "\"n8n\"")
	}},
	{"Changedetection.io", 0.95, "👁️", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "changedetection") || strings.Contains(b, "changedetection")
	}},

	// ── Remote access ────────────────────────────────────────────────────────
	{"Guacamole", 0.95, "🖥️", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Guacamole") || strings.Contains(b, "guacamole")
	}},

	// ── Communication / Social ───────────────────────────────────────────────
	{"Matrix Synapse", 0.95, "💬", func(h http.Header, b, t string) bool {
		return strings.Contains(h.Get("Server"), "Synapse") ||
			strings.Contains(b, "matrix-synapse") || strings.Contains(t, "Matrix")
	}},

	// ── Misc ─────────────────────────────────────────────────────────────────
	{"Verdaccio", 0.90, "📦", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Verdaccio") || strings.Contains(b, "verdaccio")
	}},
	{"IT Tools", 0.90, "🔧", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "IT Tools") || strings.Contains(b, "it-tools")
	}},
}

// fingerprint matches HTTP response data against known service signatures.
// Returns name, confidence (0–1) and an emoji icon, or empty strings if unknown.
func fingerprint(h http.Header, body, title string) (name string, confidence float32, icon string) {
	for _, sig := range signatures {
		if sig.match(h, body, title) {
			return sig.name, sig.confidence, sig.icon
		}
	}
	return "", 0, ""
}

// ── Stage 1: TCP connect sweep ────────────────────────────────────────────────

type openPort struct {
	ip   string
	port int
}

// tcpSweep checks which (ip, port) pairs accept a TCP connection.
// Uses 4096 workers with a configurable dial timeout — no data exchange.
// logf is called at 5% progress intervals with per-type error counts.
func tcpSweep(ctx context.Context, ips []string, ports []int, logf func(string, ...any), timeout time.Duration) []openPort {
	type job struct {
		ip   string
		port int
	}

	// ── Pre-sweep debug summary ───────────────────────────────────────────────
	start := time.Now()
	logf("[TCP] Starting sweep: %d hosts × %d ports = %d combinations", len(ips), len(ports), len(ips)*len(ports))
	logf("[TCP] Timeout: %v/conn, 4096 concurrent workers", timeout)
	if len(ips) == 0 {
		logf("[TCP] ERROR: no hosts to scan — check subnet config")
		return nil
	} else if len(ips) <= 30 {
		logf("[TCP] Hosts: %s", strings.Join(ips, ", "))
	} else {
		logf("[TCP] Hosts: %s … %s (%d total)", ips[0], ips[len(ips)-1], len(ips))
	}

	jobs := make(chan job, 1024)
	results := make(chan openPort, 256)

	total := int64(len(ips) * len(ports))
	var done atomic.Int64
	var countOpen, countTimeouts, countRefused, countOther atomic.Int64

	const workers = 4096
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					logf("[TCP] Context cancelled — stopping workers")
					return
				}
				addr := net.JoinHostPort(j.ip, strconv.Itoa(j.port))
				conn, err := net.DialTimeout("tcp", addr, timeout)
				if err == nil {
					conn.Close()
					countOpen.Add(1)
					results <- openPort{j.ip, j.port}
				} else {
					errStr := err.Error()
					switch {
					case strings.Contains(errStr, "i/o timeout") || strings.Contains(errStr, "timeout"):
						countTimeouts.Add(1)
					case strings.Contains(errStr, "connection refused"):
						countRefused.Add(1)
					default:
						countOther.Add(1)
						logf("[ERR] %s:%d → %v", j.ip, j.port, err)
					}
				}
				if total > 0 {
					n := done.Add(1)
					prev := (n - 1) * 100 / total
					curr := n * 100 / total
					if curr/25 > prev/25 && curr < 100 {
						logf("[TCP] %d%% (%d/%d) — open:%d refused:%d timeout:%d other:%d",
							(curr/25)*25, n, total,
							countOpen.Load(), countRefused.Load(), countTimeouts.Load(), countOther.Load())
					}
				}
			}
		}()
	}

	go func() {
		for _, ip := range ips {
			for _, port := range ports {
				jobs <- job{ip, port}
			}
		}
		close(jobs)
		wg.Wait()
		close(results)
	}()

	var open []openPort
	for op := range results {
		open = append(open, op)
	}

	elapsed := time.Since(start).Round(time.Millisecond)
	logf("[TCP] Done in %s: %d open, %d refused, %d timeout, %d other",
		elapsed, countOpen.Load(), countRefused.Load(), countTimeouts.Load(), countOther.Load())

	return open
}

// ── Subnet helpers ────────────────────────────────────────────────────────────

// getLocalSubnet returns the subnet of the first suitable non-loopback IPv4
// interface. Virtual interfaces created by Docker are skipped because they
// carry wide masks (/16) that would cause the scanner to probe tens of
// thousands of hosts. The result is capped at /24 as a safety net against
// accidentally scanning large cloud or VPN subnets.
// logf is called to report why each interface is skipped or selected.
func getLocalSubnet(logf func(string, ...any)) (*net.IPNet, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	logf("Interface scan: %d interfaces found", len(ifaces))
	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue // silently skip loopback
		}
		if iface.Flags&net.FlagUp == 0 {
			logf("  %s: skip (down)", iface.Name)
			continue
		}
		// Skip Docker bridge and virtual ethernet interfaces.
		n := iface.Name
		if n == "docker0" || strings.HasPrefix(n, "br-") || strings.HasPrefix(n, "veth") {
			logf("  %s: skip (Docker virtual interface)", iface.Name)
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			logf("  %s: skip (addrs error: %v)", iface.Name, err)
			continue
		}
		for _, addr := range addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok {
				continue
			}
			ip4 := ipnet.IP.To4()
			if ip4 == nil || ip4.IsLoopback() {
				continue
			}
			ones, bits := ipnet.Mask.Size()
			if ones == 0 || bits != 32 {
				logf("  %s: skip (non-IPv4 or /0 mask)", iface.Name)
				continue
			}
			// Cap at /24: don't scan more than 254 hosts when the interface
			// has a wide mask (e.g. /8 or /16 on cloud or VPN networks).
			if ones < 24 {
				logf("  %s: mask /%d is wider than /24, capping to /24", iface.Name, ones)
				ones = 24
			}
			_, subnet, _ := net.ParseCIDR(fmt.Sprintf("%s/%d", ip4, ones))
			logf("  %s: selected → %s", iface.Name, subnet)
			return subnet, nil
		}
		logf("  %s: skip (no suitable IPv4 address)", iface.Name)
	}
	return nil, fmt.Errorf("no suitable network interface found")
}

// generateIPs returns all host IPs (excluding network and broadcast) in subnet.
func generateIPs(subnet *net.IPNet) []string {
	var ips []string
	ip4 := subnet.IP.To4()
	if ip4 == nil {
		return nil
	}
	cur := make(net.IP, 4)
	copy(cur, ip4.Mask(subnet.Mask))
	incrementIP(cur)

	bcast := broadcastIP(subnet)
	for subnet.Contains(cur) && !cur.Equal(bcast) {
		dst := make(net.IP, 4)
		copy(dst, cur)
		ips = append(ips, dst.String())
		incrementIP(cur)
	}
	return ips
}

func incrementIP(ip net.IP) {
	for i := len(ip) - 1; i >= 0; i-- {
		ip[i]++
		if ip[i] != 0 {
			break
		}
	}
}

func broadcastIP(subnet *net.IPNet) net.IP {
	ip4 := subnet.IP.To4()
	bcast := make(net.IP, 4)
	for i := range bcast {
		bcast[i] = ip4[i] | ^subnet.Mask[i]
	}
	return bcast
}

// ── Stage 2 + 3: HTTP probe + fingerprint ─────────────────────────────────────

type probeResult struct {
	ip          string
	port        int
	url         string
	title       string
	icon        string // emoji fallback or "file" (when iconBytes is set)
	iconBytes   []byte // raw image bytes for icon file; nil when icon is emoji/empty
	serviceName string
	confidence  float32
}

// scanNetwork discovers services via four concurrent paths:
//  1. TCP sweep across configured subnets, then HTTP probe open ports.
//     An ARP pre-sweep (Linux + CAP_NET_RAW) narrows the host list first.
//  2. mDNS (DNS-SD) — finds services advertising on the local multicast group
//  3. SSDP — finds UPnP/DLNA devices via 239.255.255.250:1900
//  4. WS-Discovery — finds ONVIF cameras, printers, and Windows devices via
//     239.255.255.250:3702
//
// Results are streamed over the returned channel as they arrive.
func (d *Discoverer) scanNetwork(ctx context.Context, cidrs []string, withTCP bool) <-chan *probeResult {
	ch := make(chan *probeResult, 64)

	go func() {
		defer close(ch)

		// probedPorts deduplicates HTTP probes across all concurrent discovery
		// paths. If TCP sweep, mDNS, and SSDP all discover 10.0.0.5:80, it is
		// only probed once. Different ports on the same IP are each probed.
		var probedPorts sync.Map // key: "ip:port"
		probeOnce := func(op openPort) {
			key := fmt.Sprintf("%s:%d", op.ip, op.port)
			if _, loaded := probedPorts.LoadOrStore(key, struct{}{}); loaded {
				return
			}
			if r := probeHTTP(ctx, op.ip, op.port); r != nil {
				ch <- r
			}
		}

		var outerWg sync.WaitGroup

		// ── Path 1: TCP sweep → HTTP probe (manual full scan only) ──────────
		if withTCP {
			outerWg.Add(1)
			go func() {
				defer outerWg.Done()

				// Parse CIDRs or fall back to auto-detect.
				var nets []*net.IPNet
				if len(cidrs) == 0 {
					subnet, err := getLocalSubnet(d.logf)
					if err != nil {
						d.logf("Failed to detect local subnet: %v", err)
						return
					}
					nets = []*net.IPNet{subnet}
				} else {
					for _, cidr := range cidrs {
						_, ipnet, err := net.ParseCIDR(cidr)
						if err != nil {
							d.logf("Invalid subnet %q: %v", cidr, err)
							continue
						}
						d.logf("[SCAN] Parsed subnet: %s", ipnet.String())
						nets = append(nets, ipnet)
					}
					if len(nets) == 0 {
						d.logf("[SCAN] ERROR: no valid subnets to scan")
						return
					}
				}

				seen := make(map[string]bool)
				var ips []string
				for _, subnet := range nets {
					hosts := generateIPs(subnet)
					d.logf("[SCAN] Subnet %s: %d hosts generated", subnet, len(hosts))
					if len(hosts) <= 20 {
						d.logf("[SCAN] IP list: %s", strings.Join(hosts, ", "))
					}
					for _, ip := range hosts {
						if !seen[ip] {
							seen[ip] = true
							ips = append(ips, ip)
						}
					}
				}

				// ARP pre-sweep: quickly find live hosts before the full TCP sweep.
				// On Linux with CAP_NET_RAW this can dramatically reduce scan time
				// by skipping 750 ms timeouts for dead IPs.
				if liveHosts := arpSweep(ctx, ips, time.Second); len(liveHosts) > 0 {
					var alive []string
					for _, ip := range ips {
						if liveHosts[ip] {
							alive = append(alive, ip)
						}
					}
					d.logf("[ARP] %d/%d hosts alive: %s", len(alive), len(ips), strings.Join(alive, ", "))
					ips = alive
				} else {
					d.logf("[ARP] No live hosts via ARP (unavailable or all dead) — scanning all %d IPs", len(ips))
				}

				d.logf("[TCP] Handing off to tcpSweep: %d hosts × %d ports", len(ips), len(allPorts))
				open := tcpSweep(ctx, ips, allPorts, d.logf, d.cfg.ScanTimeout)
				d.logf("[TCP] tcpSweep returned: %d open ports total", len(open))
				for _, op := range open {
					d.logf("[TCP]   → %s:%d", op.ip, op.port)
				}

				if len(open) == 0 || ctx.Err() != nil {
					return
				}

				d.logf("HTTP probing %d open ports…", len(open))
				jobs := make(chan openPort, len(open))
				for _, op := range open {
					jobs <- op
				}
				close(jobs)

				const httpWorkers = 80
				var wg sync.WaitGroup
				for i := 0; i < httpWorkers; i++ {
					wg.Add(1)
					go func() {
						defer wg.Done()
						for op := range jobs {
							if ctx.Err() != nil {
								return
							}
							scheme := "http"
							if util.IsHTTPSPort(op.port) {
								scheme = "https"
							}
							d.logf("[HTTP] Probing %s://%s:%d/", scheme, op.ip, op.port)
							probeOnce(op)
						}
					}()
				}
				wg.Wait()
			}()
		}

		// ── Path 2: mDNS (DNS-SD) ────────────────────────────────────────────
		outerWg.Add(1)
		go func() {
			defer outerWg.Done()
			for _, op := range discoverMDNS(ctx, 4*time.Second) {
				if ctx.Err() != nil {
					return
				}
				probeOnce(op)
			}
		}()

		// ── Path 3: SSDP (UPnP) ─────────────────────────────────────────────
		outerWg.Add(1)
		go func() {
			defer outerWg.Done()
			for _, op := range discoverSSDP(ctx, 4*time.Second) {
				if ctx.Err() != nil {
					return
				}
				probeOnce(op)
			}
		}()

		// ── Path 4: WS-Discovery (ONVIF cameras, printers, Windows) ─────────
		outerWg.Add(1)
		go func() {
			defer outerWg.Done()
			for _, op := range discoverWSDiscovery(ctx, 4*time.Second) {
				if ctx.Err() != nil {
					return
				}
				probeOnce(op)
			}
		}()

		outerWg.Wait()
	}()

	return ch
}

func probeHTTP(ctx context.Context, ip string, port int) *probeResult {
	scheme := "http"
	if util.IsHTTPSPort(port) {
		scheme = "https"
	}
	rawURL := fmt.Sprintf("%s://%s:%d/", scheme, ip, port)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()

	// Skip responses that indicate no real service at this address.
	if resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound {
		return nil
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil
	}
	bodyStr := string(body)

	title := extractTitle(bodyStr)
	if title == "" {
		// Try reverse DNS before falling back to the raw IP address.
		if names, err := net.DefaultResolver.LookupAddr(ctx, ip); err == nil && len(names) > 0 {
			title = strings.TrimSuffix(names[0], ".")
		} else {
			title = ip
		}
	}

	// Stage 3: Fingerprint using headers + body + title.
	svcName, confidence, svcIcon := fingerprint(resp.Header, bodyStr, title)

	// Always try to fetch a real favicon; use fingerprint emoji only as fallback.
	iconData := fetchFaviconBytes(ctx, extractFaviconURL(bodyStr, rawURL))
	iconStr := svcIcon // emoji fallback
	if len(iconData) > 0 {
		iconStr = "file"
	}

	return &probeResult{
		ip:          ip,
		port:        port,
		url:         rawURL,
		title:       title,
		icon:        iconStr,
		iconBytes:   iconData,
		serviceName: svcName,
		confidence:  confidence,
	}
}

// ── HTML helpers ──────────────────────────────────────────────────────────────

func extractTitle(html string) string {
	m := reTitleTag.FindStringSubmatch(html)
	if len(m) < 2 {
		return ""
	}
	t := strings.TrimSpace(m[1])
	t = strings.Join(strings.Fields(t), " ")
	if len(t) > 80 {
		t = t[:80]
	}
	return t
}

func extractFaviconURL(html, baseURL string) string {
	// Prefer apple-touch-icon: always a high-quality PNG, no ICO format artifacts.
	for _, pair := range [][2]*regexp.Regexp{
		{reAppleTouchIcon, reAppleTouchIcon2},
		{reFaviconHref, reFaviconHref2},
	} {
		for _, re := range pair {
			if m := re.FindStringSubmatch(html); len(m) >= 2 {
				return resolveRef(m[1], baseURL)
			}
		}
	}
	return resolveRef("/favicon.ico", baseURL)
}

func resolveRef(ref, base string) string {
	if strings.HasPrefix(ref, "//") {
		bu, _ := url.Parse(base)
		return bu.Scheme + ":" + ref
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		return ref
	}
	bu, err := url.Parse(base)
	if err != nil {
		return ref
	}
	rel, err := url.Parse(ref)
	if err != nil {
		return ref
	}
	return bu.ResolveReference(rel).String()
}

// FetchFaviconForTarget fetches the page at targetURL, extracts the favicon
// link, fetches the favicon, and returns the raw image bytes.
// Returns nil if no favicon is found or the fetch fails.
func FetchFaviconForTarget(ctx context.Context, targetURL string) []byte {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return nil
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil
	}
	faviconURL := extractFaviconURL(string(body), targetURL)
	return fetchFaviconBytes(ctx, faviconURL)
}

func fetchFaviconBytes(ctx context.Context, faviconURL string) []byte {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, faviconURL, nil)
	if err != nil {
		return nil
	}
	resp, err := httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return nil
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil || len(data) == 0 {
		return nil
	}
	return data
}

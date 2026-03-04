package discovery

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

var (
	// scanPorts covers common homelab service web UIs.
	scanPorts = []int{
		80,    // HTTP
		443,   // HTTPS
		1880,  // Node-RED
		2283,  // Immich
		2342,  // PhotoPrism
		3000,  // Grafana, Gitea, various
		3001,  // various
		4533,  // Navidrome
		5000,  // various (Changedetection.io, Kavita)
		5001,  // Synology DSM HTTPS, various
		5055,  // Overseerr
		5380,  // Technitium DNS
		5800,  // noVNC / VNC web
		6767,  // Bazarr
		6080,  // noVNC
		7575,  // Homarr
		7878,  // Radarr
		8001,  // various
		8006,  // Proxmox VE
		8008,  // Matrix Synapse HTTP
		8080,  // Traefik dashboard, various
		8083,  // Calibre-Web / Emby
		8096,  // Jellyfin
		8112,  // Deluge Web UI
		8123,  // Home Assistant
		8181,  // Tautulli
		8200,  // HashiCorp Vault
		8384,  // Syncthing
		8443,  // HTTPS alternative
		8448,  // Matrix Synapse HTTPS / federation
		8484,  // Dasherr / Homer
		8686,  // Lidarr
		8787,  // Readarr
		8888,  // Jupyter Notebook
		8920,  // Jellyfin HTTPS
		8989,  // Sonarr
		9000,  // Portainer, MinIO API, various
		9001,  // MinIO Console
		9090,  // Prometheus
		9091,  // Transmission
		9117,  // Jackett
		9443,  // Portainer HTTPS
		9696,  // Prowlarr
		9714,  // Scrutiny
		10000, // Webmin
		13378, // Audiobookshelf
		19999, // Netdata
		32400, // Plex
	}

	// httpsPorts are probed with HTTPS instead of HTTP.
	httpsPorts = map[int]bool{443: true, 5001: true, 8006: true, 8443: true, 8448: true, 8920: true, 9443: true}

	httpClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
			DisableKeepAlives:   true,
			IdleConnTimeout:     5 * time.Second,
			TLSHandshakeTimeout: 3 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 3 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}

	reTitleTag    = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
	reFaviconHref  = regexp.MustCompile(`(?i)<link[^>]+rel=["'][^"']*icon[^"']*["'][^>]+href=["']([^"']+)["']`)
	reFaviconHref2 = regexp.MustCompile(`(?i)<link[^>]+href=["']([^"']+)["'][^>]+rel=["'][^"']*icon[^"']*["']`)
)

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
// Uses 256 workers with a 750ms dial timeout — no data exchange.
func tcpSweep(ctx context.Context, ips []string, ports []int) []openPort {
	type job struct {
		ip   string
		port int
	}
	jobs := make(chan job, 1024)
	results := make(chan openPort, 256)

	const workers = 256
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				if ctx.Err() != nil {
					return
				}
				addr := fmt.Sprintf("%s:%d", j.ip, j.port)
				conn, err := net.DialTimeout("tcp", addr, 750*time.Millisecond)
				if err == nil {
					conn.Close()
					results <- openPort{j.ip, j.port}
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
	return open
}

// ── Subnet helpers ────────────────────────────────────────────────────────────

// getLocalSubnet returns the subnet of the first non-loopback IPv4 interface.
func getLocalSubnet() (*net.IPNet, error) {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	for _, iface := range ifaces {
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
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
				continue // skip non-IPv4 or /0 masks
			}
			_, subnet, _ := net.ParseCIDR(fmt.Sprintf("%s/%d", ip4, ones))
			return subnet, nil
		}
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
	icon        string
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
func (d *Discoverer) scanNetwork(ctx context.Context, cidrs []string) <-chan *probeResult {
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

		// ── Path 1: TCP sweep → HTTP probe ──────────────────────────────────
		outerWg.Add(1)
		go func() {
			defer outerWg.Done()

			// Parse CIDRs or fall back to auto-detect.
			var nets []*net.IPNet
			if len(cidrs) == 0 {
				subnet, err := getLocalSubnet()
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
					nets = append(nets, ipnet)
				}
				if len(nets) == 0 {
					return
				}
			}

			seen := make(map[string]bool)
			var ips []string
			for _, subnet := range nets {
				hosts := generateIPs(subnet)
				d.logf("Subnet %s: %d hosts", subnet, len(hosts))
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
				d.logf("ARP sweep: %d/%d hosts alive", len(alive), len(ips))
				ips = alive
			}

			d.logf("TCP sweep: %d hosts × %d ports", len(ips), len(scanPorts))
			open := tcpSweep(ctx, ips, scanPorts)
			d.logf("TCP sweep complete: %d open ports found", len(open))

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
						probeOnce(op)
					}
				}()
			}
			wg.Wait()
		}()

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
	if httpsPorts[port] {
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

	// Use fingerprint emoji icon if matched; otherwise fetch the favicon.
	icon := svcIcon
	if icon == "" {
		faviconURL := extractFaviconURL(bodyStr, rawURL)
		icon = fetchFaviconBase64(ctx, faviconURL)
	}

	return &probeResult{
		ip:          ip,
		port:        port,
		url:         rawURL,
		title:       title,
		icon:        icon,
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
	for _, re := range []*regexp.Regexp{reFaviconHref, reFaviconHref2} {
		if m := re.FindStringSubmatch(html); len(m) >= 2 {
			return resolveRef(m[1], baseURL)
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

func fetchFaviconBase64(ctx context.Context, faviconURL string) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, faviconURL, nil)
	if err != nil {
		return ""
	}
	resp, err := httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		return ""
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil || len(data) == 0 {
		return ""
	}
	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "image/x-icon"
	}
	return "data:" + ct + ";base64," + base64.StdEncoding.EncodeToString(data)
}

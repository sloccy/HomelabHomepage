package discovery

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lantern/internal/store"
	"lantern/internal/util"

	"go4.org/netipx"
	"golang.org/x/sync/errgroup"
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

	reTitleTag = regexp.MustCompile(`(?is)<title[^>]*>(.*?)</title>`)
)

// resolveURLToPort parses a URL and resolves the host to an IP, returning the
// resulting openPort. Used by SSDP and WS-Discovery to normalise device addresses.
func resolveURLToPort(ctx context.Context, rawURL string) (openPort, bool) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return openPort{}, false
	}
	host := u.Hostname()
	if net.ParseIP(host) == nil {
		addrs, err := net.DefaultResolver.LookupHost(ctx, host)
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

// simpleSig builds a signature that matches when title contains titleMatch OR body contains bodyMatch.
func simpleSig(name string, conf float32, icon, titleMatch, bodyMatch string) signature {
	return signature{name, conf, icon, func(_ http.Header, b, t string) bool {
		return strings.Contains(t, titleMatch) || strings.Contains(b, bodyMatch)
	}}
}

var signatures = []signature{
	// ── Infrastructure ───────────────────────────────────────────────────────
	{"Proxmox VE", 0.99, "🖥️", func(h http.Header, b, _ string) bool {
		return strings.Contains(h.Get("Server"), "pve-api-daemon") || strings.Contains(b, "Proxmox Virtual Environment")
	}},
	simpleSig("Cockpit", 0.92, "🖥️", "Cockpit", "cockpit"),
	simpleSig("Webmin", 0.90, "🖥️", "Webmin", "webmin"),
	{"Synology DSM", 0.95, "💾", func(_ http.Header, b, t string) bool {
		return strings.Contains(t, "Synology") || strings.Contains(b, "Synology DiskStation") || strings.Contains(b, "SYNO.")
	}},
	simpleSig("TrueNAS", 0.95, "💾", "TrueNAS", "TrueNAS"),
	simpleSig("UniFi", 0.92, "📡", "UniFi", "UniFi"),
	simpleSig("OpenWrt", 0.92, "📡", "OpenWrt", "OpenWrt"),

	// ── Monitoring / Observability ───────────────────────────────────────────
	{"Grafana", 0.99, "📊", func(h http.Header, b, t string) bool {
		return h.Get("X-Grafana-Version") != "" || strings.Contains(t, "Grafana") || strings.Contains(b, "grafana")
	}},
	simpleSig("Prometheus", 0.95, "📈", "Prometheus", "prometheus_"),
	{"Netdata", 0.99, "📉", func(h http.Header, b, t string) bool {
		return h.Get("X-Netdata-Version") != "" || strings.Contains(b, "netdataRoot") || strings.Contains(t, "Netdata")
	}},
	simpleSig("Uptime Kuma", 0.95, "🟢", "Uptime Kuma", "uptimekuma"),
	simpleSig("Scrutiny", 0.95, "💾", "Scrutiny", "scrutiny"),
	simpleSig("Healthchecks", 0.90, "✅", "Healthchecks", "healthchecks"),
	simpleSig("Dozzle", 0.95, "📋", "Dozzle", "dozzle"),

	// ── Container management ─────────────────────────────────────────────────
	simpleSig("Portainer", 0.95, "🐳", "Portainer", "Portainer"),
	simpleSig("Yacht", 0.90, "⛵", "Yacht", "yacht-app"),

	// ── Reverse proxies ──────────────────────────────────────────────────────
	simpleSig("Nginx Proxy Manager", 0.95, "🔀", "Nginx Proxy Manager", "Nginx Proxy Manager"),
	simpleSig("Traefik", 0.92, "🔀", "Traefik", "traefik"),

	// ── Home automation ──────────────────────────────────────────────────────
	simpleSig("Home Assistant", 0.95, "🏠", "Home Assistant", "Home Assistant"),
	{"Node-RED", 0.97, "🔴", func(h http.Header, b, t string) bool {
		return strings.Contains(h.Get("X-Powered-By"), "node-red") || strings.Contains(t, "Node-RED") || strings.Contains(b, "node-red")
	}},
	simpleSig("Frigate", 0.95, "📹", "Frigate", "Frigate NVR"),
	simpleSig("Zigbee2MQTT", 0.90, "📡", "Zigbee2MQTT", "zigbee2mqtt"),

	// ── Media servers ────────────────────────────────────────────────────────
	{"Jellyfin", 0.99, "🎬", func(h http.Header, b, t string) bool {
		return h.Get("X-Jellyfin-Version") != "" || strings.Contains(b, "Jellyfin") || strings.Contains(t, "Jellyfin")
	}},
	{"Plex", 0.99, "🎬", func(h http.Header, b, t string) bool {
		return h.Get("X-Plex-Protocol") != "" || strings.Contains(b, "Plex Media Server") || strings.Contains(t, "Plex")
	}},
	{"Emby", 0.95, "🎬", func(h http.Header, b, t string) bool {
		return h.Get("X-Emby-Server-Id") != "" || strings.Contains(t, "Emby") || strings.Contains(b, "Emby Server")
	}},
	simpleSig("Navidrome", 0.95, "🎵", "Navidrome", "Navidrome"),
	simpleSig("Audiobookshelf", 0.95, "🎧", "Audiobookshelf", "audiobookshelf"),

	// ── Media request / management ───────────────────────────────────────────
	simpleSig("Overseerr", 0.95, "🎭", "Overseerr", "Overseerr"),
	simpleSig("Jellyseerr", 0.95, "🎭", "Jellyseerr", "Jellyseerr"),
	simpleSig("Ombi", 0.90, "🎭", "Ombi", "Ombi"),
	simpleSig("Tautulli", 0.95, "📊", "Tautulli", "tautulli"),

	// ── *arr suite ───────────────────────────────────────────────────────────
	simpleSig("Sonarr", 0.95, "📺", "Sonarr", "Sonarr"),
	simpleSig("Radarr", 0.95, "🎥", "Radarr", "Radarr"),
	simpleSig("Lidarr", 0.92, "🎵", "Lidarr", "Lidarr"),
	simpleSig("Readarr", 0.92, "📚", "Readarr", "Readarr"),
	simpleSig("Prowlarr", 0.95, "🔍", "Prowlarr", "Prowlarr"),
	simpleSig("Bazarr", 0.95, "💬", "Bazarr", "Bazarr"),
	simpleSig("Jackett", 0.95, "🔍", "Jackett", "Jackett"),

	// ── Download clients ─────────────────────────────────────────────────────
	simpleSig("Transmission", 0.92, "⬇️", "Transmission", "Transmission Web"),
	simpleSig("qBittorrent", 0.92, "⬇️", "qBittorrent", "qBittorrent"),
	simpleSig("Deluge", 0.90, "⬇️", "Deluge", "Deluge Web"),
	simpleSig("ruTorrent", 0.90, "⬇️", "ruTorrent", "ruTorrent"),
	simpleSig("Flood", 0.90, "⬇️", "Flood", "flood-app"),
	simpleSig("SABnzbd", 0.95, "📥", "SABnzbd", "sabnzbd"),
	simpleSig("NZBGet", 0.95, "📥", "NZBGet", "nzbget"),

	// ── Git / CI ─────────────────────────────────────────────────────────────
	{"Gitea", 0.99, "🦊", func(h http.Header, b, t string) bool {
		return h.Get("X-Gitea-Version") != "" || strings.Contains(t, "Gitea") || strings.Contains(b, "Gitea")
	}},
	{"Forgejo", 0.99, "🦊", func(h http.Header, b, t string) bool {
		return h.Get("X-Forgejo-Version") != "" || strings.Contains(t, "Forgejo") || strings.Contains(b, "Forgejo")
	}},
	simpleSig("Woodpecker CI", 0.90, "🚀", "Woodpecker", "woodpecker-ci"),
	simpleSig("Drone CI", 0.90, "🚀", "Drone", "drone.io"),
	simpleSig("Harbor", 0.92, "🗃️", "Harbor", "harbor-app"),

	// ── Auth / Identity ──────────────────────────────────────────────────────
	{"Vaultwarden", 0.95, "🔐", func(_ http.Header, b, t string) bool {
		return strings.Contains(b, "Vaultwarden") || strings.Contains(t, "Vaultwarden") || strings.Contains(b, "bitwarden")
	}},
	simpleSig("Authelia", 0.95, "🔐", "Authelia", "Authelia"),
	{"Authentik", 0.97, "🔐", func(h http.Header, b, t string) bool {
		return strings.Contains(h.Get("X-Powered-By"), "authentik") || strings.Contains(b, "ak-flow") || strings.Contains(t, "authentik")
	}},
	simpleSig("Keycloak", 0.95, "🔑", "Keycloak", "Keycloak"),
	{"HashiCorp Vault", 0.97, "🔒", func(h http.Header, b, t string) bool {
		return h.Get("X-Vault-Request") != "" || strings.Contains(b, "hashicorp-vault") || strings.Contains(t, "Vault")
	}},

	// ── Networking / VPN ─────────────────────────────────────────────────────
	simpleSig("Pi-hole", 0.95, "🕳️", "Pi-hole", "Pi-hole"),
	simpleSig("AdGuard Home", 0.95, "🛡️", "AdGuard", "AdGuard Home"),
	simpleSig("Technitium DNS", 0.95, "🌐", "Technitium", "TechnitiumDNS"),
	simpleSig("WireGuard Easy", 0.95, "🔒", "WG Easy", "wg-easy"),
	simpleSig("Headscale", 0.90, "🔒", "Headscale", "headscale"),

	// ── Notifications ────────────────────────────────────────────────────────
	simpleSig("Gotify", 0.95, "🔔", "Gotify", "gotify"),
	simpleSig("ntfy", 0.95, "🔔", "ntfy", "ntfy.sh"),

	// ── Photos / Files ───────────────────────────────────────────────────────
	simpleSig("Immich", 0.95, "📷", "Immich", "Immich"),
	simpleSig("PhotoPrism", 0.95, "📸", "PhotoPrism", "PhotoPrism"),
	{"Nextcloud", 0.97, "☁️", func(h http.Header, b, t string) bool {
		return h.Get("X-Nextcloud-Request-ID") != "" || strings.Contains(b, "Nextcloud") || strings.Contains(t, "Nextcloud")
	}},
	{"Syncthing", 0.97, "🔄", func(h http.Header, b, t string) bool {
		return h.Get("X-Syncthing-Id") != "" || strings.Contains(t, "Syncthing") || strings.Contains(b, "syncthing")
	}},
	simpleSig("MinIO", 0.95, "🗄️", "MinIO", "minio"),
	simpleSig("Seafile", 0.90, "🗄️", "Seafile", "seafile"),

	// ── Reading / Documents ──────────────────────────────────────────────────
	simpleSig("Calibre-Web", 0.95, "📚", "Calibre-Web", "calibre-web"),
	simpleSig("Komga", 0.95, "📚", "Komga", "Komga"),
	simpleSig("Kavita", 0.95, "📚", "Kavita", "kavita"),
	simpleSig("Paperless-ngx", 0.95, "📄", "Paperless", "paperless"),
	simpleSig("Stirling-PDF", 0.95, "📄", "Stirling", "stirling-pdf"),
	simpleSig("BookStack", 0.90, "📖", "BookStack", "BookStack"),
	simpleSig("Wallabag", 0.90, "📰", "Wallabag", "wallabag"),
	simpleSig("FreshRSS", 0.90, "📰", "FreshRSS", "FreshRSS"),
	simpleSig("Miniflux", 0.90, "📰", "Miniflux", "miniflux"),

	// ── Homelab dashboards ───────────────────────────────────────────────────
	simpleSig("Homarr", 0.95, "🏠", "Homarr", "Homarr"),
	simpleSig("Homer", 0.90, "🏠", "Homer", "homer-app"),
	simpleSig("Flame", 0.90, "🔥", "Flame", "flame-app"),
	simpleSig("Dashy", 0.90, "📊", "Dashy", "dashy"),
	simpleSig("Heimdall", 0.90, "🛡️", "Heimdall", "heimdall"),
	simpleSig("Organizr", 0.90, "📁", "Organizr", "organizr"),

	// ── Food / Life ──────────────────────────────────────────────────────────
	simpleSig("Mealie", 0.95, "🍽️", "Mealie", "mealie"),
	simpleSig("Grocy", 0.95, "🛒", "grocy", "grocy"),
	simpleSig("Tandoor", 0.90, "🍽️", "Tandoor", "tandoor"),

	// ── Bookmarks / Links ────────────────────────────────────────────────────
	simpleSig("Linkding", 0.95, "🔗", "linkding", "linkding"),
	simpleSig("Shlink", 0.90, "🔗", "Shlink", "shlink"),

	// ── Automation / Workflow ────────────────────────────────────────────────
	{"n8n", 0.95, "⚙️", func(_ http.Header, b, t string) bool {
		return strings.EqualFold(t, "n8n") || strings.Contains(b, "\"n8n\"")
	}},
	simpleSig("Changedetection.io", 0.95, "👁️", "changedetection", "changedetection"),

	// ── Remote access ────────────────────────────────────────────────────────
	simpleSig("Guacamole", 0.95, "🖥️", "Guacamole", "guacamole"),

	// ── Communication / Social ───────────────────────────────────────────────
	{"Matrix Synapse", 0.95, "💬", func(h http.Header, b, t string) bool {
		return strings.Contains(h.Get("Server"), "Synapse") || strings.Contains(b, "matrix-synapse") || strings.Contains(t, "Matrix")
	}},

	// ── Misc ─────────────────────────────────────────────────────────────────
	simpleSig("Verdaccio", 0.90, "📦", "Verdaccio", "verdaccio"),
	simpleSig("IT Tools", 0.90, "🔧", "IT Tools", "it-tools"),
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
// Uses up to 4096 concurrent goroutines with a configurable dial timeout — no data exchange.
// logf is called at 25% progress intervals with per-type error counts.
func tcpSweep(ctx context.Context, ips []string, ports []int, logf func(string, ...any), timeout time.Duration) []openPort {
	// ── Pre-sweep debug summary ───────────────────────────────────────────────
	start := time.Now()
	logf("[TCP] Starting sweep: %d hosts × %d ports = %d combinations", len(ips), len(ports), len(ips)*len(ports))
	logf("[TCP] Timeout: %v/conn, 4096 concurrent workers", timeout)
	switch {
	case len(ips) == 0:
		logf("[TCP] ERROR: no hosts to scan — check subnet config")
		return nil
	case len(ips) <= 30:
		logf("[TCP] Hosts: %s", strings.Join(ips, ", "))
	default:
		logf("[TCP] Hosts: %s … %s (%d total)", ips[0], ips[len(ips)-1], len(ips))
	}

	total := int64(len(ips) * len(ports))
	var done atomic.Int64
	var countOpen, countTimeouts, countRefused, countOther atomic.Int64
	var mu sync.Mutex
	var open []openPort

	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(4096)

	for _, ip := range ips {
		for _, port := range ports {
			g.Go(func() error {
				if gctx.Err() != nil {
					return nil
				}
				addr := net.JoinHostPort(ip, strconv.Itoa(port))
				conn, err := (&net.Dialer{Timeout: timeout}).DialContext(gctx, "tcp", addr)
				if err == nil {
					_ = conn.Close()
					countOpen.Add(1)
					mu.Lock()
					open = append(open, openPort{ip, port})
					mu.Unlock()
				} else {
					errStr := err.Error()
					switch {
					case strings.Contains(errStr, "i/o timeout") || strings.Contains(errStr, "timeout"):
						countTimeouts.Add(1)
					case strings.Contains(errStr, "connection refused"):
						countRefused.Add(1)
					default:
						countOther.Add(1)
						logf("[ERR] %s:%d → %v", ip, port, err)
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
				return nil
			})
		}
	}
	_ = g.Wait()

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
	prefix, err := netip.ParsePrefix(subnet.String())
	if err != nil || !prefix.Addr().Is4() {
		return nil
	}
	r := netipx.RangeOfPrefix(prefix.Masked())
	var ips []string
	// r.From() is the network address; r.To() is the broadcast — skip both.
	for ip := r.From().Next(); ip.Compare(r.To()) < 0; ip = ip.Next() {
		ips = append(ips, ip.String())
	}
	return ips
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

				allPorts := make([]int, 65535)
				for i := range allPorts {
					allPorts[i] = i + 1
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
	// Try the heuristic scheme first; fall back to the opposite if it fails.
	// This handles plain HTTP services on well-known HTTPS ports (e.g. 5001)
	// and HTTPS services on non-standard ports.
	scheme := "http"
	if util.IsHTTPSPort(port) {
		scheme = "https"
	}
	if r := tryProbe(ctx, ip, port, scheme); r != nil {
		return r
	}
	alt := "https"
	if scheme == "https" {
		alt = "http"
	}
	return tryProbe(ctx, ip, port, alt)
}

func tryProbe(ctx context.Context, ip string, port int, scheme string) *probeResult {
	rawURL := fmt.Sprintf("%s://%s:%d/", scheme, ip, port)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

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
	iconData := util.FetchFaviconFromHTML(ctx, bodyStr, rawURL)
	iconStr := svcIcon // emoji fallback
	if len(iconData) > 0 {
		iconStr = store.IconFile
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

// detectScheme probes ip:port to determine the actual HTTP or HTTPS scheme.
// It tries the heuristic guess first, then falls back to the opposite scheme.
// Returns the heuristic default if neither probe succeeds (e.g. service is down).
func detectScheme(ctx context.Context, ip string, port int) string {
	scheme := "http"
	if util.IsHTTPSPort(port) {
		scheme = "https"
	}
	if schemeReachable(ctx, ip, port, scheme) {
		return scheme
	}
	alt := "https"
	if scheme == "https" {
		alt = "http"
	}
	if schemeReachable(ctx, ip, port, alt) {
		return alt
	}
	return scheme // fallback to heuristic
}

func schemeReachable(ctx context.Context, ip string, port int, scheme string) bool {
	rawURL := fmt.Sprintf("%s://%s:%d/", scheme, ip, port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return false
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode != http.StatusBadRequest
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

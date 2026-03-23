# Lantern

**Homelab reverse proxy, service discovery, and homepage — in a single Docker container.**

[![GHCR](https://img.shields.io/badge/ghcr.io-sloccy%2Flantern-blue?logo=github)](https://github.com/sloccy/Lantern/pkgs/container/lantern)
[![License: MIT](https://img.shields.io/badge/license-MIT-green)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8?logo=go)](go.mod)

---

## Features

| | Feature | Details |
|---|---------|---------|
| 🔀 | **Reverse proxy** | HTTPS termination with subdomain routing. Accepts self-signed backend certs. |
| 🔒 | **Wildcard TLS** | Auto-provisions and renews Let's Encrypt wildcard cert via Cloudflare DNS-01 challenge. Falls back to a self-signed cert while provisioning. |
| 🐳 | **Docker discovery** | Watches the Docker socket; auto-assigns subdomains from container names. Reads Traefik labels for compatibility. |
| 🔍 | **Network scan** | Full TCP sweep (all 65535 ports) on manual trigger, with ARP pre-sweep on Linux to skip dead hosts. Identifies services with 94 built-in fingerprints. |
| 📡 | **Multicast discovery** | Passive mDNS/DNS-SD, SSDP, and WS-Discovery listeners run on every scheduled scan interval — no TCP sweep required. |
| 🌐 | **Dynamic DNS** | Tracks public IP via ipify.org and updates Cloudflare A records automatically. |
| ☁️ | **Cloudflare Tunnel** | Manage a Cloudflare Zero Trust tunnel directly from the UI — create, route services through it, and delete, without touching the Cloudflare dashboard. |
| 🏠 | **Dark dashboard** | HTMX-powered SPA with a service grid homepage, bookmark bar, system stats, and a full management view. |

---

## Quick Start

**1. Get the compose file**

```bash
curl -O https://raw.githubusercontent.com/sloccy/Lantern/main/docker-compose.yml
```

**2. Fill in your values**

Edit `docker-compose.yml` and set at minimum:

```yaml
DOMAIN: "yourdomain.com"
CF_API_TOKEN: "your-token"   # Zone:DNS:Edit permission
CF_ZONE_ID: "your-zone-id"
SERVER_IP: "10.0.0.5"        # This machine's LAN IP
```

**3. Start**

```bash
docker compose up -d
```

Lantern will be available at `https://lantern.yourdomain.com` once the wildcard cert provisions (usually under 30 seconds).

---

## Deployment

### Full annotated docker-compose.yml

```yaml
services:
  lantern:
    image: ghcr.io/sloccy/lantern:latest
    # Alternatively, build from source:
    # build: .
    container_name: lantern
    restart: unless-stopped

    # NET_RAW: required for ARP pre-sweep (faster scans) and multicast (mDNS/WS-Discovery).
    cap_add:
      - NET_RAW

    # Add the host's docker group so the nonroot user can access the Docker socket.
    # Find the GID with: stat -c '%g' /var/run/docker.sock
    group_add:
      - "1000"

    # Host networking is required for mDNS (port 5353) and WS-Discovery (port 3702)
    # multicast to reach the local network. Remove the 'ports' section when using this.
    network_mode: host

    # Alternative: bridge networking. Disables mDNS and WS-Discovery multicast.
    # Uncomment and remove 'network_mode: host' above.
    # ports:
    #   - "80:80"
    #   - "443:443"

    volumes:
      - lantern_data:/data
      - /var/run/docker.sock:/var/run/docker.sock:ro

    environment:
      # Your root domain. Wildcard cert covers *.yourdomain.com
      DOMAIN: "yourdomain.com"

      # Cloudflare API token — Zone:DNS:Edit permission for your zone.
      CF_API_TOKEN: "your-cf-api-token"

      # Cloudflare Zone ID (found on the zone overview page).
      CF_ZONE_ID: "your-cf-zone-id"

      # Local IP of this server — used for DNS A records.
      # Not required if using Cloudflare Tunnel mode exclusively.
      SERVER_IP: "10.0.0.5"

      # Cloudflare Account ID — required for tunnel management.
      # Found on the Cloudflare dashboard right sidebar.
      # Once set, use Manage → Cloudflare Tunnel in the UI.
      # CF_ACCOUNT_ID: ""

      # Where to store config, certs, and ACME keys inside the container.
      DATA_DIR: /data

      # How often background (mDNS/SSDP/WS-Discovery) scans run.
      SCAN_INTERVAL: "24h"

      # TCP dial timeout per port for full network scans, in milliseconds.
      # Increase for slow links or VLANs. Default: 200
      # SCAN_TIMEOUT_MS: "200"

volumes:
  lantern_data:
    driver: local
```

### Pre-deployment checklist

- [ ] **Cloudflare API token** — create at Cloudflare Dashboard → Profile → API Tokens. Needs `Zone:DNS:Edit` for your zone.
- [ ] **Cloudflare Zone ID** — found on the zone overview page right sidebar.
- [ ] **Docker socket GID** — run `stat -c '%g' /var/run/docker.sock` on the host and set `group_add` to match.
- [ ] **Host networking** — use `network_mode: host` for full mDNS and WS-Discovery multicast support. Use `ports` if host networking is not available.
- [ ] **NET_RAW capability** — required for ARP pre-sweep and multicast. Safe to omit; scanning still works without it.

### Cloudflare Tunnel mode

To route services through a Cloudflare Zero Trust tunnel instead of exposing ports directly:

1. Set `CF_ACCOUNT_ID` in your compose file.
2. Navigate to **Manage → Cloudflare Tunnel** in the UI and click **Create Tunnel**.
3. When adding or editing a service, enable the **Route via Tunnel** toggle.

`SERVER_IP` is optional in tunnel mode — no A records are created for tunneled services.

### Bridge networking fallback

If host networking is not available (e.g., on a shared host or Kubernetes):

```yaml
services:
  lantern:
    image: ghcr.io/sloccy/lantern:latest
    ports:
      - "80:80"
      - "443:443"
    cap_add:
      - NET_RAW
    group_add:
      - "1000"
    volumes:
      - lantern_data:/data
      - /var/run/docker.sock:/var/run/docker.sock:ro
    environment:
      DOMAIN: "yourdomain.com"
      CF_API_TOKEN: "your-token"
      CF_ZONE_ID: "your-zone-id"
      SERVER_IP: "10.0.0.5"
volumes:
  lantern_data:
```

> **Note:** In bridge networking mode, mDNS (port 5353) and WS-Discovery (port 3702) multicast is blocked by Docker NAT. These discovery paths are silently skipped; TCP sweep and SSDP still work.

---

## Configuration

All configuration is via environment variables.

| Variable | Default | Description |
|----------|---------|-------------|
| `DOMAIN` | *(required)* | Root domain. Wildcard cert covers `*.DOMAIN`. |
| `CF_API_TOKEN` | *(required)* | Cloudflare API token with `Zone:DNS:Edit` permission. |
| `CF_ZONE_ID` | *(required)* | Cloudflare Zone ID. |
| `SERVER_IP` | — | Local IP for subdomain DNS A records. Not needed in tunnel-only mode. |
| `CF_ACCOUNT_ID` | — | Cloudflare Account ID. Required to create/manage tunnels from the UI. |
| `CF_TUNNEL_ID` | — | Auto-populated by the UI after tunnel creation. Can be pre-set to adopt an existing tunnel. |
| `DATA_DIR` | `/data` | Persistent data directory inside the container. |
| `SCAN_INTERVAL` | `24h` | Background scan interval. Accepts Go duration strings: `6h`, `30m`, etc. |
| `SCAN_TIMEOUT_MS` | `200` | TCP dial timeout per port during full network scans, in milliseconds. Increase for slow links. |

---

## Docker Labels

Add labels to any container to control how Lantern discovers it:

```yaml
services:
  plex:
    image: plexinc/pms-docker
    labels:
      lantern.name: "Plex Media Server"
      lantern.subdomain: "plex"
      lantern.port: "32400"
      # lantern.scheme: "https"   # optional; auto-detected for ports 443/8443/9443
      # lantern.url: "http://10.0.0.5:32400"  # fully explicit target, skips all port logic
      # lantern.enable: "false"   # opt this container out entirely

  sonarr:
    image: linuxserver/sonarr
    labels:
      lantern.port: "8989"
```

**Label priority (highest → lowest):**

1. `lantern.url` — explicit target URL; skips all other port logic
2. `lantern.port` — use this port on `SERVER_IP`
3. `traefik.http.services.<n>.loadbalancer.server.port` — Traefik label compatibility
4. Published port fallback — any published TCP port

**Traefik label compatibility** — if your containers already have Traefik labels, Lantern reads them automatically:

```yaml
labels:
  traefik.http.routers.sonarr.rule: "Host(`sonarr.yourdomain.com`)"
  traefik.http.services.sonarr.loadbalancer.server.port: "8989"
```

---

## Service Discovery

Lantern uses four concurrent discovery paths:

### 1. Full TCP sweep (manual trigger only)

Triggered by clicking **Scan Now** in the UI or calling `POST /api/scan`. Sweeps all 65535 TCP ports across configured subnets. On Linux with `CAP_NET_RAW`, an ARP pre-sweep first identifies live hosts and skips dead IPs — dramatically reducing scan time on sparse subnets.

Subnets are auto-detected from the host's network interfaces (capped at /24 to avoid scanning large cloud/VPN subnets). Additional subnets can be added via **Manage → Scan Subnets**.

### 2. mDNS / DNS-SD

Listens on `224.0.0.251:5353` for devices advertising HTTP services via Bonjour/Avahi. Requires `network_mode: host` and `CAP_NET_RAW`.

### 3. SSDP (UPnP)

Queries `239.255.255.250:1900` for UPnP/DLNA devices. Works in both host and bridge networking.

### 4. WS-Discovery

Queries `239.255.255.250:3702` for ONVIF cameras, printers, and Windows devices. Requires `network_mode: host`.

Paths 2–4 run on every scheduled `SCAN_INTERVAL`. Path 1 is manual-only.

### Fingerprint engine

After a TCP port responds to HTTP, the response headers, body, and `<title>` are matched against **94 built-in signatures** spanning:

*Infrastructure:* Proxmox VE, Cockpit, Webmin, Synology DSM, TrueNAS, UniFi, OpenWrt
*Monitoring:* Grafana, Prometheus, Netdata, Uptime Kuma, Scrutiny, Healthchecks, Dozzle
*Containers:* Portainer, Yacht
*Media:* Jellyfin, Plex, Emby, Navidrome, Audiobookshelf
*Media management:* Overseerr, Jellyseerr, Ombi, Tautulli, Sonarr, Radarr, Lidarr, Readarr, Prowlarr, Bazarr, Jackett
*Downloads:* Transmission, qBittorrent, Deluge, ruTorrent, Flood, SABnzbd, NZBGet
*Auth:* Vaultwarden, Authelia, Authentik, Keycloak, HashiCorp Vault
*Networking:* Pi-hole, AdGuard Home, Technitium DNS, WireGuard Easy, Headscale
*Files/Photos:* Immich, PhotoPrism, Nextcloud, Syncthing, MinIO, Seafile
*Automation:* Home Assistant, Node-RED, Frigate, Zigbee2MQTT, n8n, Changedetection.io
*Git/CI:* Gitea, Forgejo, Woodpecker CI, Drone CI, Harbor
*Reading/Documents:* Calibre-Web, Komga, Kavita, Paperless-ngx, Stirling-PDF, BookStack, Wallabag, FreshRSS, Miniflux
*And more:* Guacamole, Matrix Synapse, Gotify, ntfy, Mealie, Grocy, Tandoor, Homarr, Homer, and others

When a fingerprint matches, the service name and emoji icon are populated automatically. Lantern also fetches and stores the actual favicon as a base64 data URI.

---

## API Reference

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/api/services` | List all assigned services |
| `POST` | `/api/services` | Create a service (form data) |
| `PUT` | `/api/services/{id}` | Update a service |
| `DELETE` | `/api/services/{id}` | Delete a service and its DNS record |
| `POST` | `/api/services/{id}/move` | Move service to a new position |
| `POST` | `/api/services/reorder` | Reorder multiple services (`{"ids": [...]}`) |
| `GET` | `/api/icons/{id}` | Get icon image for a service |
| `POST` | `/api/services/{id}/icon` | Upload a custom icon (multipart) |
| `POST` | `/api/services/{id}/favicon` | Pull favicon from the service's target URL |
| `DELETE` | `/api/services/{id}/icon` | Clear custom icon |
| `GET` | `/api/discovered` | List unassigned discovered services |
| `DELETE` | `/api/discovered/{id}` | Dismiss a discovered service |
| `POST` | `/api/discovered/{id}/ignore` | Permanently ignore an IP:port |
| `GET` | `/api/ignored` | List ignored IP:port entries |
| `DELETE` | `/api/ignored/{id}` | Un-ignore an entry |
| `POST` | `/api/scan` | Trigger an immediate full network scan |
| `GET` | `/api/status` | Scan status, public IP, domain, tunnel info |
| `GET` | `/api/scan/subnets` | List configured scan subnets |
| `POST` | `/api/scan/subnets` | Add a subnet (`{"cidr": "192.168.2.0/24"}`) |
| `DELETE` | `/api/scan/subnets?cidr=...` | Remove a subnet |
| `GET` | `/api/ddns` | List DDNS-managed domains and current public IP |
| `POST` | `/api/ddns` | Add a DDNS domain |
| `DELETE` | `/api/ddns/{domain}` | Remove a DDNS domain and its DNS record |
| `GET` | `/api/bookmarks` | List bookmarks |
| `POST` | `/api/bookmarks` | Create a bookmark |
| `PUT` | `/api/bookmarks/{id}` | Update a bookmark |
| `DELETE` | `/api/bookmarks/{id}` | Delete a bookmark |
| `POST` | `/api/bookmarks/{id}/move` | Move bookmark to a new position |
| `GET` | `/api/settings` | Get dashboard settings |
| `PUT` | `/api/settings` | Update dashboard settings |
| `GET` | `/api/tunnel` | Get tunnel status |
| `POST` | `/api/tunnel` | Create a Cloudflare tunnel |
| `DELETE` | `/api/tunnel` | Delete the Cloudflare tunnel |
| `GET` | `/api/health` | Service health map (`{id: "up"\|"down"}`) |
| `GET` | `/api/sysinfo` | Host CPU, memory, and disk stats |
| `GET` | `/api/favicon?url=...` | Proxy a favicon from an internal URL |

---

## Data Layout

```
/data/
  config.json         ← services, discovered, bookmarks, DDNS domains, settings
  icons/              ← uploaded and fetched service icons
  certs/
    cert.pem          ← TLS certificate (wildcard)
    key.pem           ← TLS private key
    resource.json     ← ACME cert resource (for renewal)
  acme/
    account.key       ← ACME account private key
    account.json      ← ACME account registration
```

---

## Building from Source

**Docker:**

```bash
docker build -t lantern .
```

**Local Go build** (requires Go 1.25+):

```bash
go mod tidy
go build -o lantern .
./lantern
```

---

## Troubleshooting

**Cert provisioning fails on startup**
A temporary self-signed cert is served while Lantern retries in the background. Check that `CF_API_TOKEN` has `Zone:DNS:Edit` permission and that `CF_ZONE_ID` matches your domain's zone.

**No containers discovered**
Verify the Docker socket is mounted (`/var/run/docker.sock:/var/run/docker.sock:ro`) and that `group_add` matches the socket's GID (`stat -c '%g' /var/run/docker.sock`).

**No mDNS / WS-Discovery results**
These require `network_mode: host`. In bridge networking mode these paths are silently skipped.

**Full scan finds nothing**
Check the scan log in **Manage → Status**. If no subnets are detected, add one manually via **Manage → Scan Subnets**. Increase `SCAN_TIMEOUT_MS` if scanning over a slow link or VLAN.

**Scan is slow**
Add `CAP_NET_RAW` — the ARP pre-sweep eliminates timeout waits for dead hosts. On a /24 with 20 live hosts, scan time drops from ~3 minutes to under 30 seconds.

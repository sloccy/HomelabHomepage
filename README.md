# Lantern

A self-contained Go reverse proxy, service discovery tool, and homelab homepage. Runs as a single Docker container.

## Quick Start

### 1. Edit `docker-compose.yml`

Set your values in the `environment` section:

```yaml
DOMAIN: yourdomain.com          # Your root domain
CF_API_TOKEN: "your-token"  # Cloudflare API token (Zone:DNS:Edit)
CF_ZONE_ID: "your-zone-id"  # Cloudflare Zone ID
SERVER_IP: "10.0.0.5"       # Local IP for DNS A records
SCAN_INTERVAL: "24h"        # Network scan interval (Go duration)
```

### 2. Build and run

```bash
docker compose up -d --build
```

The Lantern GUI will be available at `https://lantern.yourdomain.com`.

---

## Features

| Feature | Details |
|---------|---------|
| **Reverse proxy** | HTTPS termination, subdomain routing, supports self-signed backend certs |
| **TLS** | Auto wildcard cert via Let's Encrypt + Cloudflare DNS-01 challenge |
| **Docker discovery** | Watches Docker socket, auto-assigns subdomain = container name |
| **Network scan** | Scans local /24 subnet for HTTP services on 13 common ports |
| **Dynamic DNS** | Tracks public IP via ipify.org, updates Cloudflare records |
| **GUI** | Dark-themed SPA — homepage (service grid) + manage view |

## Environment Variables

| Variable | Default | Description |
|----------|---------|-------------|
| `DOMAIN` | `yourdomain.com` | Root domain for wildcard cert and DNS |
| `CF_API_TOKEN` | — | Cloudflare API token with Zone:DNS:Edit permission |
| `CF_ZONE_ID` | — | Cloudflare Zone ID |
| `SERVER_IP` | — | Local IP used for subdomain DNS A records |
| `DATA_DIR` | `/data` | Persistent data directory |
| `SCAN_INTERVAL` | `24h` | Network scan interval (any Go duration, e.g. `6h`, `30m`) |

## Data layout

```
/data/
  config.json       ← services, discovered, DDNS domains, public IP
  certs/
    cert.pem        ← TLS certificate (wildcard)
    key.pem         ← TLS private key
    resource.json   ← ACME cert resource (for renewal)
  acme/
    account.key     ← ACME account private key
    account.json    ← ACME account registration
```

## Building locally (without Docker)

```bash
go mod tidy
go build -o lantern .
```

Requires Go 1.22+.

## API Reference

| Method | Path | Description |
|--------|------|-------------|
| `GET`  | `/api/services` | List assigned services |
| `POST` | `/api/services` | Create/assign service |
| `PUT`  | `/api/services/{id}` | Update service |
| `DELETE` | `/api/services/{id}` | Delete service + DNS record |
| `GET`  | `/api/discovered` | List unassigned discovered services |
| `DELETE` | `/api/discovered/{id}` | Dismiss discovered service |
| `POST` | `/api/scan` | Trigger immediate network scan |
| `GET`  | `/api/status` | Scan status, public IP, domain info |
| `GET`  | `/api/ddns` | List DDNS domains |
| `POST` | `/api/ddns` | Add DDNS domain |
| `DELETE` | `/api/ddns/{domain}` | Remove DDNS domain |

## Docker Labels

Add labels to any container to control how Lantern discovers it:

```yaml
services:
  plex:
    image: plexinc/pms-docker
    labels:
      lantern.name: "Plex Media Server"
      lantern.subdomain: "plex"
      lantern.port: "32400"        # non-standard port
      # lantern.scheme: "https"    # optional, auto-detected for 443/8443/9443
      # lantern.url: "http://10.0.0.5:32400"  # fully explicit target
      # lantern.enable: "false"    # opt out entirely

  sonarr:
    image: linuxserver/sonarr
    labels:
      lantern.port: "8989"
```

**Label priority (highest → lowest):**
1. `lantern.url` — explicit target, skips all other port logic
2. `lantern.port` — use this port on SERVER_IP
3. `traefik.http.services.<n>.loadbalancer.server.port` — Traefik compatibility
4. Published port fallback (any published TCP port)

**Traefik label compatibility** — if your containers already have Traefik labels,
Lantern reads them automatically:

```yaml
labels:
  traefik.http.routers.sonarr.rule: "Host(`sonarr.yourdomain.com`)"
  traefik.http.services.sonarr.loadbalancer.server.port: "8989"
```

## Network Scan Port List

The scanner probes these ports and ignores any that don't respond with HTTP:

`80, 443, 2283 (Immich), 3000 (Grafana/Gitea), 4533 (Navidrome), 5000, 5001, 5055 (Overseerr), 6080, 7878 (Radarr), 8001, 8006 (Proxmox), 8080, 8096 (Jellyfin), 8123 (Home Assistant), 8443, 8686 (Lidarr), 8920, 8989 (Sonarr), 9000 (Portainer), 9090 (Prometheus), 9091 (Transmission), 9117 (Jackett), 9443, 19999 (Netdata), 32400 (Plex)`

## Notes

- The container needs `NET_BIND_SERVICE` capability (included in `docker-compose.yml`) to bind ports 80/443 as the `nonroot` distroless user.
- If Let's Encrypt cert provisioning fails on startup, a temporary self-signed cert is used while retrying in the background.
- Docker discovery requires mounting `/var/run/docker.sock` (read-only).
- Network scanning runs on the configured interval only — not on startup.

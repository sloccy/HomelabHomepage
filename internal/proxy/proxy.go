package proxy

import (
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"atlas/internal/config"
	"atlas/internal/store"
)

var insecureTransport = &http.Transport{
	TLSClientConfig:     &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // intentional for backend self-signed certs
	IdleConnTimeout:     90 * time.Second,
	TLSHandshakeTimeout: 10 * time.Second,
}

// Handler is the top-level HTTP handler that routes by subdomain.
type Handler struct {
	cfg        *config.Config
	store      *store.Store
	webHandler http.Handler
}

func New(cfg *config.Config, st *store.Store, webHandler http.Handler) *Handler {
	return &Handler{cfg: cfg, store: st, webHandler: webHandler}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	host := r.Host
	// Strip port if present.
	if i := strings.LastIndexByte(host, ':'); i != -1 {
		host = host[:i]
	}

	suffix := "." + h.cfg.Domain
	atlasHost := "atlas" + suffix

	switch {
	case host == atlasHost || host == h.cfg.Domain || host == "":
		h.webHandler.ServeHTTP(w, r)
		return
	case strings.HasSuffix(host, suffix):
		sub := strings.TrimSuffix(host, suffix)
		h.proxySubdomain(w, r, sub)
	default:
		http.NotFound(w, r)
	}
}

func (h *Handler) proxySubdomain(w http.ResponseWriter, r *http.Request, sub string) {
	svc := h.store.GetServiceBySubdomain(sub)
	if svc == nil {
		h.errorPage(w, r, 404, fmt.Sprintf("No service assigned to <strong>%s.%s</strong>", sub, h.cfg.Domain), "")
		return
	}
	target, err := url.Parse(svc.Target)
	if err != nil {
		h.errorPage(w, r, 502, "Invalid target URL for this service.", svc.Name)
		return
	}

	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = insecureTransport
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		h.errorPage(w, r, 502, fmt.Sprintf("Could not reach <strong>%s</strong>.<br><small>%v</small>", svc.Name, err), svc.Name)
	}
	// Preserve original Host header behaviour.
	origDirector := rp.Director
	rp.Director = func(req *http.Request) {
		origDirector(req)
		req.Header.Set("X-Forwarded-Host", r.Host)
		req.Header.Set("X-Real-IP", realIP(r))
		if req.Header.Get("X-Forwarded-Proto") == "" {
			req.Header.Set("X-Forwarded-Proto", scheme(r))
		}
	}
	// Rewrite Location headers that point to the backend host back to the
	// public-facing URL, preventing redirect loops (e.g. Proxmox VE).
	rp.ModifyResponse = func(resp *http.Response) error {
		loc := resp.Header.Get("Location")
		if loc == "" {
			return nil
		}
		locURL, err := url.Parse(loc)
		if err != nil {
			return nil
		}
		if locURL.Host == target.Host {
			locURL.Scheme = scheme(r)
			locURL.Host = r.Host
			resp.Header.Set("Location", locURL.String())
		}
		return nil
	}
	rp.ServeHTTP(w, r)
}

func (h *Handler) errorPage(w http.ResponseWriter, r *http.Request, code int, msg, svcName string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	title := "Service Unavailable"
	if code == 404 {
		title = "Service Not Found"
	}
	fmt.Fprintf(w, errorHTML, title, title, msg, h.cfg.Domain)
}

func realIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return strings.SplitN(ip, ",", 2)[0]
	}
	return r.RemoteAddr
}

func scheme(r *http.Request) string {
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

const errorHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>%s — Atlas</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{background:#0f0f1a;color:#e2e8f0;font-family:system-ui,sans-serif;display:flex;align-items:center;justify-content:center;min-height:100vh;padding:2rem}
.card{background:#1a1a2e;border:1px solid #2d2d4e;border-radius:12px;padding:3rem;max-width:480px;text-align:center}
.icon{font-size:3rem;margin-bottom:1rem}
h1{font-size:1.5rem;margin-bottom:.75rem;color:#ef4444}
p{color:#94a3b8;line-height:1.6}
a{color:#7c3aed;text-decoration:none}a:hover{text-decoration:underline}
.back{margin-top:2rem}
.btn{display:inline-block;padding:.5rem 1.25rem;background:#7c3aed;color:#fff;border-radius:6px;text-decoration:none;font-size:.875rem}
.btn:hover{background:#6d28d9;text-decoration:none}
</style>
</head>
<body>
<div class="card">
  <div class="icon">🚫</div>
  <h1>%s</h1>
  <p>%s</p>
  <div class="back"><a class="btn" href="https://atlas.%s">← Back to Atlas</a></div>
</div>
</body>
</html>`

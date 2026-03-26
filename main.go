package main

import (
	"context"
	"crypto/tls"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"lantern/internal/certs"
	"lantern/internal/cf"
	"lantern/internal/config"
	"lantern/internal/ddns"
	"lantern/internal/discovery"
	"lantern/internal/proxy"
	"lantern/internal/store"
	"lantern/internal/sysinfo"
	"lantern/internal/tunnel"
	"lantern/internal/web"
)

// Injected at build time via -ldflags "-X main.version=... -X main.commit=..."
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	// Healthcheck subcommand for container health probes.
	// Distroless images have no shell or wget, so the binary handles its own check.
	// Usage: /lantern healthcheck
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		resp, err := http.Get("http://127.0.0.1/healthz")
		if err != nil || resp.StatusCode != http.StatusOK {
			os.Exit(1)
		}
		resp.Body.Close()
		os.Exit(0)
	}

	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[lantern] ")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("starting version=%s commit=%s domain=%s server_ip=%s scan_interval=%s",
		version, commit, cfg.Domain, cfg.ServerIP, cfg.ScanInterval)
	if cfg.Domain == "" {
		log.Println("WARNING: DOMAIN not set — TLS cert provisioning and DNS management will not work")
	}

	// Persistent store.
	st, err := store.New(cfg.DataDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	// Cloudflare DNS + tunnel client (no-op when credentials are absent).
	cfClient, err := cf.New(cfg.CFAPIToken, cfg.CFZoneID, cfg.CFTunnelID, cfg.CFAccountID)
	if err != nil {
		log.Fatalf("cloudflare: %v", err)
	}
	if cfg.CFAPIToken == "" || cfg.CFZoneID == "" {
		log.Println("WARNING: CF_API_TOKEN or CF_ZONE_ID not set — DNS management disabled")
	}
	if cfClient.TunnelEnabled() {
		log.Printf("Cloudflare Tunnel mode active: tunnel_id=%s", cfg.CFTunnelID)
	} else if cfg.CFTunnelID != "" {
		log.Println("WARNING: CF_TUNNEL_ID set but CF_ACCOUNT_ID missing — tunnel management disabled")
	}

	// TLS certificate manager.
	certMgr, err := certs.New(cfg)
	if err != nil {
		log.Fatalf("certs: %v", err)
	}

	// Web server (serves GUI + REST API).
	webSrv := web.New(cfg, st, cfClient, version)

	// Proxy handler (routes by subdomain).
	proxyHandler := proxy.New(cfg, st, webSrv)

	// Service discovery (network scan + Docker watch).
	disco := discovery.New(cfg, st, cfClient)
	webSrv.SetScanner(disco)

	// Cloudflare Tunnel manager (subprocess lifecycle).
	tunnelMgr := tunnel.New(cfg, st, cfClient)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := tunnelMgr.StartIfConfigured(ctx); err != nil {
		log.Printf("tunnel: auto-start failed: %v", err)
	}
	webSrv.SetTunnelManager(tunnelMgr)

	sysinfo.Start(ctx)
	go disco.DockerWatch(ctx)
	go disco.ScheduledScan(ctx)
	go webSrv.StartHealthChecker(ctx)

	// Dynamic DNS.
	ddnsMgr := ddns.New(cfg, st, cfClient)
	go ddnsMgr.Run(ctx)

	// HTTP → HTTPS redirect (with /healthz for container health checks).
	// Start this before cert provisioning so the healthcheck responds immediately
	// on first deploy, even while EnsureCert() is blocked on DNS-01 propagation.
	httpMux := http.NewServeMux()
	httpMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	httpMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		target := "https://" + r.Host + r.URL.RequestURI()
		http.Redirect(w, r, target, http.StatusMovedPermanently)
	})
	httpSrv := &http.Server{
		Addr:              ":80",
		Handler:           httpMux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		log.Println("HTTP  listening on :80")
		if err := httpSrv.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v", err)
		}
	}()

	// Provision TLS certificate (may take 60-120s on first run via Let's Encrypt).
	if err := certMgr.EnsureCert(); err != nil {
		log.Printf("certs: initial cert failed: %v", err)
	}
	go certMgr.RenewLoop()

	// HTTPS server.
	tlsCfg := &tls.Config{
		GetCertificate: certMgr.GetCertificate,
		MinVersion:     tls.VersionTLS12,
	}
	httpsSrv := &http.Server{
		Addr:              ":443",
		Handler:           proxyHandler,
		TLSConfig:         tlsCfg,
		ReadHeaderTimeout: 30 * time.Second,
		WriteTimeout:      0, // streaming responses
		IdleTimeout:       120 * time.Second,
	}
	go func() {
		log.Println("HTTPS listening on :443")
		if err := httpsSrv.ListenAndServeTLS("", ""); err != http.ErrServerClosed {
			log.Printf("HTTPS server error: %v", err)
			os.Exit(1)
		}
	}()

	// Graceful shutdown.
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("shutting down...")
	cancel()

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutCancel()

	_ = httpSrv.Shutdown(shutCtx)
	_ = httpsSrv.Shutdown(shutCtx)
	tunnelMgr.Stop()
	webSrv.Stop()
	_ = st.Save()
	log.Println("bye")
}

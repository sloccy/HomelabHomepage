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

	"launchpad/internal/certs"
	"launchpad/internal/cf"
	"launchpad/internal/config"
	"launchpad/internal/ddns"
	"launchpad/internal/discovery"
	"launchpad/internal/proxy"
	"launchpad/internal/store"
	"launchpad/internal/web"
)

// Injected at build time via -ldflags "-X main.version=... -X main.commit=..."
var (
	version = "dev"
	commit  = "unknown"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmsgprefix)
	log.SetPrefix("[launchpad] ")

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("config: %v", err)
	}
	log.Printf("starting version=%s commit=%s domain=%s server_ip=%s scan_interval=%s",
		version, commit, cfg.Domain, cfg.ServerIP, cfg.ScanInterval)

	// Persistent store.
	st, err := store.New(cfg.DataDir)
	if err != nil {
		log.Fatalf("store: %v", err)
	}

	// Cloudflare DNS client (no-op when credentials are absent).
	cfClient, err := cf.New(cfg.CFAPIToken, cfg.CFZoneID)
	if err != nil {
		log.Fatalf("cloudflare: %v", err)
	}
	if cfg.CFAPIToken == "" || cfg.CFZoneID == "" {
		log.Println("WARNING: CF_API_TOKEN or CF_ZONE_ID not set — DNS management disabled")
	}

	// TLS certificate manager.
	certMgr, err := certs.New(cfg)
	if err != nil {
		log.Fatalf("certs: %v", err)
	}
	if err := certMgr.EnsureCert(); err != nil {
		log.Printf("certs: initial cert failed: %v", err)
	}
	go certMgr.RenewLoop()

	// Web server (serves GUI + REST API).
	webSrv := web.New(cfg, st, cfClient)

	// Proxy handler (routes by subdomain).
	proxyHandler := proxy.New(cfg, st, webSrv)

	// Service discovery (network scan + Docker watch).
	disco := discovery.New(cfg, st, cfClient)
	webSrv.SetScanner(disco)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go disco.DockerWatch(ctx)
	go disco.ScheduledScan(ctx)

	// Dynamic DNS.
	ddnsMgr := ddns.New(cfg, st, cfClient)
	go ddnsMgr.Run(ctx)

	// HTTP → HTTPS redirect (with /healthz for container health checks).
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
		log.Println("HTTP  listening on :80")
		if err := httpSrv.ListenAndServe(); err != http.ErrServerClosed {
			log.Printf("HTTP server error: %v", err)
		}
	}()
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
	_ = st.Save()
	log.Println("bye")
}

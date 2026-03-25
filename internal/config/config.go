package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	Domain       string
	CFAPIToken   string
	CFZoneID     string
	CFTunnelID   string
	CFAccountID  string
	ServerIP     string
	DataDir      string
	ScanInterval time.Duration
	ScanTimeout  time.Duration
}

func Load() (*Config, error) {
	scanInterval := 24 * time.Hour
	if s := os.Getenv("SCAN_INTERVAL"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return nil, fmt.Errorf("invalid SCAN_INTERVAL %q: %w", s, err)
		}
		scanInterval = d
	}

	scanTimeoutMs := 200
	if s := os.Getenv("SCAN_TIMEOUT_MS"); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			scanTimeoutMs = v
		} else {
			return nil, fmt.Errorf("invalid SCAN_TIMEOUT_MS %q: %w", s, err)
		}
	}

	cfg := &Config{
		Domain:       getEnv("DOMAIN", ""),
		CFAPIToken:   os.Getenv("CF_API_TOKEN"),
		CFZoneID:     os.Getenv("CF_ZONE_ID"),
		CFTunnelID:   os.Getenv("CF_TUNNEL_ID"),
		CFAccountID:  os.Getenv("CF_ACCOUNT_ID"),
		ServerIP:     os.Getenv("SERVER_IP"),
		DataDir:      getEnv("DATA_DIR", "/data"),
		ScanInterval: scanInterval,
		ScanTimeout:  time.Duration(scanTimeoutMs) * time.Millisecond,
	}
	return cfg, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

package config

import (
	"fmt"
	"os"
	"time"
)

// Config holds all runtime configuration loaded from environment variables.
type Config struct {
	Domain       string
	CFAPIToken   string
	CFZoneID     string
	ServerIP     string
	DataDir      string
	ScanInterval time.Duration
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

	cfg := &Config{
		Domain:       getEnv("DOMAIN", ""),
		CFAPIToken:   os.Getenv("CF_API_TOKEN"),
		CFZoneID:     os.Getenv("CF_ZONE_ID"),
		ServerIP:     os.Getenv("SERVER_IP"),
		DataDir:      getEnv("DATA_DIR", "/data"),
		ScanInterval: scanInterval,
	}
	return cfg, nil
}

func getEnv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

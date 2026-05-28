package config

import (
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr bool
	}{
		{
			name: "defaults",
			want: Config{
				DataDir:      "/data",
				ScanInterval: 24 * time.Hour,
				ScanTimeout:  200 * time.Millisecond,
			},
		},
		{
			name: "all values set",
			env: map[string]string{
				"DOMAIN":          "example.com",
				"CF_API_TOKEN":    "tok",
				"CF_ZONE_ID":      "zone1",
				"CF_TUNNEL_ID":    "tun1",
				"CF_ACCOUNT_ID":   "acc1",
				"SERVER_IP":       "1.2.3.4",
				"DATA_DIR":        "/tmp/data",
				"SCAN_INTERVAL":   "1h",
				"SCAN_TIMEOUT_MS": "500",
			},
			want: Config{
				Domain:       "example.com",
				CFAPIToken:   "tok",
				CFZoneID:     "zone1",
				CFTunnelID:   "tun1",
				CFAccountID:  "acc1",
				ServerIP:     "1.2.3.4",
				DataDir:      "/tmp/data",
				ScanInterval: time.Hour,
				ScanTimeout:  500 * time.Millisecond,
			},
		},
		{
			name:    "invalid SCAN_INTERVAL",
			env:     map[string]string{"SCAN_INTERVAL": "not-a-duration"},
			wantErr: true,
		},
		{
			name:    "invalid SCAN_TIMEOUT_MS",
			env:     map[string]string{"SCAN_TIMEOUT_MS": "abc"},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Clear env vars that have defaults so each test is isolated.
			for _, key := range []string{
				"DOMAIN", "CF_API_TOKEN", "CF_ZONE_ID", "CF_TUNNEL_ID", "CF_ACCOUNT_ID",
				"SERVER_IP", "DATA_DIR", "SCAN_INTERVAL", "SCAN_TIMEOUT_MS",
			} {
				t.Setenv(key, "")
			}
			for k, v := range tt.env {
				t.Setenv(k, v)
			}

			got, err := Load()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Load() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if *got != tt.want {
				t.Errorf("Load() = %+v, want %+v", *got, tt.want)
			}
		})
	}
}

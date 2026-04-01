package discovery

import (
	"testing"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/go-connections/nat"
)

func TestTraefikSubdomain(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		domain string
		want   string
	}{
		{
			name: "simple host rule with domain suffix",
			labels: map[string]string{
				"traefik.http.routers.myapp.rule": "Host(`myapp.example.com`)",
			},
			domain: "example.com",
			want:   "myapp",
		},
		{
			name: "host rule without matching domain",
			labels: map[string]string{
				"traefik.http.routers.myapp.rule": "Host(`myapp.other.com`)",
			},
			domain: "example.com",
			want:   "myapp-other-com",
		},
		{
			name: "no domain configured — full host sanitised",
			labels: map[string]string{
				"traefik.http.routers.myapp.rule": "Host(`myapp.example.com`)",
			},
			domain: "",
			want:   "myapp-example-com",
		},
		{
			name:   "no traefik labels",
			labels: map[string]string{"com.example.foo": "bar"},
			domain: "example.com",
			want:   "",
		},
		{
			name: "case insensitive Host match",
			labels: map[string]string{
				"traefik.http.routers.app.rule": "HOST(`app.example.com`)",
			},
			domain: "example.com",
			want:   "app",
		},
		{
			name: "label key without .rule suffix ignored",
			labels: map[string]string{
				"traefik.http.routers.app.service": "app-svc",
			},
			domain: "example.com",
			want:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := traefikSubdomain(tt.labels, tt.domain)
			if got != tt.want {
				t.Errorf("traefikSubdomain = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestTraefikPort(t *testing.T) {
	tests := []struct {
		name   string
		labels map[string]string
		want   int
	}{
		{
			name: "valid port",
			labels: map[string]string{
				"traefik.http.services.myapp.loadbalancer.server.port": "32400",
			},
			want: 32400,
		},
		{
			name:   "no port label",
			labels: map[string]string{"other": "val"},
			want:   0,
		},
		{
			name: "non-numeric port",
			labels: map[string]string{
				"traefik.http.services.myapp.loadbalancer.server.port": "notaport",
			},
			want: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := traefikPort(tt.labels)
			if got != tt.want {
				t.Errorf("traefikPort = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestBestPort(t *testing.T) {
	tests := []struct {
		name  string
		ports []container.Port
		want  int
	}{
		{
			name:  "prefers 8080 over other ports",
			ports: []container.Port{{Type: "tcp", PublicPort: 32400}, {Type: "tcp", PublicPort: 8080}},
			want:  8080,
		},
		{
			name:  "falls back to first tcp port",
			ports: []container.Port{{Type: "tcp", PublicPort: 32400}},
			want:  32400,
		},
		{
			name:  "ignores udp ports",
			ports: []container.Port{{Type: "udp", PublicPort: 9000}},
			want:  0,
		},
		{
			name:  "no ports",
			ports: nil,
			want:  0,
		},
		{
			name:  "prefers 80 over 9000",
			ports: []container.Port{{Type: "tcp", PublicPort: 9000}, {Type: "tcp", PublicPort: 80}},
			want:  80,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := bestPort(tt.ports)
			if got != tt.want {
				t.Errorf("bestPort = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestSplitTarget(t *testing.T) {
	tests := []struct {
		target   string
		wantIP   string
		wantPort int
	}{
		{"http://10.0.0.1:8080", "10.0.0.1", 8080},
		{"https://10.0.0.2:9443", "10.0.0.2", 9443},
		{"http://10.0.0.3:80/", "10.0.0.3", 80},
		{"10.0.0.4:3000", "10.0.0.4", 3000},
	}
	for _, tt := range tests {
		ip, port := splitTarget(tt.target)
		if ip != tt.wantIP || port != tt.wantPort {
			t.Errorf("splitTarget(%q) = (%q, %d), want (%q, %d)", tt.target, ip, port, tt.wantIP, tt.wantPort)
		}
	}
}

func TestPreserveScheme(t *testing.T) {
	tests := []struct {
		old, new, want string
	}{
		{"https://10.0.0.1:8080", "http://10.0.0.2:9000", "https://10.0.0.2:9000"},
		{"http://10.0.0.1:80", "https://10.0.0.2:443", "http://10.0.0.2:443"},
		{"no-scheme", "http://10.0.0.1:80", "http://10.0.0.1:80"},
	}
	for _, tt := range tests {
		got := preserveScheme(tt.old, tt.new)
		if got != tt.want {
			t.Errorf("preserveScheme(%q, %q) = %q, want %q", tt.old, tt.new, got, tt.want)
		}
	}
}

func TestPortsFromNat(t *testing.T) {
	pm := nat.PortMap{
		"8080/tcp": []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "8080"}},
		"443/tcp":  []nat.PortBinding{{HostIP: "0.0.0.0", HostPort: "9443"}},
	}
	ports := portsFromNat(pm)
	portSet := make(map[int]bool)
	for _, p := range ports {
		if p.Type == "tcp" {
			portSet[int(p.PublicPort)] = true
		}
	}
	if !portSet[8080] || !portSet[9443] {
		t.Errorf("portsFromNat: expected 8080 and 9443, got %v", ports)
	}
}

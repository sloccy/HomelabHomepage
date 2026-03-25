package util

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"net"
	"sort"
	"strings"

	"lantern/internal/store"
)

// NewID returns a random 16-character hex string suitable for entity IDs.
func NewID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// SanitiseSubdomain converts an arbitrary string into a valid DNS label:
// lowercase, spaces/underscores/dots replaced with hyphens, all other
// non-alphanumeric characters stripped, leading/trailing hyphens removed.
// Returns "service" if the result would be empty.
func SanitiseSubdomain(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else if r == '_' || r == '.' || r == ' ' {
			b.WriteRune('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "service"
	}
	return s
}

// ParseFormBool returns true when val is "on", "true", or "1" — the values
// sent by HTML checkboxes and common boolean query parameters.
func ParseFormBool(val string) bool {
	return val == "on" || val == "true" || val == "1"
}

// IsHTTPSPort reports whether the given port is conventionally served over HTTPS.
// This is the canonical set used across discovery and service assignment.
func IsHTTPSPort(port int) bool {
	switch port {
	case 443, 5001, 8006, 8443, 8448, 8920, 9443:
		return true
	}
	return false
}

// SortDiscoveredByIP sorts a slice of discovered services by IP address then
// port, in ascending order.
func SortDiscoveredByIP(svcs []*store.DiscoveredService) {
	sort.Slice(svcs, func(i, j int) bool {
		a := net.ParseIP(svcs[i].IP).To4()
		b := net.ParseIP(svcs[j].IP).To4()
		if a == nil {
			a = net.ParseIP(svcs[i].IP)
		}
		if b == nil {
			b = net.ParseIP(svcs[j].IP)
		}
		if cmp := bytes.Compare(a, b); cmp != 0 {
			return cmp < 0
		}
		return svcs[i].Port < svcs[j].Port
	})
}

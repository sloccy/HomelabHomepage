package discovery

import (
	"testing"
)

func TestExtractTitle(t *testing.T) {
	tests := []struct {
		html string
		want string
	}{
		{"<html><head><title>Plex Media Server</title></head></html>", "Plex Media Server"},
		{"<title>  spaced  out  </title>", "spaced out"},
		{"<TITLE>Uppercase Tag</TITLE>", "Uppercase Tag"},
		{"no title here", ""},
		// Truncated to 80 chars
		{"<title>" + string(make([]byte, 100)) + "</title>", string(make([]byte, 80))},
	}
	for _, tt := range tests {
		got := extractTitle(tt.html)
		if got != tt.want {
			t.Errorf("extractTitle(%q) = %q, want %q", tt.html, got, tt.want)
		}
	}
}

func TestFingerprint(t *testing.T) {
	// Fingerprint with no matching signature should return empty values.
	name, conf, icon := fingerprint(nil, "random body content", "Random Title")
	if name != "" || conf != 0 || icon != "" {
		t.Errorf("fingerprint (no match): got (%q, %v, %q)", name, conf, icon)
	}
}

package util

import (
	"strings"
	"testing"
)

func TestSanitiseSubdomain(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"Plex Media Server", "plex-media-server"},
		{"home_assistant", "home-assistant"},
		{"my.service", "my-service"},
		{"  leading spaces  ", "leading-spaces"},
		{"UPPERCASE", "uppercase"},
		{"hello-world", "hello-world"},
		{"123numeric", "123numeric"},
		{"--leading-hyphens--", "leading-hyphens"},
		{"special!@#chars", "specialchars"},
		{"", "service"},
		{"!!!!", "service"},
		{"a", "a"},
	}
	for _, tt := range tests {
		got := SanitiseSubdomain(tt.in)
		if got != tt.want {
			t.Errorf("SanitiseSubdomain(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestParseFormBool(t *testing.T) {
	trueVals := []string{"on", "true", "1"}
	falseVals := []string{"", "off", "false", "0", "yes", "no", "ON", "True"}
	for _, v := range trueVals {
		if !ParseFormBool(v) {
			t.Errorf("ParseFormBool(%q) = false, want true", v)
		}
	}
	for _, v := range falseVals {
		if ParseFormBool(v) {
			t.Errorf("ParseFormBool(%q) = true, want false", v)
		}
	}
}

func TestIsHTTPSPort(t *testing.T) {
	https := []int{443, 5001, 8006, 8443, 8448, 8920, 9443}
	http := []int{80, 8080, 8081, 3000, 9000, 1}
	for _, p := range https {
		if !IsHTTPSPort(p) {
			t.Errorf("IsHTTPSPort(%d) = false, want true", p)
		}
	}
	for _, p := range http {
		if IsHTTPSPort(p) {
			t.Errorf("IsHTTPSPort(%d) = true, want false", p)
		}
	}
}

func TestNewID(t *testing.T) {
	id := NewID()
	if len(id) != 16 {
		t.Errorf("NewID() length = %d, want 16", len(id))
	}
	const hexChars = "0123456789abcdef"
	for _, c := range id {
		if !strings.ContainsRune(hexChars, c) {
			t.Errorf("NewID() contains non-hex char %q in %q", c, id)
		}
	}
	// Should be unique
	id2 := NewID()
	if id == id2 {
		t.Errorf("NewID() returned same value twice: %q", id)
	}
}

package util

import (
	"testing"
)

func TestResolveRef(t *testing.T) {
	base := "http://10.0.0.1:8080/path/page"
	tests := []struct {
		ref  string
		want string
	}{
		{"//cdn.example.com/img.png", "http://cdn.example.com/img.png"},
		{"https://absolute.com/icon.png", "https://absolute.com/icon.png"},
		{"http://absolute.com/icon.png", "http://absolute.com/icon.png"},
		{"/favicon.ico", "http://10.0.0.1:8080/favicon.ico"},
		{"../images/icon.png", "http://10.0.0.1:8080/images/icon.png"},
	}
	for _, tt := range tests {
		got := resolveRef(tt.ref, base)
		if got != tt.want {
			t.Errorf("resolveRef(%q, %q) = %q, want %q", tt.ref, base, got, tt.want)
		}
	}
}

func TestExtractFaviconURL(t *testing.T) {
	base := "http://10.0.0.1:8080"
	tests := []struct {
		name string
		html string
		want string
	}{
		{
			name: "apple touch icon",
			html: `<link rel="apple-touch-icon" href="/apple-touch-icon.png">`,
			want: "http://10.0.0.1:8080/apple-touch-icon.png",
		},
		{
			name: "shortcut icon",
			html: `<link rel="shortcut icon" href="/favicon.ico">`,
			want: "http://10.0.0.1:8080/favicon.ico",
		},
		{
			name: "no icon in html — default favicon.ico",
			html: `<html><head><title>Test</title></head></html>`,
			want: "http://10.0.0.1:8080/favicon.ico",
		},
		{
			name: "absolute url icon",
			html: `<link rel="icon" href="https://cdn.example.com/icon.png">`,
			want: "https://cdn.example.com/icon.png",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractFaviconURL(tt.html, base)
			if got != tt.want {
				t.Errorf("extractFaviconURL = %q, want %q", got, tt.want)
			}
		})
	}
}

package sysinfo

import (
	"testing"
)

func TestParseMemLine(t *testing.T) {
	tests := []struct {
		line    string
		want    uint64
		wantErr bool
	}{
		{"MemTotal:   16384 kB", 16384 * 1024, false},
		{"MemAvailable:   8192 kB", 8192 * 1024, false},
		{"MemTotal:   0 kB", 0, false},
		{"MemTotal:", 0, true},        // no value field
		{"MemTotal: abc kB", 0, true}, // non-numeric value
		{"", 0, true},                 // empty line
	}
	for _, tt := range tests {
		t.Run(tt.line, func(t *testing.T) {
			got, err := parseMemLine(tt.line)
			if (err != nil) != tt.wantErr {
				t.Fatalf("parseMemLine(%q) error = %v, wantErr %v", tt.line, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("parseMemLine(%q) = %d, want %d", tt.line, got, tt.want)
			}
		})
	}
}

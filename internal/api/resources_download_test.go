package api

import (
	"strings"
	"testing"
)

func TestValidDownloadFilename(t *testing.T) {
	tests := []struct {
		name string
		ok   bool
	}{
		{"model.bin", true},
		{"my report.txt", true},
		{"模型.bin", true},
		{"", false},
		{" padded", false},
		{"dir/file", false},
		{`dir\file`, false},
		{"line\nbreak", false},
		{"nul\x00", false},
		{".", false},
		{"..", false},
		{strings.Repeat("a", 256), false},
	}
	for _, tt := range tests {
		if got := validDownloadFilename(tt.name); got != tt.ok {
			t.Errorf("validDownloadFilename(%q) = %v, want %v", tt.name, got, tt.ok)
		}
	}
}

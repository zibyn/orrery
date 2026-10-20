package api

import (
	"testing"
	"time"
)

// A retention is applied in whole seconds, so 0 means "forever" and a positive
// sub-second value is rejected instead of truncating to 0 (defaults.md).
func TestEnvRetention(t *testing.T) {
	const fallback = 30 * 24 * time.Hour
	tests := []struct {
		name  string
		value string
		want  time.Duration
		bad   bool
	}{
		{"unset uses the fallback", "", fallback, false},
		{"0 keeps forever", "0", 0, false},
		{"a duration in hours", "720h", 720 * time.Hour, false},
		{"negative", "-1h", 0, true},
		{"sub-second would truncate to 0", "500ms", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("ORRERY_TEST_RETENTION", tt.value)
			got, err := envRetention("ORRERY_TEST_RETENTION", fallback)
			if (err != nil) != tt.bad || got != tt.want {
				t.Fatalf("envRetention = (%v, %v), want (%v, bad=%v)", got, err, tt.want, tt.bad)
			}
		})
	}
}

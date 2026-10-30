package nodeproto

import "testing"

func TestParseEgressEntry(t *testing.T) {
	tests := []struct {
		raw    string
		prefix string
		port   uint16
		ok     bool
	}{
		{"10.0.0.0/8", "10.0.0.0/8", 0, true},
		{"10.0.0.5", "10.0.0.5/32", 0, true},
		{"10.0.0.5/32:443", "10.0.0.5/32", 443, true},
		{"10.0.0.2:53", "10.0.0.2/32", 53, true},
		{"10.1.2.3/8", "10.0.0.0/8", 0, true},
		{"0.0.0.0/0:65535", "0.0.0.0/0", 65535, true},
		{"10.0.0.0/8:0", "", 0, false},
		{"10.0.0.0/8:65536", "", 0, false},
		{"10.0.0.0/8:", "", 0, false},
		{"10.0.0.0/8:+80", "", 0, false},
		{"10.0.0.0/33", "", 0, false},
		{"::1/128", "", 0, false},
		{"::ffff:10.0.0.1", "", 0, false},
		{"example.com:443", "", 0, false},
		{"", "", 0, false},
		{" 10.0.0.0/8", "", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.raw, func(t *testing.T) {
			entry, err := ParseEgressEntry(tt.raw)
			if (err == nil) != tt.ok {
				t.Fatalf("ParseEgressEntry(%q) error = %v, want ok %v", tt.raw, err, tt.ok)
			}
			if tt.ok && (entry.Prefix.String() != tt.prefix || entry.Port != tt.port) {
				t.Fatalf("ParseEgressEntry(%q) = %v:%d, want %s:%d", tt.raw, entry.Prefix, entry.Port, tt.prefix, tt.port)
			}
		})
	}
}

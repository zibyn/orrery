package api

import "testing"

// The reported Sandbox address is stored only when it is IPv4 and inside the
// Node's Sandbox subnet, when that is known; anything else is no address.
func TestObservedSandboxAddress(t *testing.T) {
	subnet := "10.213.1.0/24"
	for _, tt := range []struct {
		name, address string
		subnet        *string
		want          string
	}{
		{"inside the subnet", "10.213.1.7", &subnet, "10.213.1.7"},
		{"outside the subnet", "10.213.2.7", &subnet, ""},
		{"IPv6", "fd00::7", &subnet, ""},
		{"IPv6 with no subnet known", "fd00::7", nil, ""},
		{"not an address", "10.213.1.7/24", &subnet, ""},
		{"no subnet known", "10.213.2.7", nil, "10.213.2.7"},
		{"none reported", "", &subnet, ""},
	} {
		if got := observedSandboxAddress(tt.address, tt.subnet); got != tt.want {
			t.Errorf("%s: address = %q, want %q", tt.name, got, tt.want)
		}
	}
}

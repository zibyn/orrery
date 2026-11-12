package agent

import (
	"errors"
	"testing"
)

// runtime.md §2: the Sandbox's loopback is not the host's, so loopback
// nameservers are dropped and everything else is kept.
func TestSandboxResolvConf(t *testing.T) {
	for _, tt := range []struct{ name, host, want string }{
		{
			name: "upstream kept",
			host: "nameserver 223.5.5.5\nnameserver 114.114.114.114\nsearch .\n",
			want: "nameserver 223.5.5.5\nnameserver 114.114.114.114\nsearch .\n",
		},
		{
			name: "loopback dropped",
			host: "# stub\nnameserver 127.0.0.53\nnameserver ::1\nnameserver 10.0.0.2\noptions edns0 trust-ad\n",
			want: "# stub\nnameserver 10.0.0.2\noptions edns0 trust-ad\n",
		},
		{
			name: "only loopback",
			host: "nameserver 127.0.0.1",
			want: "",
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := sandboxResolvConf(tt.host); got != tt.want {
				t.Fatalf("sandboxResolvConf(%q) = %q, want %q", tt.host, got, tt.want)
			}
		})
	}
}

// #252: the sandbox_links negative probe counts only a dial that timed out
// as dropped; a connect, a refusal or a failed exec says nothing.
func TestProbeDialDropped(t *testing.T) {
	for _, tt := range []struct {
		name   string
		code   int
		stderr string
		err    error
		want   bool
	}{
		{"connected", 0, "", nil, false},
		{"timed out", 1, "dial tcp 10.213.0.3:7: i/o timeout", nil, true},
		{"refused", 1, "dial tcp 10.213.0.3:7: connect: connection refused", nil, false},
		{"exec failed", 0, "", errors.New("create probe exec"), false},
	} {
		if got := probeDialDropped(tt.code, tt.stderr, tt.err); got != tt.want {
			t.Errorf("%s: probeDialDropped = %v, want %v", tt.name, got, tt.want)
		}
	}
}

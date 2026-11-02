package agent

import "testing"

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

// TestBundledDaemonDefaults: the Agent talks to the containerd and buildkitd
// install.sh bundles, never the host's (ADR-0028, defaults.md).
func TestBundledDaemonDefaults(t *testing.T) {
	if DefaultContainerdSock != "/run/orrery/containerd/containerd.sock" {
		t.Errorf("DefaultContainerdSock = %q", DefaultContainerdSock)
	}
	if DefaultBuildkitAddr != "unix:///run/orrery/buildkit/buildkitd.sock" {
		t.Errorf("DefaultBuildkitAddr = %q", DefaultBuildkitAddr)
	}
}

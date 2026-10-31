package agent

import "testing"

// runtime.md §13: the Agent records the host-side veth the bridge plugin
// created, and the Sandbox address, from the CNI result.
func TestParseCNIResult(t *testing.T) {
	raw := []byte(`{"cniVersion":"1.0.0","interfaces":[
		{"name":"orrery0","mac":"aa:aa:aa:aa:aa:aa"},
		{"name":"veth1a2b3c4d","mac":"bb:bb:bb:bb:bb:bb"},
		{"name":"eth0","mac":"cc:cc:cc:cc:cc:cc","sandbox":"/run/netns/orrery-x"}],
		"ips":[{"interface":2,"address":"10.213.0.7/24","gateway":"10.213.0.1"}]}`)
	got, err := parseCNIResult(raw, "orrery0")
	if err != nil {
		t.Fatal(err)
	}
	if got.HostVeth != "veth1a2b3c4d" || got.Address != "10.213.0.7" {
		t.Fatalf("parseCNIResult = %+v", got)
	}
	if _, err := parseCNIResult([]byte(`{"interfaces":[{"name":"orrery0"}]}`), "orrery0"); err == nil {
		t.Fatal("a result without a veth or address must fail")
	}
}

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

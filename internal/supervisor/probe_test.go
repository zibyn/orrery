package supervisor

import "testing"

// A container's private cgroup namespace reports "/"; a host service reports
// its slice path, which is where Local Runtime limits live (runtime.md §10).
func TestCgroupPathFrom(t *testing.T) {
	for _, tt := range []struct{ data, want string }{
		{"0::/\n", "/sys/fs/cgroup"},
		{"0::/user.slice/user-1000.slice/x.service\n", "/sys/fs/cgroup/user.slice/user-1000.slice/x.service"},
		{"4:cpu:/legacy\n0::/unified\n", "/sys/fs/cgroup/unified"},
		{"4:cpu:/legacy\n", "/sys/fs/cgroup"},
		{"", "/sys/fs/cgroup"},
	} {
		if got := cgroupPathFrom(tt.data); got != tt.want {
			t.Errorf("cgroupPathFrom(%q) = %q, want %q", tt.data, got, tt.want)
		}
	}
}

package deploy

// Tests for the Node half of install.sh that need no root: the generated
// runtime configs and systemd units, and the binary change detection that
// decides which runtime unit restarts (ADR-0028).

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// sh sources install.sh and runs script after it.
func sh(t *testing.T, script string, args ...string) string {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"-c", "source ./install.sh\n" + script, "test"}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		var stderr string
		if ee, ok := err.(*exec.ExitError); ok {
			stderr = string(ee.Stderr)
		}
		t.Fatalf("%s: %v\n%s%s", script, err, out, stderr)
	}
	return string(out)
}

func wantLines(t *testing.T, name, got string, want ...string) {
	t.Helper()
	lines := map[string]bool{}
	for _, l := range strings.Split(got, "\n") {
		lines[strings.TrimSpace(l)] = true
	}
	for _, w := range want {
		if !lines[w] {
			t.Errorf("%s lacks the line %q:\n%s", name, w, got)
		}
	}
}

func TestSourcingRunsNothing(t *testing.T) {
	if got := sh(t, "echo sourced"); got != "sourced\n" {
		t.Fatalf("sourcing install.sh printed %q", got)
	}
}

// curl ... | bash -s -- node is how operators run it, so piped in, it must
// still dispatch; usage exits 2.
func TestPipedRunDispatches(t *testing.T) {
	f, err := os.Open("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	cmd := exec.Command("bash", "-s", "--", "bogus")
	cmd.Stdin = f
	out, err := cmd.CombinedOutput()
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 2 || !strings.Contains(string(out), "usage:") {
		t.Fatalf("piped install.sh bogus: %v\n%s", err, out)
	}
}

// The spike's tested config (docs/research/agent-owned-containerd-spike.md
// §1.1): no host drop-ins, no NRI, no CRI, everything under private paths.
func TestContainerdTOML(t *testing.T) {
	wantLines(t, "containerd.toml", sh(t, "containerd_toml"),
		`version = 3`,
		`root = "/var/lib/orrery-agent/containerd"`,
		`state = "/run/orrery/containerd/state"`,
		`imports = []`,
		`disabled_plugins = ["io.containerd.grpc.v1.cri", "io.containerd.cri.v1.runtime", "io.containerd.cri.v1.images", "io.containerd.podsandbox.controller.v1.podsandbox", "io.containerd.nri.v1.nri"]`,
		`address = "/run/orrery/containerd/containerd.sock"`,
		`address = "/run/orrery/containerd/containerd.sock.ttrpc"`,
		`address = "/run/orrery/containerd/debug.sock"`,
		`[plugins."io.containerd.internal.v1.opt"]`,
		`path = "/var/lib/orrery-agent/containerd/opt"`,
	)
}

func TestBuildkitdTOML(t *testing.T) {
	got := sh(t, `buildkitd_toml registry.example:8443 s3.example`)
	wantLines(t, "buildkitd.toml", got,
		`[registry."registry.example:8443"]`,
		`[registry."s3.example"]`,
		`ca = ["/etc/orrery-agent/ca.crt"]`,
	)
	if n := strings.Count(got, `ca = [`); n != 2 {
		t.Errorf("want one ca per host, got %d:\n%s", n, got)
	}
	if got := sh(t, `buildkitd_toml`); strings.Contains(got, "[registry.") {
		t.Errorf("no hosts, yet a registry section:\n%s", got)
	}
}

// A rerun without --trust-registry keeps the hosts the last run wrote.
func TestBuildkitdHostsRoundTrip(t *testing.T) {
	f := filepath.Join(t.TempDir(), "buildkitd.toml")
	sh(t, `buildkitd_toml a.example:5000 '[::1]:9000' > "$1"`, f)
	if got := sh(t, `buildkitd_hosts "$1"`, f); got != "a.example:5000\n[::1]:9000\n" {
		t.Errorf("buildkitd_hosts = %q", got)
	}
	if got := sh(t, `buildkitd_hosts "$1"`, filepath.Join(t.TempDir(), "missing")); got != "" {
		t.Errorf("buildkitd_hosts of a missing file = %q", got)
	}
}

const bundlePath = "Environment=PATH=/opt/orrery/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

func TestContainerdUnit(t *testing.T) {
	wantLines(t, "orrery-containerd.service", sh(t, "containerd_unit"),
		bundlePath,
		"ExecStart=/opt/orrery/bin/containerd --config /etc/orrery-agent/containerd.toml",
		"Type=notify",
		"Delegate=yes",
		"KillMode=process",
		"Restart=always",
		"OOMScoreAdjust=-999",
	)
}

func TestBuildkitdUnit(t *testing.T) {
	wantLines(t, "orrery-buildkitd.service", sh(t, "buildkitd_unit"),
		"After=orrery-containerd.service",
		bundlePath,
		"ExecStart=/opt/orrery/bin/buildkitd --config /etc/orrery-agent/buildkitd.toml --root /var/lib/orrery-agent/buildkit --addr unix:///run/orrery/buildkit/buildkitd.sock --oci-worker=true --containerd-worker=false --oci-worker-net host",
	)
}

// Wants=, not Requires=: a containerd restart must not restart the Agent,
// and a containerd stop then start must not leave it stopped (spike §4.3).
func TestAgentUnit(t *testing.T) {
	got := sh(t, `agent_unit containerd "/opt/orrery/bin/orrery-agent run --runtime containerd --ca /etc/orrery-agent/ca.crt"`)
	wantLines(t, "orrery-agent.service", got,
		"ExecStart=/opt/orrery/bin/orrery-agent run --runtime containerd --ca /etc/orrery-agent/ca.crt",
		"Wants=network-online.target orrery-containerd.service",
		"After=network-online.target orrery-containerd.service",
		"KillMode=process",
	)
	if strings.Contains("\n"+got, "\nRequires=") {
		t.Errorf("the Agent unit must not use Requires=:\n%s", got)
	}
	local := sh(t, `agent_unit local "/opt/orrery/bin/orrery-agent run --runtime local"`)
	if strings.Contains(local, "orrery-containerd") {
		t.Errorf("a local Node's Agent depends on orrery-containerd:\n%s", local)
	}
}

// install_bins prints what it changed, so a rerun of the same release
// restarts no runtime unit and a replaced binary restarts only its own.
func TestInstallBins(t *testing.T) {
	src, dst := t.TempDir(), t.TempDir()
	write := func(rel, body string, mode os.FileMode) {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("containerd", "c1", 0o755)
	write("buildkitd", "b1", 0o755)
	write("gvisor-bin/gvisor_sentry", "g1", 0o755)
	write("gvisor-bin/README", "doc", 0o644)
	run := func() string { return sh(t, `install_bins "$1" "$2" | LC_ALL=C sort`, src, dst) }

	if got := run(); got != "buildkitd\ncontainerd\ngvisor-bin/README\ngvisor-bin/gvisor_sentry\n" {
		t.Errorf("first install changed %q", got)
	}
	if got := run(); got != "" {
		t.Errorf("same release changed %q", got)
	}
	write("buildkitd", "b2", 0o755)
	if got := run(); got != "buildkitd\n" {
		t.Errorf("replacing buildkitd changed %q", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dst, "buildkitd")); string(b) != "b2" {
		t.Errorf("buildkitd = %q, want b2", b)
	}
	for rel, want := range map[string]os.FileMode{"gvisor-bin/gvisor_sentry": 0o755, "gvisor-bin/README": 0o644} {
		fi, err := os.Stat(filepath.Join(dst, rel))
		if err != nil || fi.Mode().Perm() != want {
			t.Errorf("%s mode = %v (%v), want %v", rel, fi.Mode().Perm(), err, want)
		}
	}
}

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

	"go.yaml.in/yaml/v3"
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
		"SuccessExitStatus=1",
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

// A bundle that cannot be copied stops the install, so a partial one is
// never restarted and recorded as the new version.
func TestInstallBinsFailureStops(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root writes through a read-only directory")
	}
	src, dst := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "containerd"), []byte("c1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dst, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dst, 0o755) })
	cmd := exec.Command("bash", "-c", "source ./install.sh\nchanged=\"$(install_bins \"$1\" \"$2\")\"\necho installed", "test", src, dst)
	out, err := cmd.CombinedOutput()
	if err == nil || strings.Contains(string(out), "installed") {
		t.Fatalf("a failed copy did not stop the install: %v\n%s", err, out)
	}
}

// A target that cannot be written stops the install. Fed by a pipeline,
// write_if_changed's die would end only the subshell and read as
// "unchanged", so no call site may pipe into it.
func TestWriteIfChangedFailureStops(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing", "unit")
	cmd := exec.Command("bash", "-c", "source ./install.sh\nif write_if_changed \"$1\" < <(echo x); then echo changed; fi\necho continued", "test", missing)
	out, err := cmd.CombinedOutput()
	if err == nil || strings.Contains(string(out), "continued") || !strings.Contains(string(out), "cannot write") {
		t.Fatalf("a failed write did not stop the install: %v\n%s", err, out)
	}
	src, err := os.ReadFile("install.sh")
	if err != nil {
		t.Fatal(err)
	}
	for i, l := range strings.Split(string(src), "\n") {
		if strings.Contains(l, "| write_if_changed") {
			t.Errorf("install.sh:%d pipes into write_if_changed: %s", i+1, strings.TrimSpace(l))
		}
	}
}

// Old deploy.md had operators pass the host's sockets; ADR-0028 leaves
// install.sh no mode on the host daemons, so a rerun drops them. --buildkit=
// stays: it turns Snapshot builds off.
func TestDropHostDaemons(t *testing.T) {
	got := sh(t, `drop_host_daemons run --containerd /run/containerd/containerd.sock --isolation runsc --buildkit=unix:///run/buildkit/buildkitd.sock --containerd=/x --buildkit unix:///y 2>/dev/null`)
	if got != "run --isolation runsc" {
		t.Errorf("drop_host_daemons = %q", got)
	}
	if got := sh(t, `drop_host_daemons run --buildkit= --containerd 2>/dev/null`); got != "run --buildkit=" {
		t.Errorf("drop_host_daemons = %q", got)
	}
	if got := sh(t, `drop_host_daemons run --containerd /x 2>&1 >/dev/null`); !strings.Contains(got, "--containerd /x") {
		t.Errorf("drop_host_daemons does not say what it dropped: %q", got)
	}
}

// The Agent runs its self-check only on connect, so the import alone leaves
// the Node unavailable (#208 host check).
func TestProbeImageNotice(t *testing.T) {
	wantLines(t, "probe_image_notice", sh(t, "probe_image_notice"),
		"ctr -a /run/orrery/containerd/containerd.sock -n orrery images import -",
		"systemctl restart orrery-agent",
	)
}

// The oidc block lands in config.yaml verbatim, so secrets with YAML
// metacharacters must round-trip.
func TestOIDCBlock(t *testing.T) {
	want := map[string]string{
		"issuer_url":     "https://polaris.example.com",
		"client_id":      "orrery-web",
		"client_secret":  "a'b: #c",
		"audience":       "api://orrery",
		"webhook_secret": "whsec_x/y+z=",
	}
	out := sh(t, `oidc_block "$@"`, want["issuer_url"], want["client_id"], want["client_secret"], want["audience"], want["webhook_secret"])
	var got struct {
		OIDC map[string]string `yaml:"oidc"`
	}
	if err := yaml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("oidc_block is not YAML: %v\n%s", err, out)
	}
	for k, v := range want {
		if got.OIDC[k] != v {
			t.Errorf("%s = %q, want %q\n%s", k, got.OIDC[k], v, out)
		}
	}
}

// A fresh server install pins both sandbox images by the digest ghcr reports
// for the release (#235; platform_snapshots is digest-only, ADR-0012).
func TestPlatformSnapshotsBlock(t *testing.T) {
	const digest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	got := sh(t, `
curl() {
  case "$*" in
    *ghcr.io/token*) echo '{"token":"tok"}' ;;
    *"Bearer tok"*/v2/zibyn/orrery-sandbox/manifests/v1.2.3*|*"Bearer tok"*/v2/zibyn/orrery-sandbox-cuda/manifests/v1.2.3*)
      printf 'HTTP/2 200\r\nDocker-Content-Digest: `+digest+`\r\n\r\n' ;;
    *) return 22 ;;
  esac
}
platform_snapshots_block v1.2.3`)
	var parsed struct {
		PlatformSnapshots []map[string]any `yaml:"platform_snapshots"`
	}
	if err := yaml.Unmarshal([]byte(got), &parsed); err != nil {
		t.Fatalf("not YAML: %v\n%s", err, got)
	}
	want := []map[string]any{
		{"name": "ubuntu-24.04", "ref": "ghcr.io/zibyn/orrery-sandbox@" + digest, "default": true},
		{"name": "cuda-12.8-ubuntu-24.04", "ref": "ghcr.io/zibyn/orrery-sandbox-cuda@" + digest, "default_gpu": true},
	}
	if len(parsed.PlatformSnapshots) != len(want) {
		t.Fatalf("platform_snapshots = %v", parsed.PlatformSnapshots)
	}
	for i, w := range want {
		for k, v := range w {
			if parsed.PlatformSnapshots[i][k] != v {
				t.Errorf("entry %d %s = %v, want %v", i, k, parsed.PlatformSnapshots[i][k], v)
			}
		}
	}
}

// A failed lookup stops the install instead of writing one with no Snapshot.
func TestPlatformSnapshotsBlockLookupFails(t *testing.T) {
	cmd := exec.Command("bash", "-c", "source ./install.sh\ncurl() { return 22; }\nblock=\"$(platform_snapshots_block v1.2.3)\"\necho wrote")
	out, err := cmd.CombinedOutput()
	if err == nil || strings.Contains(string(out), "wrote") || !strings.Contains(string(out), "orrery-sandbox") {
		t.Fatalf("a failed lookup did not stop the install: %v\n%s", err, out)
	}
}

// fakeNode is a Node host in a temp dir for running install.sh node as a
// non-root user: curl serves release archives from rel, and systemctl and
// trust_ca only log to $d/log.
type fakeNode struct {
	t      *testing.T
	d, rel string
}

func newFakeNode(t *testing.T) *fakeNode {
	n := &fakeNode{t: t, d: t.TempDir(), rel: t.TempDir()}
	if err := os.MkdirAll(filepath.Join(n.d, "state"), 0o755); err != nil {
		t.Fatal(err)
	}
	// An enrolled Node: install.sh skips orrery-agent enroll.
	if err := os.WriteFile(filepath.Join(n.d, "state", "identity.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	return n
}

// release packs files (archive path to content) as version's Node archive.
func (n *fakeNode) release(version string, files map[string]string) {
	n.t.Helper()
	src := n.t.TempDir()
	for rel, body := range files {
		p := filepath.Join(src, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			n.t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			n.t.Fatal(err)
		}
	}
	dir := filepath.Join(n.rel, version)
	pkg := "orrery-node_" + version + "_linux_amd64.tar.gz"
	script := `set -e; mkdir -p "$2"; tar -C "$1" -czf "$2/$3" .; cd "$2"; sha256sum "$3" > SHA256SUMS`
	if out, err := exec.Command("bash", "-c", script, "pack", src, dir, pkg).CombinedOutput(); err != nil {
		n.t.Fatalf("pack %s: %v\n%s", version, err, out)
	}
}

// run runs install.sh node with args and returns its output.
func (n *fakeNode) run(args ...string) (string, error) {
	if out, err := exec.Command("uname", "-m").Output(); err != nil || strings.TrimSpace(string(out)) != "x86_64" {
		n.t.Skip("the fake release is amd64")
	}
	script := `source ./install.sh
d="$1" rel="$2"; shift 2
NODE_BIN="$d/bin" NODE_CONF="$d/conf" NODE_STATE="$d/state" NODE_UNITS="$d/units"
mkdir -p "$NODE_UNITS"
id() { echo 0; }
curl() { local url="$2"; cp "$rel/$(basename "$(dirname "$url")")/$(basename "$url")" "$4"; }
systemctl() { echo "systemctl $*" >> "$d/log"; }
trust_ca() { echo "trust_ca $(cat "$1")" >> "$d/log"; }
node "$@"`
	out, err := exec.Command("bash", append([]string{"-c", script, "test", n.d, n.rel}, args...)...).CombinedOutput()
	return string(out), err
}

// log returns and clears what systemctl and trust_ca logged.
func (n *fakeNode) log() string {
	p := filepath.Join(n.d, "log")
	b, _ := os.ReadFile(p)
	os.Remove(p)
	return string(b)
}

// An archive from before ADR-0028 has no bin/: install.sh would install
// nothing, restart the old binaries and record the new version.
func TestNodeRefusesArchiveWithoutBin(t *testing.T) {
	n := newFakeNode(t)
	n.release("v0.9.0", map[string]string{"orrery-agent": "old"})
	out, err := n.run("--version", "v0.9.0", "--runtime", "local")
	if err == nil || !strings.Contains(out, "ADR-0028") {
		t.Fatalf("an archive without bin/ did not stop the install: %v\n%s", err, out)
	}
	if _, err := os.Stat(filepath.Join(n.d, "conf", "version")); !os.IsNotExist(err) {
		t.Errorf("conf/version written: %v", err)
	}
	if l := n.log(); l != "" {
		t.Errorf("install went on:\n%s", l)
	}
}

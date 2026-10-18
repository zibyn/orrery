package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/zibyn/stars-sandbox/internal/nodeproto"
	"github.com/zibyn/stars-sandbox/internal/store"
)

func TestValidateBuildDockerfile(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	tests := []struct {
		name     string
		docker   string
		contexts map[string]bool
		hasBase  bool
		ok       bool
	}{
		{"base context", "FROM base\nRUN true\n", nil, true, true},
		{"base without field", "FROM base\n", nil, false, false},
		{"named context", "FROM ctx\nCOPY --from=ctx /x /x\n", map[string]bool{"ctx": true}, false, true},
		{"stage alias", "FROM base AS build\nRUN true\nFROM build\n", nil, true, true},
		{"numeric stage", "FROM base AS build\nFROM 0\n", nil, true, true},
		{"digest pinned", "FROM alpine@" + digest + "\n", nil, false, true},
		{"floating tag", "FROM alpine:3.22\n", nil, false, false},
		{"tag in copy from", "FROM base\nCOPY --from=alpine:3.22 /x /x\n", nil, true, false},
		{"context in mount from", "FROM base\nRUN --mount=type=bind,from=ctx,target=/c cat /c/x\n", map[string]bool{"ctx": true}, true, true},
		{"tag in mount from", "FROM base\nRUN --mount=type=bind,from=alpine:3.22,target=/c cat /c/x\n", nil, true, false},
		{"unresolved arg", "FROM ${BASE}\n", nil, true, false},
		{"scratch", "FROM scratch\n", nil, false, true},
		{"continuation", "FROM \\\n  base\nRUN true\n", nil, true, true},
		{"comment", "# comment\nFROM base\n", nil, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validateBuildDockerfile(tc.docker, tc.contexts, tc.hasBase)
			if tc.ok && err != nil {
				t.Fatalf("validateBuildDockerfile = %v, want nil", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("validateBuildDockerfile = nil, want an error")
			}
		})
	}
}

func TestParseContextRef(t *testing.T) {
	version := "01a14d20-9122-7087-a204-253b29c3ccc7"
	resource, ref := parseContextRef("assets@" + version)
	if resource != "assets" || ref == nil || *ref != version {
		t.Fatalf("parseContextRef = %q, %v", resource, ref)
	}
	// An '@' that does not end in a UUID belongs to the name.
	resource, ref = parseContextRef("team@assets")
	if resource != "team@assets" || ref != nil {
		t.Fatalf("parseContextRef = %q, %v", resource, ref)
	}
	resource, ref = parseContextRef("assets")
	if resource != "assets" || ref != nil {
		t.Fatalf("parseContextRef = %q, %v", resource, ref)
	}
}

func TestBuildCauseMapping(t *testing.T) {
	for cause, want := range map[string]string{
		"admission_timeout": "timed_out",
		"unsatisfiable":     "build_failed",
		"node_lost":         "node_lost",
		"":                  "build_failed",
	} {
		sb := store.Sandbox{State: string(SandboxFailed)}
		if cause != "" {
			sb.Cause = &cause
		}
		if got := versionCauseForSandbox(sb); got != want {
			t.Errorf("versionCauseForSandbox(%q) = %q, want %q", cause, got, want)
		}
	}
	for cause, want := range map[string]ExecCause{
		"timed_out": ExecTimedOut, "build_interrupted": ExecRuntimeLost,
		"node_lost": ExecNodeLost, "build_failed": ExecExited, "": ExecExited,
	} {
		if got := execCauseForVersionCause(cause); got != want {
			t.Errorf("execCauseForVersionCause(%q) = %q, want %q", cause, got, want)
		}
	}
	if got := buildFailureCause("nonsense"); got != "build_failed" {
		t.Errorf("buildFailureCause(nonsense) = %q", got)
	}
}

func TestBuildExpired(t *testing.T) {
	now := time.Now()
	started := pgtype.Timestamptz{Time: now.Add(-2 * time.Hour), Valid: true}
	row := store.ListBuildRunsRow{
		ExecAcceptedAt:     started,
		ExecTimeoutSeconds: i32p(3600),
	}
	if !buildExpired(row, now) {
		t.Fatal("a build older than its timeout must be expired")
	}
	row.ExecTimeoutSeconds = i32p(3 * 3600)
	if buildExpired(row, now) {
		t.Fatal("a build younger than its timeout is not expired")
	}
	deadline := pgtype.Timestamptz{Time: now.Add(-time.Second), Valid: true}
	row.ExecDeadlineAt = deadline
	if !buildExpired(row, now) {
		t.Fatal("an explicit deadline in the past must expire")
	}
}

func TestValidBuildName(t *testing.T) {
	for _, name := range []string{"ctx", "assets", "a.b-c_d", "A1"} {
		if !validBuildName(name) {
			t.Errorf("validBuildName(%q) = false", name)
		}
	}
	for _, name := range []string{"", "base", "a/b", "../x", "a b", "-x", strings.Repeat("x", 65)} {
		if validBuildName(name) {
			t.Errorf("validBuildName(%q) = true", name)
		}
	}
}

func TestAdmissionExhaustedRequiresSnapshotBuild(t *testing.T) {
	now := time.Now()
	node := readyNode(t, now)
	node.Hardware = []byte(`{"cpu_cores":8,"memory_bytes":34359738368,"disk_free":1000,"disk_total":1000}`)
	build := store.Sandbox{Kind: string(SandboxKindBuild), Cpu: 4, Memory: 8 << 30}
	if !admissionExhausted([]store.Node{node}, nil, build, nil) {
		t.Fatal("a Node without snapshot_build must make the build unsatisfiable")
	}
	// A Node that has not reported capabilities is not proof either way.
	unknown := node
	unknown.Capabilities = nil
	if admissionExhausted([]store.Node{unknown}, nil, build, nil) {
		t.Fatal("an unreported capability must keep the build queued")
	}
	// Enforced and large enough: the build is feasible.
	var report nodeproto.CapabilityReport
	if err := json.Unmarshal(capabilityReport(t, false), &report); err != nil {
		t.Fatal(err)
	}
	report.Entries = append(report.Entries, nodeproto.CapabilityEntry{
		Name: nodeproto.CapSnapshotBuild, Value: nodeproto.ValueEnforced,
	})
	capable := node
	capable.Capabilities, _ = json.Marshal(report)
	if admissionExhausted([]store.Node{capable}, nil, build, nil) {
		t.Fatal("a capable Node with room must not be exhausted")
	}
	if !admissionExhausted(nil, nil, build, nil) {
		t.Fatal("no Nodes at all means the build can never be placed")
	}
}

func i32p(v int32) *int32 { return &v }

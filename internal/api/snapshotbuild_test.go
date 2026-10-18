package api

import (
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

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
	for cause, want := range map[string]string{
		"timed_out": "timed_out", "build_interrupted": "runtime_lost",
		"node_lost": "node_lost", "build_failed": "exited", "": "exited",
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

func i32p(v int32) *int32 { return &v }

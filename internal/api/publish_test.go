package api

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestNormalizePublishPath(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{"root workspace prefix", "/workspace", ".", true},
		{"workspace prefix trailing slash", "/workspace/", ".", true},
		{"workspace prefix relative", "/workspace/src/app", "src/app", true},
		{"relative path", "src/app", "src/app", true},
		{"relative dot", ".", ".", true},
		{"empty means root", "", ".", true},
		{"clean dot segments", "./src/./app", "src/app", true},
		{"absolute outside workspace", "/etc/passwd", "", false},
		{"parent traversal", "../secrets", "", false},
		{"traversal inside", "src/../../secrets", "", false},
		{"workspace prefix traversal", "/workspace/../etc", "", false},
		{"NUL byte", "src\x00app", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := normalizePublishPath(tt.input)
			if tt.ok {
				if err != nil {
					t.Fatalf("normalizePublishPath(%q) error = %v", tt.input, err)
				}
				if got != tt.want {
					t.Fatalf("normalizePublishPath(%q) = %q, want %q", tt.input, got, tt.want)
				}
				return
			}
			if err == nil {
				t.Fatalf("normalizePublishPath(%q) = %q, want an error", tt.input, got)
			}
		})
	}
}

func TestWorkspaceProvenanceRoundTrip(t *testing.T) {
	workspaceID := uuid.MustParse("0192f000-0000-7000-8000-000000000002")
	raw := workspaceProvenance(workspaceID, "src/app")
	var decoded struct {
		Source    string `json:"source"`
		Workspace struct {
			ID   string `json:"id"`
			Path string `json:"path"`
		} `json:"workspace"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Source != "workspace" || decoded.Workspace.ID != workspaceID.String() || decoded.Workspace.Path != "src/app" {
		t.Fatalf("provenance = %s", raw)
	}
	id, path, ok := parseWorkspaceProvenance(raw)
	if !ok || id != workspaceID || path != "src/app" {
		t.Fatalf("parseWorkspaceProvenance = (%v, %q, %v)", id, path, ok)
	}
	if _, _, ok := parseWorkspaceProvenance(json.RawMessage(`{"source":"upload"}`)); ok {
		t.Fatal("upload provenance parsed as a workspace publish")
	}
}

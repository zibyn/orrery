package api

import "testing"

func TestNormalizePublishPath(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
		ok    bool
	}{
		{"/workspace itself", "/workspace", "/workspace", true},
		{"/workspace/", "/workspace/", "/workspace", true},
		{"under /workspace", "/workspace/src/app", "/workspace/src/app", true},
		{"root filesystem path", "/etc/app/", "/etc/app", true},
		{"root itself", "/", "/", true},
		{"relative path", "src/app", "src/app", true},
		{"relative dot", ".", ".", true},
		{"empty means root", "", ".", true},
		{"clean dot segments", "./src/./app", "src/app", true},
		{"parent traversal", "../secrets", "", false},
		{"traversal inside", "src/../../secrets", "", false},
		{"absolute traversal", "/workspace/../etc", "", false},
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

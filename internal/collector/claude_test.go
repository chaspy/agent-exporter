package collector

import (
	"path/filepath"
	"testing"
)

func TestExtractClaudeProject(t *testing.T) {
	t.Parallel()

	projectsDir := filepath.Join(string(filepath.Separator), "Users", "chaspy", ".claude", "projects")

	tests := []struct {
		name string
		path string
		want string
	}{
		{
			name: "github encoded path",
			path: filepath.Join(
				projectsDir,
				"-Users-chaspy-go-src-github-com-chaspy-myassistant-worktree-feat-xxx",
				"1234.jsonl",
			),
			want: "chaspy/myassistant",
		},
		{
			name: "non github path",
			path: filepath.Join(
				projectsDir,
				"-Users-chaspy-go-src-example-com-chaspy-myassistant",
				"1234.jsonl",
			),
			want: "unknown",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := extractClaudeProject(tt.path, projectsDir); got != tt.want {
				t.Fatalf("extractClaudeProject(%q, %q) = %q, want %q", tt.path, projectsDir, got, tt.want)
			}
		})
	}
}

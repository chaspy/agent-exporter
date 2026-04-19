package collector

import "testing"

func TestExtractRepositoryFromGitOrigin(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		origin string
		want   string
	}{
		{
			name:   "https github url",
			origin: "https://github.com/chaspy/myassistant.git",
			want:   "chaspy/myassistant",
		},
		{
			name:   "ssh github url",
			origin: "git@github.com:chaspy/myassistant.git",
			want:   "chaspy/myassistant",
		},
		{
			name:   "invalid url",
			origin: "not-a-url",
			want:   "unknown",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := extractRepositoryFromGitOrigin(tt.origin); got != tt.want {
				t.Fatalf("extractRepositoryFromGitOrigin(%q) = %q, want %q", tt.origin, got, tt.want)
			}
		})
	}
}

func TestExtractRepositoryFromCWD(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cwd  string
		want string
	}{
		{
			name: "repo root",
			cwd:  "/Users/chaspy/go/src/github.com/chaspy/myassistant",
			want: "chaspy/myassistant",
		},
		{
			name: "worktree path",
			cwd:  "/Users/chaspy/go/src/github.com/chaspy/myassistant/worktree-main",
			want: "chaspy/myassistant",
		},
		{
			name: "non github cwd",
			cwd:  "/tmp/sandbox",
			want: "unknown",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := extractRepositoryFromCWD(tt.cwd); got != tt.want {
				t.Fatalf("extractRepositoryFromCWD(%q) = %q, want %q", tt.cwd, got, tt.want)
			}
		})
	}
}

func TestNormalizeCodexModel(t *testing.T) {
	t.Parallel()

	if got := normalizeCodexModel("", "openai"); got != "openai" {
		t.Fatalf("normalizeCodexModel fallback = %q, want %q", got, "openai")
	}

	if got := normalizeCodexModel("gpt-5.4", "openai"); got != "gpt-5.4" {
		t.Fatalf("normalizeCodexModel explicit = %q, want %q", got, "gpt-5.4")
	}
}

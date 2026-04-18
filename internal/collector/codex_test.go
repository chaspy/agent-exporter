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

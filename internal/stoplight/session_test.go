package stoplight

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSessionDisplay(t *testing.T) {
	tests := []struct {
		name     string
		label    string
		override string
		want     string
	}{
		{"label only", "auth-api", "", "auth-api"},
		{"override wins", "auth-api", "deploy", "deploy"},
		{"override only", "", "deploy", "deploy"},
		{"neither", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			session := &Session{Label: tt.label, Override: tt.override}
			if got := session.Display(); got != tt.want {
				t.Errorf("Display() = %q, want %q", got, tt.want)
			}
		})
	}
}

// writeGitHEAD builds a directory with a .git/HEAD holding the given contents.
func writeGitHEAD(t *testing.T, contents string) string {
	t.Helper()
	dir := t.TempDir()
	gitDir := filepath.Join(dir, ".git")
	if err := os.MkdirAll(gitDir, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte(contents), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return dir
}

func TestDeriveLabelFromGitBranch(t *testing.T) {
	tests := []struct {
		name string
		head string
		want string
	}{
		{"plain branch", "ref: refs/heads/main\n", "main"},
		{"no trailing newline", "ref: refs/heads/main", "main"},
		{"feature prefix stripped", "ref: refs/heads/feature/auth-api\n", "auth-api"},
		{"bugfix prefix stripped", "ref: refs/heads/bugfix/leak\n", "leak"},
		{"hotfix prefix stripped", "ref: refs/heads/hotfix/urgent\n", "urgent"},
		{"worktree prefix stripped", "ref: refs/heads/worktree-readme\n", "readme"},
		{"stacked prefixes stripped", "ref: refs/heads/worktree-feature/auth\n", "auth"},
		{"inner slash kept", "ref: refs/heads/team/auth\n", "team/auth"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := writeGitHEAD(t, tt.head)
			if got := DeriveLabel(dir, "claude-code", "s1"); got != tt.want {
				t.Errorf("DeriveLabel() = %q, want %q", got, tt.want)
			}
		})
	}
}

// A detached HEAD holds a bare SHA, which is not a name worth showing, so
// derivation falls through to the directory name.
func TestDeriveLabelDetachedHeadFallsBackToDir(t *testing.T) {
	dir := writeGitHEAD(t, "9fceb02d0ae598e95dc970b74767f19372d61af8\n")
	want := filepath.Base(dir)
	if got := DeriveLabel(dir, "claude-code", "s1"); got != want {
		t.Errorf("DeriveLabel() = %q, want %q", got, want)
	}
}

// A linked worktree has a .git file pointing at the real git directory.
func TestDeriveLabelFindsBranchThroughWorktreeFile(t *testing.T) {
	root := t.TempDir()
	realGit := filepath.Join(root, "gitdir")
	if err := os.MkdirAll(realGit, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(realGit, "HEAD"), []byte("ref: refs/heads/wt\n"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	work := filepath.Join(root, "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	pointer := []byte("gitdir: " + realGit + "\n")
	if err := os.WriteFile(filepath.Join(work, ".git"), pointer, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if got := DeriveLabel(work, "", "s1"); got != "wt" {
		t.Errorf("DeriveLabel() = %q, want %q", got, "wt")
	}
}

// A subdirectory of a repository still derives the repository's branch.
func TestDeriveLabelWalksUpToGitDir(t *testing.T) {
	dir := writeGitHEAD(t, "ref: refs/heads/main\n")
	nested := filepath.Join(dir, "internal", "stoplight")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if got := DeriveLabel(nested, "", "s1"); got != "main" {
		t.Errorf("DeriveLabel() = %q, want %q", got, "main")
	}
}

// The fallback order from RFC 1 section 9, exercised where no git repo exists.
func TestDeriveLabelFallbackOrder(t *testing.T) {
	// A directory with no repository anywhere above it.
	noRepo := t.TempDir()
	nested := filepath.Join(noRepo, "payments")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	tests := []struct {
		name      string
		cwd       string
		provider  string
		sessionID string
		want      string
	}{
		{"directory name when no branch", nested, "claude-code", "s1", "payments"},
		{"provider when no cwd", "", "claude-code", "s1", "claude-code"},
		{"session id when no cwd or provider", "", "", "s1", "s1"},
		{"prefix stripped from provider", "", "worktree-ci", "s1", "ci"},
		{"prefix stripped from session id", "", "", "feature/s1", "s1"},
		{"everything empty", "", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DeriveLabel(tt.cwd, tt.provider, tt.sessionID)
			if got != tt.want {
				t.Errorf("DeriveLabel(%q, %q, %q) = %q, want %q",
					tt.cwd, tt.provider, tt.sessionID, got, tt.want)
			}
		})
	}
}

// A prefix that would leave nothing behind is kept, because an empty label is
// worse on screen than a noisy one.
func TestDeriveLabelKeepsPrefixWhenNothingWouldRemain(t *testing.T) {
	if got := DeriveLabel("", "", "feature/"); got != "feature/" {
		t.Errorf("DeriveLabel() = %q, want %q", got, "feature/")
	}
}

func TestStripNoise(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"no prefix", "auth-api", "auth-api"},
		{"feature", "feature/auth", "auth"},
		{"bugfix", "bugfix/auth", "auth"},
		{"hotfix", "hotfix/auth", "auth"},
		{"worktree", "worktree-auth", "auth"},
		{"stacked", "feature/worktree-auth", "auth"},
		{"only mid-string is untouched", "my-feature/auth", "my-feature/auth"},
		{"empty", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := stripNoise(tt.input); got != tt.want {
				t.Errorf("stripNoise(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

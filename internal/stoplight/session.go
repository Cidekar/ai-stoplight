package stoplight

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Session is one unit of work the relay is tracking. It may come from any
// producer: an LLM CLI, a build, a script.
type Session struct {
	ID       string    // opaque, stable for the life of the session
	Label    string    // derived from cwd, or sent by the producer
	Override string    // set by `stoplight task`, wins over Label
	Provider string    // free text: claude-code, deepseek, ci, ...
	State    State     // where the session is in its lifecycle
	Started  time.Time // fixes the rotation order
	LastSeen time.Time // drives the timeout
	Dir      string    // working directory, for label derivation

	// labelSent records that Label came from the producer rather than from
	// DeriveLabel. A derived label is recomputed when its inputs move, so that
	// a first report with no cwd does not freeze the session-ID fallback in
	// place when a later report finally supplies one. A sent label is never
	// recomputed: the producer said what it wanted called.
	//
	// Unexported: this is bookkeeping for the tracker, not part of the session
	// a caller reads.
	labelSent bool
}

// Display is the label actually shown, override first.
func (s *Session) Display() string {
	if s.Override != "" {
		return s.Override
	}
	return s.Label
}

// noisePrefixes are stripped from a derived label before display. A screen
// showing ten characters cannot afford to spend eight on "feature/".
var noisePrefixes = []string{"feature/", "bugfix/", "hotfix/", "worktree-"}

// DeriveLabel builds a label for a session that sent none, per RFC 1 section 9:
// the git branch of cwd, else the directory name, else the provider, else the
// session ID. Noise prefixes are stripped from whichever one wins.
func DeriveLabel(cwd, provider, sessionID string) string {
	if cwd != "" {
		if branch, ok := gitBranch(cwd); ok {
			return stripNoise(branch)
		}
		if base := baseName(cwd); base != "" {
			return stripNoise(base)
		}
	}
	if provider != "" {
		return stripNoise(provider)
	}
	return stripNoise(sessionID)
}

// baseName returns the final element of a path, or "" when the path names no
// directory in particular (root, ".", or empty).
func baseName(dir string) string {
	base := filepath.Base(filepath.Clean(dir))
	if base == "." || base == string(filepath.Separator) || base == "" {
		return ""
	}
	return base
}

// gitBranch reads the checked-out branch of the repository containing dir. It
// parses .git/HEAD directly rather than shelling out to git, because this runs
// on every label derivation and the relay must not depend on git being
// installed. A detached HEAD holds a bare SHA and yields no branch name.
func gitBranch(dir string) (string, bool) {
	gitDir, ok := findGitDir(dir)
	if !ok {
		return "", false
	}
	data, err := os.ReadFile(filepath.Join(gitDir, "HEAD"))
	if err != nil {
		return "", false
	}
	ref, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "ref: refs/heads/")
	if !ok || ref == "" {
		return "", false
	}
	return ref, true
}

// findGitDir walks up from dir looking for .git. It handles the worktree case,
// where .git is a file holding "gitdir: <path>" rather than a directory.
func findGitDir(dir string) (string, bool) {
	current, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	for {
		candidate := filepath.Join(current, ".git")
		info, err := os.Stat(candidate)
		switch {
		case err != nil:
			// keep walking up
		case info.IsDir():
			return candidate, true
		default:
			if resolved, ok := readGitFile(candidate); ok {
				return resolved, true
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", false
		}
		current = parent
	}
}

// readGitFile resolves the "gitdir: <path>" pointer a linked worktree uses in
// place of a .git directory.
func readGitFile(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
	if !ok || target == "" {
		return "", false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	return target, true
}

// stripNoise removes branch-naming prefixes that carry no information on a
// ten-character screen. Prefixes are stripped repeatedly, so
// "worktree-feature/auth" reduces to "auth".
func stripNoise(label string) string {
	for changed := true; changed; {
		changed = false
		for _, prefix := range noisePrefixes {
			if trimmed, ok := strings.CutPrefix(label, prefix); ok && trimmed != "" {
				label = trimmed
				changed = true
			}
		}
	}
	return label
}

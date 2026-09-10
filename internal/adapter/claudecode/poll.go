package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/cidekar/stoplight/internal/stoplight"
)

// ProviderName is what this adapter's sessions are recorded under. Hooks and
// polls must agree on it: reconciliation is scoped by provider, so a sync
// under a different name would decline to remove the very sessions the hooks
// created. See RFC 1 section 5.4.
const ProviderName = "claude-code"

// pollCommand is the documented interface for asking Claude Code what it has
// running. `claude agents --json` is a supported, stable command.
//
// The on-disk alternatives are deliberately not used. The daemon roster under
// ~/.claude/ carries richer detail, including a field that marks a pre-warmed
// spare directly, but the documentation states plainly that the layout is not
// a stable interface. A light that breaks on a Claude Code point release is
// worse than a light that cannot name a spare.
var pollCommand = []string{"claude", "agents", "--json"}

// searchPaths are checked for the agent binary when PATH does not resolve it,
// in order of preference.
//
// The relay normally runs as a launchd or systemd service, and a service does
// not inherit the PATH of the shell that installed it. Claude Code installs to
// ~/.local/bin, which is on an interactive PATH and absent from a service's:
// the poll failed with "executable file not found in $PATH" for exactly this
// reason, on a machine where typing `claude` worked perfectly.
//
// Depending on the service manager's PATH would make polling work or not by
// accident of how the relay was started, which is not a property a light
// should have. Resolving the binary here makes it the same either way.
var searchPaths = []string{
	"$HOME/.local/bin",
	"/opt/homebrew/bin",
	"/usr/local/bin",
	"/usr/bin",
}

// resolveAgent returns a path to the agent binary, preferring PATH and falling
// back to the known install locations.
//
// It returns the bare name unchanged when nothing is found, so the failure is
// reported by exec with its own message rather than by a guess made here.
func resolveAgent(name string) string {
	if filepath.IsAbs(name) {
		return name
	}
	if found, err := exec.LookPath(name); err == nil {
		return found
	}
	for _, dir := range searchPaths {
		candidate := filepath.Join(os.ExpandEnv(dir), name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return name
}

// pollTimeout bounds one query. Querying an agent is slower than a hook by
// orders of magnitude, and the relay's ticker must not stack up behind a
// wedged child process.
const pollTimeout = 10 * time.Second

// agentEntry is one session as `claude agents --json` reports it. Only the
// fields this adapter maps are declared; the command emits more, and unknown
// fields are ignored the way the rest of the protocol requires.
type agentEntry struct {
	SessionID string `json:"sessionId"`
	Cwd       string `json:"cwd"`
	Name      string `json:"name"`
	State     string `json:"state"`
}

// Provider implements adapter.Poller.
func (a *Adapter) Provider() string { return ProviderName }

// Poll asks Claude Code for every session it considers live and translates the
// answer into protocol events. It implements adapter.Poller.
//
// The translation is the whole vendor boundary: what comes back is Claude
// Code's vocabulary, and what leaves is RFC 1's. Nothing above this function
// learns that a poll happened, or that the word "agents" exists.
//
// The observation time is taken BEFORE the command runs. A query that takes
// two seconds describes the world as it was when asked, not when it answered,
// and stamping the later time would let a slow poll outrank a hook that fired
// while it was still running.
func (a *Adapter) Poll(ctx context.Context) ([]stoplight.Report, time.Time, error) {
	observedAt := time.Now()

	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, resolveAgent(pollCommand[0]), pollCommand[1:]...).Output()
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("%s: %w", pollCommand[0], err)
	}

	var entries []agentEntry
	if err := json.Unmarshal(out, &entries); err != nil {
		return nil, time.Time{}, fmt.Errorf("decode %s output: %w", pollCommand[0], err)
	}

	reports := make([]stoplight.Report, 0, len(entries))
	for _, entry := range entries {
		if entry.SessionID == "" {
			// Without an ID there is nothing to track across polls and
			// nothing the pin or a rotation slot could address.
			continue
		}
		event, ok := eventForState(entry.State)
		if !ok {
			// A state this adapter does not know. Skipping the entry would
			// declare the session absent and end it, so it is reported as
			// working instead: an agent that exists and is doing something
			// unrecognised is still an agent that exists.
			event = "started"
		}
		reports = append(reports, stoplight.Report{
			SessionID: entry.SessionID,
			Event:     event,
			Label:     entry.Name,
			Cwd:       entry.Cwd,
			Provider:  ProviderName,
		})
	}
	return reports, observedAt, nil
}

// eventForState maps a Claude Code session state onto an RFC 1 event.
//
// This is the second half of the table in RFC 1 section 10.2. The hook mapping
// translates events as they happen; this translates a state observed at rest,
// and the two must agree about what a colour means or a poll would fight the
// hooks it is meant to correct.
func eventForState(state string) (string, bool) {
	switch state {
	case "working":
		return "started", true
	case "blocked":
		return "blocked", true
	case "done":
		return "finished", true
	case "failed", "stopped":
		// Both are over, and neither is waiting for a human. `finished` is
		// green, which is what "nothing needs you" looks like. The screen
		// cannot show why a session ended and the lamp must not imply a
		// human is needed when none is.
		return "finished", true
	default:
		return "", false
	}
}

package claudecode

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The state table from RFC 1 section 10.2, asserted the way the hook mapping
// is: this is the second place a vendor's vocabulary is allowed to appear, so
// it is guarded rather than trusted.
func TestStateTableMatchesTheRFC(t *testing.T) {
	want := map[string]string{
		"working": "started",
		"blocked": "blocked",
		"done":    "finished",
		"failed":  "finished",
		"stopped": "finished",
	}
	for state, wantEvent := range want {
		got, ok := eventForState(state)
		if !ok {
			t.Errorf("eventForState(%q) is unknown, but the RFC lists it", state)
			continue
		}
		if got != wantEvent {
			t.Errorf("eventForState(%q) = %q, want %q per RFC 1 section 10.2", state, got, wantEvent)
		}
	}
}

// The state table. A poll observes a state at rest where a hook reports an
// event as it happens, and the two must agree about what a colour means or the
// poll would fight the hooks it exists to correct.
func TestEventForState(t *testing.T) {
	tests := []struct {
		state string
		want  string
		known bool
	}{
		{"working", "started", true},
		{"blocked", "blocked", true},
		{"done", "finished", true},
		{"failed", "finished", true},
		{"stopped", "finished", true},
		{"", "", false},
		{"something-new", "", false},
	}

	for _, tt := range tests {
		got, ok := eventForState(tt.state)
		if ok != tt.known {
			t.Errorf("eventForState(%q) known = %v, want %v", tt.state, ok, tt.known)
		}
		if got != tt.want {
			t.Errorf("eventForState(%q) = %q, want %q", tt.state, got, tt.want)
		}
	}
}

// Neither `failed` nor `stopped` may map to blocked. Both are over, and
// blocked is red, which means a human is needed: a session that failed while
// nobody watched must not claim the desk's attention forever.
func TestFailedAndStoppedAreNotRed(t *testing.T) {
	for _, state := range []string{"failed", "stopped", "done"} {
		if got, _ := eventForState(state); got == "blocked" {
			t.Errorf("eventForState(%q) = blocked, want a non-red event", state)
		}
	}
}

// The poller reports the provider its hooks use. Reconciliation is scoped by
// provider, so a mismatch would mean a sync that declines to remove the very
// sessions the hooks created.
func TestProviderMatchesTheHooks(t *testing.T) {
	a := New()
	if got := a.Provider(); got != ProviderName {
		t.Errorf("Provider() = %q, want %q", got, ProviderName)
	}

	// The hooks reach the relay through `stoplight notify`, which stamps the
	// provider itself, so the hook command carries the verb rather than the
	// name. Assert the verb is there: if notify stopped being the path, the
	// provider agreement asserted above would need rechecking by hand.
	if cmd := hookCommand("/usr/local/bin/stoplight", "started"); !strings.Contains(cmd, "notify started") {
		t.Errorf("hook command does not invoke notify: %q", cmd)
	}
}

// The relay runs as a service, and a service does not inherit the PATH of the
// shell that installed it. Claude Code lives in ~/.local/bin, which is on an
// interactive PATH and absent from launchd's, so the poll failed with
// "executable file not found in $PATH" on a machine where typing `claude`
// worked. Resolution must not depend on how the relay was started.
func TestResolveAgentFindsABinaryOutsidePath(t *testing.T) {
	dir := t.TempDir()
	name := "fake-agent-binary"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}

	// An empty PATH is the worst case a service can hand us.
	t.Setenv("PATH", "")

	restore := searchPaths
	t.Cleanup(func() { searchPaths = restore })
	searchPaths = []string{dir}

	if got := resolveAgent(name); got != path {
		t.Errorf("resolveAgent(%q) = %q, want %q", name, got, path)
	}
}

// PATH still wins when it resolves, so a user with a deliberate override is
// not second-guessed by the fallback list.
func TestResolveAgentPrefersPath(t *testing.T) {
	onPath := t.TempDir()
	fallback := t.TempDir()
	name := "fake-agent-pref"

	for _, dir := range []string{onPath, fallback} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatalf("write fake binary: %v", err)
		}
	}

	t.Setenv("PATH", onPath)
	restore := searchPaths
	t.Cleanup(func() { searchPaths = restore })
	searchPaths = []string{fallback}

	if got := resolveAgent(name); got != filepath.Join(onPath, name) {
		t.Errorf("resolveAgent(%q) = %q, want the PATH copy in %q", name, got, onPath)
	}
}

// Nothing found returns the bare name, so exec reports the failure with its
// own message rather than this function inventing a path that does not exist.
func TestResolveAgentFallsBackToTheBareName(t *testing.T) {
	t.Setenv("PATH", "")
	restore := searchPaths
	t.Cleanup(func() { searchPaths = restore })
	searchPaths = []string{t.TempDir()}

	if got := resolveAgent("definitely-not-installed"); got != "definitely-not-installed" {
		t.Errorf("resolveAgent() = %q, want the name unchanged", got)
	}
}

// A directory that happens to share the binary's name is not the binary, and
// neither is a file nobody can execute.
func TestResolveAgentSkipsNonExecutables(t *testing.T) {
	dir := t.TempDir()
	name := "fake-agent-skip"

	// A directory with the right name.
	if err := os.Mkdir(filepath.Join(dir, name), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// And a non-executable file, in a later directory.
	second := t.TempDir()
	if err := os.WriteFile(filepath.Join(second, name), []byte("data"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	t.Setenv("PATH", "")
	restore := searchPaths
	t.Cleanup(func() { searchPaths = restore })
	searchPaths = []string{dir, second}

	if got := resolveAgent(name); got != name {
		t.Errorf("resolveAgent() = %q, want the bare name: neither candidate is runnable", got)
	}
}

// The hooks must identify their provider. Without it every session they
// created was unattributable, and an unattributed session could not be
// reconciled by anyone: pre-warmed workers that fire one event and are never
// dispatched piled up forever, and one holding a red state kept the lamp red
// while every real session was fine.
//
// The provider is also what scopes a sync, so the hooks and the poll have to
// agree on the string or the poll would decline to reap what the hooks made.
func TestHookCommandSendsTheProvider(t *testing.T) {
	for _, event := range []string{"idle", "started", "blocked", "finished", "ended"} {
		cmd := hookCommand("/usr/local/bin/stoplight", event)
		if !strings.Contains(cmd, "--provider "+ProviderName) {
			t.Errorf("hookCommand(%q) does not pass --provider %s: %q", event, ProviderName, cmd)
		}
	}
}

// An untitled session reports its own ID as its name, and a hex ID is the
// worst thing a seven-character screen can show. Reporting it as a label would
// also freeze it: RFC 1 section 9 never recomputes a label a producer sent, so
// the session could not pick up a better one later.
//
// The cases are taken from a real desk. 339d206f was blocked and waiting for
// input while showing "339d206f", when its worktree was called
// chore+vitest-align-3-2-7.
func TestSessionLabelDropsAnUntitledName(t *testing.T) {
	tests := []struct {
		name  string
		entry agentEntry
		want  string
	}{
		{
			name:  "a titled session keeps its name",
			entry: agentEntry{SessionID: "c37f76a7-9fae-4255-9fc0-5af63f485628", Name: "project state review"},
			want:  "project state review",
		},
		{
			name:  "a name equal to the short ID is not a name",
			entry: agentEntry{SessionID: "339d206f-635c-4d6b-98e1-2d3726537cd2", Name: "339d206f"},
			want:  "",
		},
		{
			name:  "a name equal to the full ID is not a name",
			entry: agentEntry{SessionID: "2026a22b-2c7e-44bb-a622-139fed94f1ed", Name: "2026a22b-2c7e-44bb-a622-139fed94f1ed"},
			want:  "",
		},
		{
			name:  "an empty name is nothing to show",
			entry: agentEntry{SessionID: "e3f7acef-2bfc-4abe-a667-2496b6102416", Name: ""},
			want:  "",
		},
		{
			name:  "a name that merely starts like the ID is still a name",
			entry: agentEntry{SessionID: "339d206f-635c-4d6b-98e1-2d3726537cd2", Name: "339d206f rebase"},
			want:  "339d206f rebase",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sessionLabel(tc.entry); got != tc.want {
				t.Errorf("sessionLabel() = %q, want %q", got, tc.want)
			}
		})
	}
}

// End to end: an untitled entry reaches the relay with no label but with its
// cwd, which is what lets derivation name it.
func TestPollLeavesUntitledSessionsForDerivation(t *testing.T) {
	restore := pollCommand
	t.Cleanup(func() { pollCommand = restore })

	pollCommand = fakeCommand(t, `[
	  {"sessionId":"339d206f-635c-4d6b-98e1-2d3726537cd2","cwd":"/repo/.claude/worktrees/chore+vitest-align-3-2-7","name":"339d206f","state":"blocked"},
	  {"sessionId":"c37f76a7-9fae-4255-9fc0-5af63f485628","cwd":"/repo","name":"project state review","state":"working"}
	]`)

	sessions, _, err := New().Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll() error = %v", err)
	}

	byID := map[string]string{}
	cwdByID := map[string]string{}
	for _, s := range sessions {
		byID[s.SessionID] = s.Label
		cwdByID[s.SessionID] = s.Cwd
	}

	if got := byID["339d206f-635c-4d6b-98e1-2d3726537cd2"]; got != "" {
		t.Errorf("untitled session label = %q, want empty so the relay derives it", got)
	}
	if got := cwdByID["339d206f-635c-4d6b-98e1-2d3726537cd2"]; got == "" {
		t.Error("untitled session cwd is empty: derivation has nothing to work from")
	}
	if got := byID["c37f76a7-9fae-4255-9fc0-5af63f485628"]; got != "project state review" {
		t.Errorf("titled session label = %q, want it preserved", got)
	}
}

// A poll that cannot run is an error, not an empty answer. RFC 1 section 5.4
// makes the difference load-bearing: empty means remove everything, and no
// answer means leave it alone. Conflating them clears the whole light every
// time the agent binary is missing.
func TestPollFailureIsAnErrorNotAnEmptyList(t *testing.T) {
	restore := pollCommand
	t.Cleanup(func() { pollCommand = restore })

	// A command that cannot exist.
	pollCommand = []string{filepath.Join(t.TempDir(), "no-such-binary")}

	a := New()
	sessions, observedAt, err := a.Poll(context.Background())
	if err == nil {
		t.Fatal("Poll() error = nil, want an error when the command cannot run")
	}
	if sessions != nil {
		t.Errorf("Poll() sessions = %v, want nil on error", sessions)
	}
	if !observedAt.IsZero() {
		t.Errorf("Poll() observedAt = %v, want the zero time on error", observedAt)
	}
}

// Output that is not the expected JSON is an error for the same reason.
func TestPollRejectsUnparseableOutput(t *testing.T) {
	restore := pollCommand
	t.Cleanup(func() { pollCommand = restore })

	pollCommand = fakeCommand(t, "not json at all")

	a := New()
	if _, _, err := a.Poll(context.Background()); err == nil {
		t.Fatal("Poll() error = nil, want an error on unparseable output")
	}
}

// The translation. What comes back is Claude Code's vocabulary; what leaves is
// RFC 1's, with the session name carried through as the label because a path
// leaf is a poor thing to read on a screen that holds seven characters.
func TestPollTranslatesToProtocolEvents(t *testing.T) {
	restore := pollCommand
	t.Cleanup(func() { pollCommand = restore })

	pollCommand = fakeCommand(t, `[
	  {"sessionId":"a1","cwd":"/tmp/one","name":"auth refactor","state":"working"},
	  {"sessionId":"b2","cwd":"/tmp/two","name":"deploy","state":"blocked"},
	  {"sessionId":"c3","cwd":"/tmp/three","name":"tests","state":"done"},
	  {"sessionId":"","cwd":"/tmp/four","name":"no id","state":"working"},
	  {"sessionId":"e5","cwd":"/tmp/five","name":"unknown","state":"brand-new-state"}
	]`)

	a := New()
	sessions, observedAt, err := a.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll() error = %v", err)
	}
	if observedAt.IsZero() {
		t.Error("Poll() observedAt is zero, want the time the query was taken")
	}

	// The entry with no session ID is dropped: nothing could track it across
	// polls or address it with a pin.
	if len(sessions) != 4 {
		t.Fatalf("len(sessions) = %d, want 4", len(sessions))
	}

	want := map[string]struct{ event, label string }{
		"a1": {"started", "auth refactor"},
		"b2": {"blocked", "deploy"},
		"c3": {"finished", "tests"},
		// An unrecognised state reports as working rather than being dropped.
		// Dropping it would declare the session absent and end it.
		"e5": {"started", "unknown"},
	}
	for _, s := range sessions {
		w, ok := want[s.SessionID]
		if !ok {
			t.Errorf("unexpected session %q", s.SessionID)
			continue
		}
		if s.Event != w.event {
			t.Errorf("session %s event = %q, want %q", s.SessionID, s.Event, w.event)
		}
		if s.Label != w.label {
			t.Errorf("session %s label = %q, want %q", s.SessionID, s.Label, w.label)
		}
		if s.Provider != ProviderName {
			t.Errorf("session %s provider = %q, want %q", s.SessionID, s.Provider, ProviderName)
		}
		if s.Cwd == "" {
			t.Errorf("session %s cwd is empty, want it carried through", s.SessionID)
		}
	}
}

// An agent with nothing running answers with an empty array, which is a valid
// declaration and not an error: it is how a quiet desk reports itself.
func TestPollEmptyListIsNotAnError(t *testing.T) {
	restore := pollCommand
	t.Cleanup(func() { pollCommand = restore })

	pollCommand = fakeCommand(t, `[]`)

	a := New()
	sessions, _, err := a.Poll(context.Background())
	if err != nil {
		t.Fatalf("Poll() error = %v, want nil", err)
	}
	if len(sessions) != 0 {
		t.Errorf("len(sessions) = %d, want 0", len(sessions))
	}
}

// fakeCommand writes a script that prints out and returns a command line that
// runs it, so a poll can be tested without Claude Code installed.
func fakeCommand(t *testing.T, out string) []string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-agents")
	script := "#!/bin/sh\ncat <<'STOPLIGHT_EOF'\n" + out + "\nSTOPLIGHT_EOF\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake command: %v", err)
	}
	return []string{path}
}

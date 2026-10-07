package claudecode

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/cidekar/stoplight/internal/adapter"
)

func TestName(t *testing.T) {
	if got := New().Name(); got != "claude-code" {
		t.Errorf("Name = %q, want claude-code", got)
	}
}

func TestRegisteredAtInit(t *testing.T) {
	// cmd.go drives every adapter through adapter.All, so registration is
	// what makes this package reachable at all.
	a, ok := adapter.Get("claude-code")
	if !ok {
		t.Fatal("claude-code is not registered")
	}
	if a.Name() != "claude-code" {
		t.Errorf("Name = %q, want claude-code", a.Name())
	}
}

func TestMappingMatchesTheRFC(t *testing.T) {
	// The table in RFC 1 section 10.1. This test is the guard on the one
	// place a vendor's vocabulary is allowed to appear.
	want := []hookMapping{
		{Hook: "SessionStart", Event: "idle", Matcher: "startup"},
		{Hook: "UserPromptSubmit", Event: "started"},
		{Hook: "PreToolUse", Event: "started"},
		{Hook: "Notification", Event: "blocked", Matcher: "permission_prompt"},
		{Hook: "Stop", Event: "finished"},
		{Hook: "SessionEnd", Event: "ended"},
	}
	if len(mappings) != len(want) {
		t.Fatalf("got %d mappings, want %d", len(mappings), len(want))
	}
	for i, w := range want {
		if mappings[i] != w {
			t.Errorf("mapping %d = %+v, want %+v", i, mappings[i], w)
		}
	}
}

func TestHookCommandShape(t *testing.T) {
	cmd := hookCommand("/usr/local/bin/stoplight", "blocked")

	for _, want := range []string{
		marker,
		"notify blocked",
		"--session-id",
		"--cwd",
		"|| true",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("the command is missing %q:\n%s", want, cmd)
		}
	}
}

func TestHookCommandQuotesThePath(t *testing.T) {
	// An install under "/Applications/My Tools/" must not split into two
	// arguments.
	cmd := hookCommand("/Users/a b/bin/stoplight", "idle")
	if !strings.Contains(cmd, `'/Users/a b/bin/stoplight'`) {
		t.Errorf("the path is not quoted:\n%s", cmd)
	}
}

func TestShellQuote(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"/usr/bin/stoplight", `'/usr/bin/stoplight'`},
		{"/a b/stoplight", `'/a b/stoplight'`},
		{"/it's/stoplight", `'/it'\''s/stoplight'`},
		{"", `''`},
	}
	for _, tc := range tests {
		if got := shellQuote(tc.in); got != tc.want {
			t.Errorf("shellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestHookCommandExtractsSessionID runs the generated command in a real
// shell against a real Claude Code payload.
//
// This is the test that matters most for correctness at runtime: there is
// no CLAUDE_SESSION_ID environment variable, so if the stdin parsing is
// wrong every session collapses into one and the relay cannot tell them
// apart.
func TestHookCommandExtractsSessionID(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hook command is POSIX shell")
	}

	dir := t.TempDir()
	out := filepath.Join(dir, "args")

	// A stub standing in for the stoplight binary. It records the
	// arguments it was called with.
	stub := filepath.Join(dir, "stoplight")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + out + "\n"
	if err := os.WriteFile(stub, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	payload := `{"session_id":"abc123","transcript_path":"/tmp/t.jsonl",` +
		`"cwd":"/Users/me/projects/auth-api","hook_event_name":"Notification"}`

	cmd := exec.Command("/bin/sh", "-c", hookCommand(stub, "blocked"))
	cmd.Stdin = strings.NewReader(payload)
	if combined, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run the hook: %v: %s", err, combined)
	}

	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("the stub was never called: %v", err)
	}
	want := "notify\nblocked\n--session-id\nabc123\n--cwd\n" +
		"/Users/me/projects/auth-api\n--provider\n" + ProviderName + "\n"
	if string(got) != want {
		t.Errorf("arguments =\n%q\nwant\n%q", got, want)
	}
}

// TestHookCommandSurvivesAMissingRelay checks the rule from RFC 1 section
// 6: a status light must never break its caller. A hook whose binary is
// absent must still exit 0, or Claude Code reports a hook failure to the
// user on every prompt.
func TestHookCommandSurvivesAMissingRelay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the hook command is POSIX shell")
	}

	cmd := exec.Command("/bin/sh", "-c", hookCommand("/nonexistent/stoplight", "started"))
	cmd.Stdin = strings.NewReader(`{"session_id":"x","cwd":"/tmp"}`)
	if err := cmd.Run(); err != nil {
		t.Errorf("the hook exited non-zero when the binary was missing: %v", err)
	}
}

func TestInstallWritesEveryMappedHook(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	a := newForPath(path)
	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}

	got := read(t, path)
	for _, m := range mappings {
		if !strings.Contains(got, `"`+m.Hook+`"`) {
			t.Errorf("the %s hook is missing:\n%s", m.Hook, got)
		}
		if !strings.Contains(got, "notify "+m.Event) {
			t.Errorf("the %s event is missing:\n%s", m.Event, got)
		}
	}
}

// matcherFor returns the "matcher" of the first group under a hook that
// holds an entry of ours, and whether a matcher key was present. It is the
// companion to countOurs: it reads the matcher off our own group.
func matcherFor(t *testing.T, path, hook string) (string, bool) {
	t.Helper()
	root, err := decodeObject([]byte(read(t, path)))
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	hooksVal, _ := root.Get("hooks")
	hooks, ok := hooksVal.(*object)
	if !ok {
		t.Fatalf("no hooks object")
	}
	groupsVal, _ := hooks.Get(hook)
	groups, ok := groupsVal.([]any)
	if !ok {
		t.Fatalf("no groups for %s", hook)
	}
	for _, g := range groups {
		group, ok := g.(*object)
		if !ok {
			continue
		}
		entriesVal, _ := group.Get("hooks")
		entries, ok := entriesVal.([]any)
		if !ok {
			continue
		}
		mine := false
		for _, e := range entries {
			if isOurs(e) {
				mine = true
			}
		}
		if !mine {
			continue
		}
		m, present := group.Get("matcher")
		if !present {
			return "", false
		}
		s, _ := m.(string)
		return s, true
	}
	t.Fatalf("no group of ours under %s", hook)
	return "", false
}

func TestInstallWritesTheRightMatcher(t *testing.T) {
	// A matcher narrows a hook to the sub-event it means. Two of these
	// hooks give a false light without one, so the installed file must
	// carry exactly the matcher each mapping asks for and no more.
	path := filepath.Join(t.TempDir(), "settings.json")
	a := newForPath(path)
	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}

	for _, tc := range []struct {
		hook string
		want string // empty means the matcher key must be absent
	}{
		{"SessionStart", "startup"},
		{"Notification", "permission_prompt"},
		{"UserPromptSubmit", ""},
		{"PreToolUse", ""},
		{"Stop", ""},
		{"SessionEnd", ""},
	} {
		got, present := matcherFor(t, path, tc.hook)
		if tc.want == "" {
			if present {
				t.Errorf("%s: matcher %q written, want none", tc.hook, got)
			}
			continue
		}
		if !present || got != tc.want {
			t.Errorf("%s: matcher = %q (present %v), want %q", tc.hook, got, present, tc.want)
		}
	}
}

// TestPreToolUseClearsBlocked guards the fix for the stuck-red bug: a
// PreToolUse hook must report `started`, which is the only event between a
// Notification and a Stop that can move a blocked session off red.
func TestPreToolUseClearsBlocked(t *testing.T) {
	found := false
	for _, m := range mappings {
		if m.Hook == "PreToolUse" {
			found = true
			if m.Event != "started" {
				t.Errorf("PreToolUse event = %q, want started", m.Event)
			}
		}
	}
	if !found {
		t.Error("no PreToolUse mapping: an approved permission prompt would stay red until Stop")
	}
}

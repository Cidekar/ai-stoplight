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
		{Hook: "SessionStart", Event: "idle"},
		{Hook: "UserPromptSubmit", Event: "started"},
		{Hook: "Notification", Event: "blocked"},
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
	want := "notify\nblocked\n--session-id\nabc123\n--cwd\n/Users/me/projects/auth-api\n"
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

func TestInstallOmitsTheMatcherKey(t *testing.T) {
	// UserPromptSubmit and Stop take no matcher, and for the other two an
	// absent matcher already means every occurrence.
	path := filepath.Join(t.TempDir(), "settings.json")
	a := newForPath(path)
	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := read(t, path); strings.Contains(got, "matcher") {
		t.Errorf("a matcher key was written:\n%s", got)
	}
}

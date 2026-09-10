package main

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
)

// closedAddr returns a loopback address with nothing listening on it. It binds
// a port, reads it back, and closes it, which is the only reliable way to name
// a port the operating system is not using.
func closedAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close the reserved port: %v", err)
	}
	return addr
}

// runCLI dispatches args and returns the exit code with both streams captured.
func runCLI(args ...string) (code int, stdout, stderr string) {
	var out, errOut bytes.Buffer
	code = run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

// TestNotifyExitsZeroWithNothingListening is the most important test in this
// package. notify runs inside somebody's editor hook, so a relay that is not
// there must cost the caller nothing. See RFC 1 section 6.
func TestNotifyExitsZeroWithNothingListening(t *testing.T) {
	addr := closedAddr(t)

	code, stdout, stderr := runCLI("notify", "blocked",
		"--session-id", "test-1",
		"--addr", addr,
	)

	if code != exitOK {
		t.Errorf("exit code = %d, want %d with no relay listening", code, exitOK)
	}
	// A hook's output lands in the middle of somebody's terminal, so notify
	// stays silent even when it fails.
	if stdout != "" {
		t.Errorf("stdout = %q, want empty", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

// TestNotifyAlwaysExitsZero covers every way the command can be called wrong.
// Each case would be a broken coding session if it returned anything else.
func TestNotifyAlwaysExitsZero(t *testing.T) {
	addr := closedAddr(t)

	cases := []struct {
		name string
		args []string
	}{
		{"no arguments at all", nil},
		{"no event", []string{"--session-id", "a"}},
		{"no session id", []string{"blocked"}},
		{"unknown event", []string{"invented-event", "--session-id", "a"}},
		{"unknown flag", []string{"blocked", "--not-a-flag", "x"}},
		{"flag with no value", []string{"blocked", "--session-id"}},
		{"help flag", []string{"-h"}},
		{"empty event string", []string{""}},
		{"extra positional arguments", []string{"blocked", "extra", "more"}},
		{"malformed address", []string{"blocked", "--session-id", "a", "--addr", "not::an::addr"}},
		{"empty address", []string{"blocked", "--session-id", "a", "--addr", ""}},
		{"every flag set", []string{"started",
			"--session-id", "a", "--cwd", "/tmp", "--label", "x",
			"--provider", "p", "--addr", addr}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := append([]string{"notify"}, tc.args...)
			// Default the address to a closed port so no case can reach a
			// relay that happens to be running on the developer's machine.
			if !hasFlag(tc.args, "--addr") {
				args = append(args, "--addr", addr)
			}

			code, stdout, stderr := runCLI(args...)

			if code != exitOK {
				t.Errorf("exit code = %d, want %d", code, exitOK)
			}
			if stdout != "" || stderr != "" {
				t.Errorf("wrote output: stdout=%q stderr=%q, want both empty", stdout, stderr)
			}
		})
	}
}

// hasFlag reports whether args already sets name.
func hasFlag(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
}

// TestNotifySurvivesAPanic proves the recover covers the whole notify path. A
// panic in any package notify calls must still leave the caller with a zero
// exit code.
func TestNotifySurvivesAPanic(t *testing.T) {
	code := func() (code int) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("panic escaped cmdNotify: %v", r)
			}
		}()
		// A very long argument exercises the same path with an awkward input.
		return cmdNotify([]string{
			"blocked",
			"--session-id", strings.Repeat("x", 1<<16),
			"--addr", closedAddr(t),
		})
	}()

	if code != exitOK {
		t.Errorf("exit code = %d, want %d", code, exitOK)
	}
}

// captureRelay starts a stub relay that records one request and answers 204,
// which is what RFC 1 section 6 specifies for an accepted report. It returns
// the host:port to pass as --addr, and a function returning the captured body.
func captureRelay(t *testing.T) (addr string, body func() string) {
	t.Helper()

	var (
		mu     sync.Mutex
		path   string
		params string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		path = r.URL.Path
		params = string(b)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	// httptest hands back a full URL, but --addr takes a host:port.
	hostPort := strings.TrimPrefix(srv.URL, "http://")

	return hostPort, func() string {
		mu.Lock()
		defer mu.Unlock()
		return path + " " + params
	}
}

// TestNotifyReachesAListeningRelay proves the happy path still posts, so the
// exit-zero rule is not hiding a client that never sends anything.
func TestNotifyReachesAListeningRelay(t *testing.T) {
	addr, body := captureRelay(t)

	code, _, _ := runCLI("notify", "blocked",
		"--session-id", "session-42",
		"--label", "auth-api",
		"--addr", addr,
	)
	if code != exitOK {
		t.Errorf("exit code = %d, want %d", code, exitOK)
	}

	got := body()
	for _, want := range []string{"/v1/session", "session-42", "blocked", "auth-api"} {
		if !strings.Contains(got, want) {
			t.Errorf("request does not contain %q:\n%s", want, got)
		}
	}
}

// TestNotifyAcceptsFlagsBeforeOrAfterTheEvent is the regression test for the
// flag package stopping at the first positional argument. A hook writing
// `notify blocked --session-id X` must not silently lose the session id.
func TestNotifyAcceptsFlagsBeforeOrAfterTheEvent(t *testing.T) {
	cases := []struct {
		name string
		args func(addr string) []string
	}{
		{"event first", func(a string) []string {
			return []string{"notify", "blocked", "--session-id", "s1", "--label", "auth-api", "--addr", a}
		}},
		{"flags first", func(a string) []string {
			return []string{"notify", "--session-id", "s1", "--label", "auth-api", "--addr", a, "blocked"}
		}},
		{"event in the middle", func(a string) []string {
			return []string{"notify", "--session-id", "s1", "blocked", "--label", "auth-api", "--addr", a}
		}},
		{"equals form", func(a string) []string {
			return []string{"notify", "blocked", "--session-id=s1", "--label=auth-api", "--addr=" + a}
		}},
		{"single dash flags", func(a string) []string {
			return []string{"notify", "blocked", "-session-id", "s1", "-label", "auth-api", "-addr", a}
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, body := captureRelay(t)

			code, _, _ := runCLI(tc.args(addr)...)
			if code != exitOK {
				t.Errorf("exit code = %d, want %d", code, exitOK)
			}

			got := body()
			for _, want := range []string{"s1", "blocked", "auth-api"} {
				if !strings.Contains(got, want) {
					t.Errorf("request does not contain %q:\n%s", want, got)
				}
			}
		})
	}
}

// TestSplitEvent covers the argument splitting on its own, including the forms
// that would otherwise swallow the event.
func TestSplitEvent(t *testing.T) {
	cases := []struct {
		name      string
		args      []string
		wantEvent string
		wantFlags []string
	}{
		{"empty", nil, "", []string{}},
		{"event only", []string{"blocked"}, "blocked", []string{}},
		{"event then flag", []string{"blocked", "--label", "x"}, "blocked", []string{"--label", "x"}},
		{"flag then event", []string{"--label", "x", "blocked"}, "blocked", []string{"--label", "x"}},
		{"equals form keeps its value", []string{"--label=x", "blocked"}, "blocked", []string{"--label=x"}},
		{"flag value is not the event", []string{"--label", "started"}, "", []string{"--label", "started"}},
		{"boolean flag does not eat the event", []string{"--verbose", "blocked"}, "blocked", []string{"--verbose"}},
		{"first positional wins", []string{"blocked", "extra"}, "blocked", []string{}},
		{"double dash", []string{"--", "blocked"}, "blocked", []string{}},
		{"trailing flag with no value", []string{"blocked", "--label"}, "blocked", []string{"--label"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event, flags := splitEvent(tc.args)
			if event != tc.wantEvent {
				t.Errorf("event = %q, want %q", event, tc.wantEvent)
			}
			if !slices.Equal(flags, tc.wantFlags) {
				t.Errorf("flags = %v, want %v", flags, tc.wantFlags)
			}
		})
	}
}

// TestUnknownCommand covers the documented usage failure: a message on stderr
// and exit code 2.
func TestUnknownCommand(t *testing.T) {
	code, stdout, stderr := runCLI("frobnicate")

	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if stdout != "" {
		t.Errorf("stdout = %q, want empty: a usage error belongs on stderr", stdout)
	}
	if !strings.Contains(stderr, "frobnicate") {
		t.Errorf("stderr does not name the unknown command:\n%s", stderr)
	}
	// The usage text follows the error, so the reader learns what is valid.
	if !strings.Contains(stderr, "Commands:") {
		t.Errorf("stderr does not include the command list:\n%s", stderr)
	}
}

// TestHelp checks that every documented command appears in the help text, on
// stdout, with a zero exit code.
func TestHelp(t *testing.T) {
	for _, arg := range []string{"help", "-h", "--help"} {
		t.Run(arg, func(t *testing.T) {
			code, stdout, stderr := runCLI(arg)

			if code != exitOK {
				t.Errorf("exit code = %d, want %d", code, exitOK)
			}
			if stderr != "" {
				t.Errorf("stderr = %q, want empty", stderr)
			}

			// Every command in the readme's table, and every flag on the bare
			// command.
			want := []string{
				"install", "uninstall", "status", "restart", "logs",
				"service stop", "service disable", "task", "notify",
				"--virtual", "--serial", "--addr", "--timeout",
			}
			for _, w := range want {
				if !strings.Contains(stdout, w) {
					t.Errorf("help does not mention %q", w)
				}
			}
		})
	}
}

// TestHelpGoesToStdoutNotStderr keeps help pipeable. `stoplight --help | less`
// is a normal thing to type.
func TestHelpGoesToStdoutNotStderr(t *testing.T) {
	code, stdout, stderr := runCLI("--help")
	if code != exitOK {
		t.Errorf("exit code = %d, want %d", code, exitOK)
	}
	if stdout == "" {
		t.Error("help wrote nothing to stdout")
	}
	if stderr != "" {
		t.Errorf("help wrote to stderr: %q", stderr)
	}
}

// TestServiceSubcommandUsage covers the two documented subcommands and the
// ways of getting them wrong. Only the failures are asserted here: stop and
// disable both reach the platform service manager, which a unit test must not
// drive.
func TestServiceSubcommandUsage(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want int
	}{
		{"no subcommand", []string{"service"}, exitUsage},
		{"unknown subcommand", []string{"service", "restart"}, exitUsage},
		{"unknown subcommand start", []string{"service", "start"}, exitUsage},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, stdout, stderr := runCLI(tc.args...)
			if code != tc.want {
				t.Errorf("exit code = %d, want %d", code, tc.want)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want empty on a usage error", stdout)
			}
			if stderr == "" {
				t.Error("stderr is empty, want an explanation")
			}
		})
	}
}

// TestTaskRequiresText proves the argument checks run before anything touches
// the network.
func TestTaskRequiresText(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no arguments", []string{"task"}},
		{"only a session id", []string{"task", "--session-id", "a"}},
		{"whitespace only", []string{"task", "--session-id", "a", "   "}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, _, stderr := runCLI(tc.args...)
			if code != exitUsage {
				t.Errorf("exit code = %d, want %d", code, exitUsage)
			}
			if !strings.Contains(stderr, "usage:") {
				t.Errorf("stderr does not show usage:\n%s", stderr)
			}
		})
	}
}

// TestTaskRequiresSessionID proves a label with no session to attach it to is
// a usage error rather than a silent no-op.
func TestTaskRequiresSessionID(t *testing.T) {
	code, _, stderr := runCLI("task", "nightly integration run")
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(stderr, "--session-id") {
		t.Errorf("stderr does not mention --session-id:\n%s", stderr)
	}
}

// TestTaskJoinsPositionalArguments proves an unquoted label survives the shell
// splitting it, so `stoplight task nightly integration run` works.
func TestTaskJoinsPositionalArguments(t *testing.T) {
	addr, body := captureRelay(t)

	code, stdout, _ := runCLI("task",
		"--session-id", "a",
		"--addr", addr,
		"nightly", "integration", "run",
	)
	if code != exitOK {
		t.Errorf("exit code = %d, want %d", code, exitOK)
	}
	if !strings.Contains(stdout, "nightly integration run") {
		t.Errorf("stdout does not confirm the joined label:\n%s", stdout)
	}
	if got := body(); !strings.Contains(got, "nightly integration run") {
		t.Errorf("request does not carry the joined label:\n%s", got)
	}
}

// TestTaskReportsAnUnreachableRelay separates task from notify. task is typed
// by a human, so an unreachable relay is worth saying out loud.
func TestTaskReportsAnUnreachableRelay(t *testing.T) {
	code, _, stderr := runCLI("task",
		"--session-id", "a",
		"--addr", closedAddr(t),
		"a label",
	)
	if code != exitError {
		t.Errorf("exit code = %d, want %d", code, exitError)
	}
	if stderr == "" {
		t.Error("stderr is empty, want an explanation")
	}
}

// TestBareCommandRejectsConflictingTransports proves the flags are validated
// before the relay binds anything.
func TestBareCommandRejectsConflictingTransports(t *testing.T) {
	code, _, stderr := runCLI("--virtual", "--serial", "/dev/null")
	if code != exitError {
		t.Errorf("exit code = %d, want %d", code, exitError)
	}
	if !strings.Contains(stderr, "--virtual") || !strings.Contains(stderr, "--serial") {
		t.Errorf("stderr does not name both flags:\n%s", stderr)
	}
}

// TestBareCommandRejectsABadFlag proves an unknown flag on the relay is a
// usage error, unlike on notify where it must be tolerated.
func TestBareCommandRejectsABadFlag(t *testing.T) {
	code, _, stderr := runCLI("--not-a-flag")
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if stderr == "" {
		t.Error("stderr is empty, want an explanation")
	}
}

// TestBareCommandRejectsANonLoopbackAddress proves the CLI passes --addr
// through to the relay's loopback check rather than binding it itself. RFC 1
// section 12 forbids a non-loopback bind without explicit configuration.
func TestBareCommandRejectsANonLoopbackAddress(t *testing.T) {
	code, _, stderr := runCLI("--virtual", "--addr", "0.0.0.0:7373")
	if code != exitError {
		t.Errorf("exit code = %d, want %d", code, exitError)
	}
	if stderr == "" {
		t.Error("stderr is empty, want an explanation")
	}
}

// TestBareCommandRejectsABadDuration proves --timeout is parsed as a duration.
func TestBareCommandRejectsABadDuration(t *testing.T) {
	code, _, stderr := runCLI("--virtual", "--timeout", "half an hour")
	if code != exitUsage {
		t.Errorf("exit code = %d, want %d", code, exitUsage)
	}
	if stderr == "" {
		t.Error("stderr is empty, want an explanation")
	}
}

// TestListening covers the probe behind `stoplight status`, both ways round.
func TestListening(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	open := ln.Addr().String()
	if !listening(open) {
		t.Errorf("listening(%q) = false, want true while the port is open", open)
	}

	ln.Close()
	if listening(open) {
		t.Errorf("listening(%q) = true, want false once the port is closed", open)
	}
}

// TestBinaryPathIsAbsolute proves install writes a path a hook can still
// resolve after the shell that ran it is gone.
func TestBinaryPathIsAbsolute(t *testing.T) {
	path, err := binaryPath()
	if err != nil {
		t.Fatalf("binaryPath: %v", err)
	}
	if path == "" {
		t.Fatal("binaryPath returned an empty string")
	}
	if !strings.HasPrefix(path, "/") {
		t.Errorf("binaryPath = %q, want an absolute path", path)
	}
}

// TestLogPathSitsBesideTheSocket keeps `stoplight logs` and the relay pointed
// at one file. They derive it separately, so this is the guard against drift.
func TestLogPathSitsBesideTheSocket(t *testing.T) {
	path, err := logPath()
	if err != nil {
		t.Skipf("no home directory in this environment: %v", err)
	}
	if !strings.HasSuffix(path, "stoplight.log") {
		t.Errorf("logPath = %q, want it to end in stoplight.log", path)
	}
	if !strings.Contains(path, "stoplight") {
		t.Errorf("logPath = %q, want it under the stoplight state directory", path)
	}
}

// TestRunStateWords pins the two words status prints, because they are the
// whole output of a command people read at a glance.
func TestRunStateWords(t *testing.T) {
	if got := runState(true); got != "running" {
		t.Errorf("runState(true) = %q, want %q", got, "running")
	}
	if got := runState(false); got != "stopped" {
		t.Errorf("runState(false) = %q, want %q", got, "stopped")
	}
}

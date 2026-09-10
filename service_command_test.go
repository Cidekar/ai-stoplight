package main

import (
	"bytes"
	"context"
	"flag"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cidekar/stoplight/internal/adapter"
	"github.com/cidekar/stoplight/internal/service"
)

// This file tests the boundary between the service definitions and command
// dispatch: one component generates a command line, another has to parse it,
// and nothing else in the suite spans the two.
//
// The gap this closes is a real one. Every service test asserted that the
// generated plist or unit held the right text, and every CLI test asserted
// that the commands in cmd.go worked, and between them a definition naming a
// subcommand the binary did not dispatch installed cleanly on all three
// platforms and then failed on every start.

// TestServiceCommandIsDispatchable is the cheap half: the argv the platform's
// service definition starts must name a command run() actually handles.
//
// The argv is read back out of the generated definition rather than restated
// here, so this fails if the plist, the unit or the schtasks arguments start
// naming something dispatch does not know.
func TestServiceCommandIsDispatchable(t *testing.T) {
	const binPath = "/usr/local/bin/stoplight"

	argv, ok := service.GeneratedArgv(binPath)
	if !ok {
		t.Skipf("no service manager on %s, so no generated command to check", runtime.GOOS)
	}
	if len(argv) == 0 {
		t.Fatal("the service definition generates an empty command line")
	}
	if argv[0] != binPath {
		t.Errorf("argv[0] = %q, want the binary path %q", argv[0], binPath)
	}

	// Everything after the binary is what dispatch receives.
	args := argv[1:]
	if len(args) == 0 {
		// The bare command runs the relay, so no arguments is valid.
		return
	}

	// A leading dash is a flag to the bare command, not a subcommand.
	if strings.HasPrefix(args[0], "-") {
		return
	}

	if !isKnownCommand(args[0]) {
		t.Fatalf("the %s service definition runs %q, which run() does not dispatch: "+
			"the installed service fails on every start", runtime.GOOS, strings.Join(argv, " "))
	}
}

// isKnownCommand reports whether run() dispatches name, by asking run()
// rather than by holding a second list that could drift from the switch.
//
// An unknown command is the one case that both exits exitUsage and names the
// command back on stderr, so it is distinguishable without running anything.
// The name is passed with an argument that makes every real command stop at
// its own argument checks, so nothing here touches the network, the service
// manager or the serial port.
func isKnownCommand(name string) bool {
	var out, errOut bytes.Buffer
	code := run([]string{name, "--stoplight-not-a-real-flag"}, &out, &errOut)

	// Any command that parsed its flags rejects the unknown flag as a usage
	// error too, so the exit code alone cannot tell the two apart. The
	// message is what differs: only dispatch says "unknown command".
	return !(code == exitUsage && strings.Contains(errOut.String(), "unknown command"))
}

// TestServiceCommandRuns is the half that would have caught the bug even if
// dispatch had been changed to accept the name and do nothing useful: it
// builds the binary and executes the generated command line.
//
// The command is run with the flags that keep it off real hardware and off a
// fixed port, then cancelled. A relay that starts and serves until cancelled
// is the pass; "unknown command" is the failure this exists for.
func TestServiceCommandRuns(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a binary")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}

	argv, ok := service.GeneratedArgv("placeholder")
	if !ok {
		t.Skipf("no service manager on %s, so no generated command to check", runtime.GOOS)
	}

	bin := buildBinary(t)

	// Replace the placeholder with the binary that was just built, and add
	// the flags that keep the test off the developer's own light and port.
	// Port 0 lets the kernel choose, so two runs of this suite cannot clash.
	args := append([]string{}, argv[1:]...)
	args = append(args, "--virtual", "--addr", "127.0.0.1:0")

	// A generous ceiling. The relay is expected to be cancelled well before
	// this: the deadline only stops a hang from running out the suite clock.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, args...)
	// Interrupt rather than kill, so the relay takes its normal shutdown path
	// and exits zero, which is what distinguishes a clean stop from a crash.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 5 * time.Second

	// exec.Cmd copies the child's output from a goroutine of its own, and this
	// test polls the same buffer while the process runs, so the buffer needs a
	// lock. A plain bytes.Buffer here is a data race that -race will fail on.
	out := &syncBuffer{}
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Start(); err != nil {
		t.Fatalf("start %s %s: %v", bin, strings.Join(args, " "), err)
	}

	// Wait in a goroutine so the poll below can tell "still starting" from
	// "already exited". Reading cmd.ProcessState before Wait returns is itself
	// a race, so the closed channel is what reports the exit.
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()

	// Wait for the relay to say it is listening, which is the proof that the
	// command was dispatched and got as far as serving. Polling the buffer
	// beats a fixed sleep: it passes as soon as the line lands.
	deadline := time.After(10 * time.Second)
	listening := false
	var err error
	exited := false

poll:
	for {
		if strings.Contains(out.String(), "listening on") {
			listening = true
			break
		}
		select {
		case err = <-waitErr:
			// The process exited on its own, so it never got there.
			exited = true
			break poll
		case <-deadline:
			break poll
		case <-time.After(20 * time.Millisecond):
		}
	}

	cancel()
	if !exited {
		err = <-waitErr
	}

	got := out.String()
	if strings.Contains(got, "unknown command") {
		t.Fatalf("the %s service definition runs a command the binary does not know:\n"+
			"  %s %s\noutput:\n%s", runtime.GOOS, bin, strings.Join(args, " "), got)
	}
	if !listening {
		t.Fatalf("the generated command never started the relay:\n  %s %s\noutput:\n%s\nwait: %v",
			bin, strings.Join(args, " "), got, err)
	}
}

// TestNotifyAcceptsTheAdapterFlags is the other half of the same class of
// gap: the Claude Code adapter writes a hook command naming notify flags, and
// nothing checked those names against the flag set notify defines.
//
// notify exits zero on an unknown flag on purpose, because a hook must never
// break its caller. That is the right behaviour and it is also why a wrong
// flag name is invisible: the hook succeeds, the report is dropped, and the
// light silently stops responding. The only place this can be caught is here.
//
// The list is the one mirrored in the adapter's own boundary test. Checking
// it against notifyValueFlags keeps the adapter's copy honest, and checking
// notifyValueFlags against the real flag set keeps this one honest.
func TestNotifyAcceptsTheAdapterFlags(t *testing.T) {
	// Every flag the adapter's generated hook command emits, read out of the
	// command the adapters actually generate rather than listed here.
	//
	// This extraction used to find nothing: no adapter was linked into the
	// binary, because nothing imported internal/adapter/claudecode, so its
	// init never ran and adapter.All() was empty. main.go now imports the
	// adapter packages and adapter_wiring_test.go keeps them imported, so the
	// branch below is the one that runs. The empty case is still tolerated
	// rather than failed, because asserting the registry is populated is that
	// other file's job and duplicating it here would only report the same
	// failure twice.
	if emitted := adapterHookFlags(t); len(emitted) > 0 {
		for _, name := range emitted {
			if !notifyValueFlags[name] {
				t.Errorf("an adapter emits --%s, which splitEvent does not treat as "+
					"a value flag: its value would be parsed as the event name", name)
			}
		}
	} else {
		t.Log("no adapter is registered in this binary, so no generated hook " +
			"command could be checked; the flag set is still verified below")
	}

	// The flags the Claude Code adapter's hook command is known to pass. This
	// is checked unconditionally, so the boundary is asserted even while no
	// adapter is linked in.
	for _, name := range []string{"session-id", "cwd"} {
		if !notifyValueFlags[name] {
			t.Errorf("the adapter emits --%s, which splitEvent does not treat as a "+
				"value flag: its value would be parsed as the event name", name)
		}
	}

	// notifyValueFlags drives splitEvent, so a name in it that notify does not
	// define would swallow the following argument for a flag that then goes
	// nowhere. Ask the real flag set which names exist rather than restating
	// them: a second copy of the list would agree with itself and prove
	// nothing.
	defined := map[string]bool{}
	fs, _ := newNotifyFlagSet()
	fs.VisitAll(func(f *flag.Flag) { defined[f.Name] = true })

	for name := range notifyValueFlags {
		if !defined[name] {
			t.Errorf("notifyValueFlags names %q, which notify does not define", name)
		}
	}
	for name := range defined {
		if !notifyValueFlags[name] {
			t.Errorf("notify defines --%s but splitEvent does not know it takes a "+
				"value, so `notify --%s x blocked` would lose the event", name, name)
		}
	}
}

// syncBuffer is a bytes.Buffer that may be written by the process copier
// goroutine while the test reads it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// adapterHookFlags returns every long flag the adapters' generated hook
// commands pass to notify.
//
// The commands are read back out of the configuration a real Install writes,
// with HOME redirected to a temporary directory, so this reflects what would
// land in the user's settings file rather than a restatement of it. An
// adapter that cannot install here is skipped rather than failed: this test
// is about the flags, not about the installer.
func adapterHookFlags(t *testing.T) []string {
	t.Helper()

	home := t.TempDir()
	t.Setenv("HOME", home)
	// Windows resolves the home directory from USERPROFILE.
	t.Setenv("USERPROFILE", home)

	for _, a := range adapter.All() {
		if err := a.Install("/usr/local/bin/stoplight"); err != nil {
			t.Logf("skipping %s: %v", a.Name(), err)
			continue
		}
	}

	var flags []string
	seen := map[string]bool{}

	// Walk whatever the adapters wrote and pull the flags out of every
	// command that invokes notify.
	err := filepath.WalkDir(home, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, line := range strings.Split(string(data), "\n") {
			_, invocation, ok := strings.Cut(line, " notify ")
			if !ok {
				continue
			}
			for _, field := range strings.Fields(invocation) {
				if !strings.HasPrefix(field, "--") {
					continue
				}
				name := strings.TrimPrefix(field, "--")
				name, _, _ = strings.Cut(name, "=")
				// A JSON string ends with an escaped quote and a comma.
				name = strings.TrimRight(name, `",`)
				if name != "" && !seen[name] {
					seen[name] = true
					flags = append(flags, name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", home, err)
	}
	return flags
}

// buildBinary compiles the command under test into a temporary directory and
// returns the path.
func buildBinary(t *testing.T) string {
	t.Helper()

	name := "stoplight"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(t.TempDir(), name)

	// The package directory is this test's own directory, so the build needs
	// no path juggling.
	cmd := exec.Command("go", "build", "-o", bin, ".")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build the binary: %v\n%s", err, out)
	}
	return bin
}

// TestRelayCommandMatchesTheBareCommand pins the property option (a) rests on:
// the named subcommand and the bare invocation must be the same command, or a
// service that runs one and a developer who runs the other are testing
// different programs.
//
// Both are driven into the same argument error, which is reached before any
// transport is opened, so the comparison costs nothing and touches nothing.
//
// Every case here must fail before selectTransport is called, and that is a
// requirement rather than an incidental property. cmdRelay passes a background
// context, so a case that got as far as auto-discovery would run a real
// Bluetooth scan and trip the data race inside tinygo.org/x/bluetooth described
// in the ble package doc. The cases below are chosen to stop earlier: the first
// fails flag validation, the second fails the listen-address check, and the
// last two fail in flag.Parse.
func TestRelayCommandMatchesTheBareCommand(t *testing.T) {
	cases := [][]string{
		{"--virtual", "--serial", "/dev/ttyUSB0"}, // mutually exclusive
		{"--addr", "not-an-address"},
		{"--timeout", "nonsense"},
		{"--stoplight-not-a-real-flag"},
	}

	for _, args := range cases {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			bareCode, bareOut, bareErr := runCLI(args...)
			namedCode, namedOut, namedErr := runCLI(append([]string{"relay"}, args...)...)

			if bareCode != namedCode {
				t.Errorf("exit code: bare = %d, relay = %d", bareCode, namedCode)
			}
			if bareOut != namedOut {
				t.Errorf("stdout differs:\nbare:  %q\nrelay: %q", bareOut, namedOut)
			}
			if bareErr != namedErr {
				t.Errorf("stderr differs:\nbare:  %q\nrelay: %q", bareErr, namedErr)
			}
		})
	}
}

// TestRelayIsDocumented keeps the subcommand out of the set of things that
// work but are written down nowhere. The service definitions name it, so a
// user reading a plist must be able to find it in the help.
//
// The assertion is that "relay" is listed under Commands, not that the word
// appears somewhere in the output. The usage text says "run the relay in the
// foreground" in prose, so a plain substring check passes whether or not the
// command is documented and proves nothing at all.
func TestRelayIsDocumented(t *testing.T) {
	code, stdout, stderr := runCLI("help")
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d", code, exitOK)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}

	_, commands, ok := strings.Cut(stdout, "Commands:")
	if !ok {
		t.Fatalf("the help has no command list:\n%s", stdout)
	}
	// The list ends at the blank line before the flags section.
	commands, _, _ = strings.Cut(commands, "\nFlags")

	listed := false
	for _, line := range strings.Split(commands, "\n") {
		if name, _, found := strings.Cut(strings.TrimSpace(line), " "); found && name == "relay" {
			listed = true
			break
		}
	}
	if !listed {
		t.Errorf("relay is not listed as a command, so a user reading a plist "+
			"cannot look it up:\n%s", commands)
	}
}

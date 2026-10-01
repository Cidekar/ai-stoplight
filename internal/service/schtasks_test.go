//go:build windows

package service

import (
	"slices"
	"strings"
	"testing"
)

// call is one recorded invocation of the fake command runner.
type call struct {
	name string
	args []string
}

// recorder is a fake command runner. Tests use it so no real schtasks
// ever registers a task on the developer's machine.
type recorder struct {
	calls []call
	out   []byte
	err   error
}

func (r *recorder) run(name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, call{name: name, args: args})
	return r.out, r.err
}

// ran reports whether a call with these leading arguments was made.
func (r *recorder) ran(name string, args ...string) bool {
	for _, c := range r.calls {
		if c.name != name || len(c.args) < len(args) {
			continue
		}
		match := true
		for i, a := range args {
			if c.args[i] != a {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

// hasArg reports whether any recorded call carried this argument.
func (r *recorder) hasArg(want string) bool {
	for _, c := range r.calls {
		if slices.Contains(c.args, want) {
			return true
		}
	}
	return false
}

func newTestSchtasks() (*schtasks, *recorder) {
	rec := &recorder{}
	return &schtasks{run: rec.run}, rec
}

func TestSchtasksCreateArgs(t *testing.T) {
	args := createArgs(`C:\Program Files\stoplight\stoplight.exe`)
	joined := strings.Join(args, " ")

	// ONLOGON is the Windows equivalent of RunAtLoad, /F makes install
	// idempotent, and LIMITED keeps the task unelevated.
	for _, want := range []string{"/Create", "/TN", "/SC", "ONLOGON", "/F", "/RL", "LIMITED"} {
		if !strings.Contains(joined, want) {
			t.Errorf("the arguments are missing %q: %v", want, args)
		}
	}
	// The binary path must be quoted: Program Files contains a space.
	if !strings.Contains(joined, `"C:\Program Files\stoplight\stoplight.exe" relay`) {
		t.Errorf("the path was not quoted: %v", args)
	}
}

func TestSchtasksCreateArgsQuotesRelayArgs(t *testing.T) {
	// A BLE name with a space is the normal case. It must survive as one
	// argument when the relay reads the /TR string back, not split into a
	// name plus a stray positional that the flag parser drops.
	args := createArgs(`C:\stoplight.exe`, "--ble-name", "Stoplight A4")

	var tr string
	for i, a := range args {
		if a == "/TR" && i+1 < len(args) {
			tr = args[i+1]
		}
	}
	if tr == "" {
		t.Fatalf("no /TR value in %v", args)
	}

	got := splitCommandLine(tr)
	want := []string{`C:\stoplight.exe`, "relay", "--ble-name", "Stoplight A4"}
	if !slices.Equal(got, want) {
		t.Errorf("the /TR string did not round-trip: got %#v, want %#v (tr=%q)", got, want, tr)
	}
}

func TestSchtasksInstallRejectsAnUnrepresentableArg(t *testing.T) {
	// A quote inside a value would change where the splitter breaks the next
	// argument, and a newline cannot appear in a task command at all. Both
	// are refused before any task is registered.
	for _, bad := range []string{`a"b`, "a\nb", "a\rb"} {
		s, rec := newTestSchtasks()
		if err := s.Install(`C:\stoplight.exe`, "--ble-name", bad); err == nil {
			t.Errorf("Install accepted the unrepresentable name %q", bad)
		}
		if rec.ran("schtasks", "/Create") {
			t.Errorf("Install ran schtasks for the unrepresentable name %q", bad)
		}
	}
}

func TestSchtasksCreateIsNotElevated(t *testing.T) {
	// /RL HIGHEST would prompt for elevation and gain nothing.
	joined := strings.Join(createArgs(`C:\stoplight.exe`), " ")
	if strings.Contains(joined, "HIGHEST") {
		t.Errorf("the task asks for elevation: %s", joined)
	}
	// /RU SYSTEM would make it a machine-level task.
	if strings.Contains(joined, "SYSTEM") {
		t.Errorf("the task runs as SYSTEM: %s", joined)
	}
}

func TestSchtasksInstall(t *testing.T) {
	s, rec := newTestSchtasks()

	if err := s.Install(`C:\stoplight.exe`); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !rec.ran("schtasks", "/Create") {
		t.Errorf("/Create was not called, calls: %+v", rec.calls)
	}
	// /F is what makes a second install overwrite rather than fail.
	if !rec.hasArg("/F") {
		t.Errorf("/F was not passed, calls: %+v", rec.calls)
	}
}

// TestSchtasksReinstallRestartsARunningRelay covers the reinstall path.
// /Create /F overwrites the task definition but leaves the running process on
// the old command line, and schtasks has no restart, so without this a
// reinstall over a running relay would keep the previous transport until the
// next logon. Install must end the stale process and run the task again.
func TestSchtasksReinstallRestartsARunningRelay(t *testing.T) {
	s, rec := newTestSchtasks()
	// Running() reads the Status column, so the task reports as running.
	rec.out = []byte(`"\Stoplight\Relay","N/A","Running"`)

	if err := s.Install(`C:\stoplight.exe`, "--ble"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if !rec.ran("schtasks", "/Create") {
		t.Errorf("/Create was not called, calls: %+v", rec.calls)
	}
	// The stale process is ended, then the task is run again on the new
	// definition.
	if !rec.ran("schtasks", "/End") {
		t.Errorf("the running task was not ended, calls: %+v", rec.calls)
	}
	if !rec.ran("schtasks", "/Run") {
		t.Errorf("the task was not re-run after the overwrite, calls: %+v", rec.calls)
	}
}

// TestSchtasksInstallLeavesAStoppedRelayStopped is the other half: a reinstall
// must not start a relay the user had stopped. With no running task there is
// nothing to adopt, so Install only overwrites the definition.
func TestSchtasksInstallLeavesAStoppedRelayStopped(t *testing.T) {
	s, rec := newTestSchtasks()
	// Running() reads the Status column, which here reports Ready, not Running.
	rec.out = []byte(`"\Stoplight\Relay","N/A","Ready"`)

	if err := s.Install(`C:\stoplight.exe`); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if rec.ran("schtasks", "/End") {
		t.Errorf("a stopped relay was ended, calls: %+v", rec.calls)
	}
	if rec.ran("schtasks", "/Run") {
		t.Errorf("a stopped relay was started on reinstall, calls: %+v", rec.calls)
	}
}

func TestSchtasksInstallIsIdempotent(t *testing.T) {
	s, _ := newTestSchtasks()

	if err := s.Install(`C:\stoplight.exe`); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	if err := s.Install(`C:\stoplight.exe`); err != nil {
		t.Fatalf("second Install: %v", err)
	}
}

func TestSchtasksUninstall(t *testing.T) {
	s, rec := newTestSchtasks()

	if err := s.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if !rec.ran("schtasks", "/Delete") {
		t.Errorf("/Delete was not called, calls: %+v", rec.calls)
	}
}

func TestSchtasksUninstallToleratesAMissingTask(t *testing.T) {
	// Already absent is the desired state, not an error.
	s, rec := newTestSchtasks()
	rec.out = []byte("ERROR: The system cannot find the file specified.")
	rec.err = errFake

	if err := s.Uninstall(); err != nil {
		t.Errorf("Uninstall treated a missing task as an error: %v", err)
	}
}

func TestSchtasksStartWithoutInstall(t *testing.T) {
	s, rec := newTestSchtasks()
	rec.out = []byte("ERROR: The system cannot find the file specified.")
	rec.err = errFake

	err := s.Start()
	if err == nil {
		t.Fatal("Start succeeded with no task")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("the error does not explain the cause: %v", err)
	}
}

func TestSchtasksStopToleratesANotRunningTask(t *testing.T) {
	s, rec := newTestSchtasks()
	rec.out = []byte("ERROR: The task is not running.")
	rec.err = errFake

	if err := s.Stop(); err != nil {
		t.Errorf("Stop treated a stopped task as an error: %v", err)
	}
}

func TestSchtasksRunning(t *testing.T) {
	tests := []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"running", `"\Stoplight\Relay","N/A","Running"`, nil, true},
		{"ready", `"\Stoplight\Relay","N/A","Ready"`, nil, false},
		{"missing task", "ERROR: cannot find", errFake, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, rec := newTestSchtasks()
			rec.out = []byte(tc.out)
			rec.err = tc.err

			got, err := s.Running()
			if err != nil {
				t.Fatalf("Running: %v", err)
			}
			if got != tc.want {
				t.Errorf("Running = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSchtasksDisable(t *testing.T) {
	s, rec := newTestSchtasks()

	if err := s.Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if !rec.ran("schtasks", "/Change") {
		t.Errorf("/Change was not called, calls: %+v", rec.calls)
	}
	if !rec.hasArg("/DISABLE") {
		t.Errorf("/DISABLE was not passed, calls: %+v", rec.calls)
	}
}

func TestSchtasksNeverElevates(t *testing.T) {
	s, rec := newTestSchtasks()

	_ = s.Install(`C:\stoplight.exe`)
	_ = s.Start()
	_ = s.Stop()
	_ = s.Disable()
	_, _ = s.Running()
	_ = s.Uninstall()

	for _, c := range rec.calls {
		if c.name != "schtasks" {
			t.Errorf("an unexpected command was run: %+v", c)
		}
		for _, a := range c.args {
			if a == "HIGHEST" || a == "SYSTEM" {
				t.Errorf("an elevated invocation was made: %+v", c)
			}
		}
	}
}

func TestSchtasksCommandIsTransparent(t *testing.T) {
	s, _ := newTestSchtasks()
	got := s.Command()
	if !strings.Contains(got, "schtasks") {
		t.Errorf("Command = %q, want it to name schtasks", got)
	}
	if !strings.Contains(got, taskName) {
		t.Errorf("Command = %q, want it to name the task", got)
	}
}

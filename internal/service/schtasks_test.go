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

//go:build windows

package service

import (
	"fmt"
	"os/exec"
	"strings"
)

// taskName is the Task Scheduler name for the relay. The backslash makes
// it a folder, which keeps it out of the crowded root task list.
const taskName = `\Stoplight\Relay`

// schtasks manages the relay as a Task Scheduler job that runs at logon.
//
// The task is registered for the current user only, so it needs no
// elevation and runs in the interactive session where Bluetooth lives.
type schtasks struct {
	// run executes a command. Tests replace it so no real schtasks runs.
	run func(name string, args ...string) ([]byte, error)
}

// newManager returns the Task Scheduler manager for the current user.
func newManager() (Manager, error) {
	return &schtasks{run: runCommand}, nil
}

// runCommand executes a command and returns its combined output.
func runCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// Command implements Manager.
func (s *schtasks) Command() string {
	return `schtasks /Run /TN ` + taskName
}

// createArgs builds the registration command.
//
// /SC ONLOGON starts the relay when the user logs in, which is the
// Windows equivalent of RunAtLoad. /F overwrites an existing task, which
// is what makes Install idempotent. /RL LIMITED keeps the task at the
// user's normal privileges: the relay needs none, and asking for more
// would prompt for elevation and gain nothing.
//
// Task Scheduler has no supervisor equivalent to KeepAlive, so restart on
// failure is configured separately in Install.
//
// Every token in /TR is quoted, not just the binary path. The relay reads
// its flags back off this one string, so a value with a space in it, such
// as `--ble-name "Stoplight A4"`, must stay one argument. Joining the
// tokens raw would hand the relay `--ble-name Stoplight` and leave `A4` as
// a stray positional that the flag parser drops, so the service would
// connect to the wrong light. Install rejects a value a command line
// cannot carry, so quoteArg here only has to wrap, never escape.
func createArgs(binPath string, relayArgs ...string) []string {
	tr := quoteArg(binPath) + " relay"
	for _, a := range relayArgs {
		tr += " " + quoteArg(a)
	}
	return []string{
		"/Create",
		"/TN", taskName,
		"/TR", tr,
		"/SC", "ONLOGON",
		"/RL", "LIMITED",
		"/F",
	}
}

// quoteArg renders one argument for the /TR command line.
//
// The task's command line is split by the Windows C runtime rules that
// CommandLineToArgvW implements: a run of whitespace separates arguments
// unless it sits inside double quotes. Wrapping every token in quotes
// keeps a value with a space in it whole and leaves a value without one
// unchanged after the split. A token is never left bare, because the
// splitter that reads it back cannot tell an intended space from a
// separator otherwise.
//
// A double quote is not escaped here but refused in validateArg, so this
// only has to wrap. An argument that already holds no quote survives the
// round trip exactly.
func quoteArg(arg string) string {
	return `"` + arg + `"`
}

// validateArg rejects a relay argument a schtasks /TR string cannot carry.
//
// A double quote would open or close a quoted run inside the value and so
// change where the splitter breaks the next argument, exactly the kind of
// corruption quoting is meant to prevent. A newline cannot appear in a
// task command line at all. Refusing both, rather than attempting an
// escape, is the same answer validateUnitPath gives on the systemd side:
// a value the format cannot represent is a value the service must not be
// built from.
func validateArg(arg string) error {
	if strings.ContainsAny(arg, "\"\n\r") {
		return fmt.Errorf("the relay argument %q contains a quote or line break, which a scheduled task command cannot carry, so the service cannot be installed", arg)
	}
	return nil
}

// Install registers the logon task, replacing any previous version.
func (s *schtasks) Install(binPath string, relayArgs ...string) error {
	// Both the binary path and every relay argument are written into the
	// one /TR string, so a value the command line cannot represent is
	// refused before anything is registered.
	if err := validateArg(binPath); err != nil {
		return err
	}
	for _, a := range relayArgs {
		if err := validateArg(a); err != nil {
			return err
		}
	}
	if out, err := s.run("schtasks", createArgs(binPath, relayArgs...)...); err != nil {
		return fmt.Errorf("schtasks /Create %s: %w: %s", taskName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Uninstall removes the task. A task that is already gone is the desired
// state rather than an error.
func (s *schtasks) Uninstall() error {
	out, err := s.run("schtasks", "/Delete", "/TN", taskName, "/F")
	if err != nil && !isNoSuchTask(out) {
		return fmt.Errorf("schtasks /Delete %s: %w: %s", taskName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// isNoSuchTask recognises the message meaning the task does not exist.
func isNoSuchTask(out []byte) bool {
	s := strings.ToLower(string(out))
	return strings.Contains(s, "cannot find the file specified") ||
		strings.Contains(s, "does not exist") ||
		strings.Contains(s, "the system cannot find")
}

// Start launches the relay now.
func (s *schtasks) Start() error {
	out, err := s.run("schtasks", "/Run", "/TN", taskName)
	if err != nil {
		if isNoSuchTask(out) {
			return fmt.Errorf("the service is not installed: run `stoplight install`")
		}
		return fmt.Errorf("schtasks /Run %s: %w: %s", taskName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Stop halts the relay until the next logon.
func (s *schtasks) Stop() error {
	out, err := s.run("schtasks", "/End", "/TN", taskName)
	// /End fails when the task is not running, which is the state we want.
	if err != nil && !isNoSuchTask(out) && !isNotRunning(out) {
		return fmt.Errorf("schtasks /End %s: %w: %s", taskName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// isNotRunning recognises the message meaning there was nothing to stop.
func isNotRunning(out []byte) bool {
	return strings.Contains(strings.ToLower(string(out)), "not running")
}

// Disable stops the relay and prevents it starting at logon. The task
// definition stays, so Start can bring it back without a reinstall.
func (s *schtasks) Disable() error {
	if err := s.Stop(); err != nil {
		return err
	}
	out, err := s.run("schtasks", "/Change", "/TN", taskName, "/DISABLE")
	if err != nil {
		return fmt.Errorf("schtasks /Change %s /DISABLE: %w: %s", taskName, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Running reports whether the task is currently executing.
//
// The verbose CSV listing carries a Status column, which reads "Running"
// only while the process is up. A missing task is not an error here: it
// is simply not running.
func (s *schtasks) Running() (bool, error) {
	out, err := s.run("schtasks", "/Query", "/TN", taskName, "/FO", "CSV", "/NH", "/V")
	if err != nil {
		return false, nil
	}
	return strings.Contains(string(out), `"Running"`), nil
}

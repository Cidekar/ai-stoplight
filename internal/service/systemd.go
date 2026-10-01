//go:build linux

package service

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// systemdUnit is the unit name systemctl knows the relay by.
const systemdUnit = "stoplight.service"

// systemd manages the relay as a systemd user unit.
//
// A user unit runs in the login session rather than the system manager,
// so it needs no privileges and starts when the user logs in.
type systemd struct {
	// unitPath is the unit file. Tests redirect it.
	unitPath string
	// logFile is where stdout and stderr are appended.
	logFile string
	// run executes a command. Tests replace it so no real systemctl runs.
	run func(name string, args ...string) ([]byte, error)
}

// newManager returns the systemd manager for the current user.
func newManager() (Manager, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("find the home directory: %w", err)
	}
	log, err := logPath()
	if err != nil {
		return nil, err
	}
	return &systemd{
		unitPath: filepath.Join(home, ".config", "systemd", "user", systemdUnit),
		logFile:  log,
		run:      runCommand,
	}, nil
}

// runCommand executes a command and returns its combined output.
func runCommand(name string, args ...string) ([]byte, error) {
	return exec.Command(name, args...).CombinedOutput()
}

// Command implements Manager.
func (s *systemd) Command() string {
	return "systemctl --user start " + systemdUnit
}

// unit renders the unit file.
//
// Restart=always with RestartSec=5 is the systemd equivalent of launchd's
// KeepAlive: the relay stays up through a crash, and the delay stops a
// persistent failure becoming a busy loop.
//
// WantedBy=default.target is what makes `systemctl --user enable` start
// the relay at login.
func (s *systemd) unit(binPath string, relayArgs ...string) []byte {
	var b bytes.Buffer
	// A comment line, so that Install and Uninstall can tell this file
	// from a unit at the same path that somebody else wrote.
	b.WriteString("# " + serviceMarker + "\n")
	b.WriteString("[Unit]\n")
	b.WriteString("Description=Stoplight relay\n")
	b.WriteString("Documentation=https://github.com/cidekar/stoplight\n")
	// The relay retries the light on its own, so it does not need the
	// network. Ordering after the graphical session gives it the DBus
	// session bus that BlueZ is reached through.
	b.WriteString("After=graphical-session.target\n")
	b.WriteString("\n")

	b.WriteString("[Service]\n")
	b.WriteString("Type=simple\n")
	b.WriteString("ExecStart=" + escapeUnitExec(binPath) + " relay")
	for _, arg := range relayArgs {
		b.WriteString(" " + escapeUnitPath(arg))
	}
	b.WriteString("\n")
	b.WriteString("Restart=always\n")
	b.WriteString("RestartSec=5\n")
	b.WriteString("StandardOutput=append:" + escapeUnitPath(s.logFile) + "\n")
	b.WriteString("StandardError=append:" + escapeUnitPath(s.logFile) + "\n")
	b.WriteString("\n")

	b.WriteString("[Install]\n")
	b.WriteString("WantedBy=default.target\n")
	return b.Bytes()
}

// escapeUnitExec quotes a path for ExecStart when it contains a space.
// systemd splits the command on whitespace, so an unquoted path under a
// directory with a space would be read as two arguments.
//
// A percent is doubled whether or not the path is quoted. systemd expands
// specifiers such as %h and %n everywhere in a value, quotes included, so
// "/opt/100%cool/stoplight" would otherwise run some other path entirely.
func escapeUnitExec(path string) string {
	return escapeUnitPath(path)
}

// escapeUnitPath renders a path as a unit file value: every percent
// doubled, and the whole thing quoted when it holds whitespace or a
// quote.
//
// The percent matters everywhere, not only in ExecStart. StandardOutput
// and StandardError take a path after "append:", and systemd expands
// specifiers there too, so the same escaping applies.
func escapeUnitPath(path string) string {
	// Decide on quoting from the original path, before any percent is
	// doubled. Doubling only ever adds a percent, which is not one of the
	// characters tested here, so the two orders agree today. Reading the
	// original keeps it that way: a future escape that introduced a space,
	// a quote or a backslash would otherwise flip this test by accident.
	quote := strings.ContainsAny(path, " \t\"\\")

	// A literal percent is written as two in a unit file.
	path = strings.ReplaceAll(path, "%", "%%")
	if !quote {
		return path
	}
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range path {
		if r == '"' || r == '\\' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	b.WriteByte('"')
	return b.String()
}

// validateUnitPath rejects a path that cannot be written into a unit file
// safely.
//
// A unit file is line oriented: a directive ends at the newline. systemd
// offers no escape for a newline inside a value, and no quoting hides one,
// so a path holding one would end the directive early and leave the rest
// of the path as a line of its own. "/opt/new\nline/stoplight" turns
// ExecStart into "/opt/new" followed by a bare "line/stoplight relay"
// line, which is arbitrary unit content chosen by whoever set the path.
// Refusing is the only correct answer. A carriage return is rejected with
// it, because systemd trims it and it hides the same edit in a diff.
func validateUnitPath(kind, path string) error {
	if strings.ContainsAny(path, "\n\r") {
		return fmt.Errorf("the %s %q contains a line break: a systemd unit file cannot represent one, so the service cannot be installed", kind, path)
	}
	return nil
}

// Install writes the unit, reloads the manager and enables it at login.
func (s *systemd) Install(binPath string, relayArgs ...string) error {
	// Refuse a path a unit file cannot hold, before anything is changed.
	// Both values reach a directive, so both are checked.
	if err := validateUnitPath("binary path", binPath); err != nil {
		return err
	}
	if err := validateUnitPath("log path", s.logFile); err != nil {
		return err
	}
	// A relay argument reaches the ExecStart directive too, so a line break
	// in one would split the directive exactly as a bad path would.
	for _, arg := range relayArgs {
		if err := validateUnitPath("relay argument", arg); err != nil {
			return err
		}
	}
	// Refuse a unit we did not write, before anything is changed.
	if err := checkOwned(s.unitPath); err != nil {
		return err
	}
	dir := filepath.Dir(s.logFile)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}
	if err := writeFileAtomic(s.unitPath, s.unit(binPath, relayArgs...), 0o644); err != nil {
		return err
	}
	// systemd caches unit files, so a rewrite is invisible until this.
	if out, err := s.run("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// enable is idempotent and rewrites the symlink, which also clears a
	// previous `disable`.
	if out, err := s.run("systemctl", "--user", "enable", systemdUnit); err != nil {
		return fmt.Errorf("systemctl --user enable %s: %w: %s", systemdUnit, err, strings.TrimSpace(string(out)))
	}
	// Adopt the rewritten unit on a reinstall. systemd keeps the old process
	// running until the unit is restarted, so a plain `start` later is a
	// no-op on an active unit and the relay would keep the previous transport
	// until the next login. try-restart restarts the unit only when it is
	// already active, and is a no-op otherwise, so a reinstall over a running
	// relay picks up the new ExecStart while a stopped relay stays stopped.
	// This mirrors launchd, where Install boots the agent out and back in.
	if out, err := s.run("systemctl", "--user", "try-restart", systemdUnit); err != nil {
		return fmt.Errorf("systemctl --user try-restart %s: %w: %s", systemdUnit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Uninstall stops the relay, disables it and removes the unit.
//
// A unit without our marker is left alone and reported, because deleting
// a hand-written or distro-packaged unit is not something the user can
// undo. The check comes first, so a refusal also leaves the running
// service alone rather than stopping something we then decline to remove.
func (s *systemd) Uninstall() error {
	if err := checkOwned(s.unitPath); err != nil {
		return err
	}
	// Both may fail because the unit is already gone, which is the state
	// we want, so neither failure is fatal.
	_, _ = s.run("systemctl", "--user", "stop", systemdUnit)
	_, _ = s.run("systemctl", "--user", "disable", systemdUnit)

	if err := os.Remove(s.unitPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove %s: %w", s.unitPath, err)
	}
	if out, err := s.run("systemctl", "--user", "daemon-reload"); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Start launches the relay now.
func (s *systemd) Start() error {
	if _, err := os.Stat(s.unitPath); err != nil {
		return fmt.Errorf("the service is not installed: run `stoplight install`")
	}
	if out, err := s.run("systemctl", "--user", "start", systemdUnit); err != nil {
		return fmt.Errorf("systemctl --user start %s: %w: %s", systemdUnit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Stop halts the relay until the next login.
func (s *systemd) Stop() error {
	if out, err := s.run("systemctl", "--user", "stop", systemdUnit); err != nil {
		return fmt.Errorf("systemctl --user stop %s: %w: %s", systemdUnit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Disable stops the relay and prevents it starting at login. The unit
// file stays, so Start can bring it back without a reinstall.
func (s *systemd) Disable() error {
	if out, err := s.run("systemctl", "--user", "disable", "--now", systemdUnit); err != nil {
		return fmt.Errorf("systemctl --user disable --now %s: %w: %s", systemdUnit, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// Running reports whether systemd has the unit active.
//
// `systemctl is-active` exits non-zero for every inactive state, which is
// an answer rather than a failure, so the output is what is trusted.
func (s *systemd) Running() (bool, error) {
	out, _ := s.run("systemctl", "--user", "is-active", systemdUnit)
	return strings.TrimSpace(string(out)) == "active", nil
}

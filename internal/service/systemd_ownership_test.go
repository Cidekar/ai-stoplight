//go:build linux

package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// foreignUnit is a unit file that did not come from this package.
const foreignUnit = `[Unit]
Description=Somebody else's relay

[Service]
ExecStart=/opt/somebody-else/relay
`

// TestSystemdUnitCarriesTheMarker is the precondition for the ownership
// checks.
func TestSystemdUnitCarriesTheMarker(t *testing.T) {
	s, _ := newTestSystemd(t)
	if got := string(s.unit("/usr/local/bin/stoplight")); !strings.Contains(got, serviceMarker) {
		t.Errorf("the generated unit has no marker:\n%s", got)
	}
}

// TestSystemdInstallRefusesAForeignUnit covers a unit at our path that
// another tool wrote.
func TestSystemdInstallRefusesAForeignUnit(t *testing.T) {
	s, rec := newTestSystemd(t)
	if err := os.WriteFile(s.unitPath, []byte(foreignUnit), 0o644); err != nil {
		t.Fatal(err)
	}

	err := s.Install("/usr/local/bin/stoplight")
	if err == nil {
		t.Fatalf("Install overwrote a unit it did not write")
	}
	if !strings.Contains(err.Error(), "not written by Stoplight") {
		t.Errorf("the error does not explain the problem: %v", err)
	}

	got, rerr := os.ReadFile(s.unitPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != foreignUnit {
		t.Errorf("the foreign unit was modified:\n%s", got)
	}
	if rec.ran("systemctl", "--user", "enable") {
		t.Errorf("systemctl ran despite the refusal")
	}
}

// TestSystemdUninstallRefusesAForeignUnit covers the deletion case.
func TestSystemdUninstallRefusesAForeignUnit(t *testing.T) {
	s, _ := newTestSystemd(t)
	if err := os.WriteFile(s.unitPath, []byte(foreignUnit), 0o644); err != nil {
		t.Fatal(err)
	}

	err := s.Uninstall()
	if err == nil {
		t.Fatalf("Uninstall deleted a unit it did not write")
	}
	if !strings.Contains(err.Error(), "not written by Stoplight") {
		t.Errorf("the error does not explain the problem: %v", err)
	}
	if _, serr := os.Stat(s.unitPath); serr != nil {
		t.Errorf("the foreign unit was deleted: %v", serr)
	}
}

// TestSystemdInstallRejectsALineBreakInTheBinaryPath covers directive
// injection. A unit file ends a directive at the newline and systemd has
// no escape for one, so "/opt/new\nline/stoplight" would write
// "ExecStart=/opt/new" and leave "line/stoplight relay" as a unit
// directive of its own. Install must refuse before writing anything.
func TestSystemdInstallRejectsALineBreakInTheBinaryPath(t *testing.T) {
	for _, bad := range []string{"/opt/new\nline/stoplight", "/opt/new\rline/stoplight"} {
		s, rec := newTestSystemd(t)

		err := s.Install(bad)
		if err == nil {
			t.Fatalf("Install accepted %q", bad)
		}
		if !strings.Contains(err.Error(), "line break") {
			t.Errorf("the error does not explain the problem: %v", err)
		}
		if _, serr := os.Stat(s.unitPath); !os.IsNotExist(serr) {
			t.Errorf("a unit was written for %q", bad)
		}
		if rec.ran("systemctl", "--user", "enable") {
			t.Errorf("systemctl ran despite the refusal")
		}
	}
}

// TestSystemdInstallRejectsALineBreakInTheLogPath covers the same
// injection through StandardOutput and StandardError, which a home
// directory holding a newline would reach.
func TestSystemdInstallRejectsALineBreakInTheLogPath(t *testing.T) {
	dir := t.TempDir()
	rec := &recorder{}
	s := &systemd{
		unitPath: filepath.Join(dir, "stoplight.service"),
		logFile:  "/home/jo\nRestartSec=0/stoplight.log",
		run:      rec.run,
	}

	err := s.Install("/usr/local/bin/stoplight")
	if err == nil {
		t.Fatalf("Install accepted a log path with a newline")
	}
	if !strings.Contains(err.Error(), "line break") {
		t.Errorf("the error does not explain the problem: %v", err)
	}
	if _, serr := os.Stat(s.unitPath); !os.IsNotExist(serr) {
		t.Errorf("a unit was written despite the refusal")
	}
}

// TestSystemdReinstallKeepsTightenedPermissions covers a chmodded unit.
func TestSystemdReinstallKeepsTightenedPermissions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits")
	}
	s, _ := newTestSystemd(t)
	if err := s.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	if err := os.Chmod(s.unitPath, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := s.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatalf("second Install: %v", err)
	}

	info, err := os.Stat(s.unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %04o, want 0600: the user's chmod was reset", perm)
	}
}

// TestSystemdInstallAndUninstallRoundTrip confirms the checks do not block
// the normal path.
func TestSystemdInstallAndUninstallRoundTrip(t *testing.T) {
	s, _ := newTestSystemd(t)
	if err := s.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := s.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatalf("reinstall over our own unit: %v", err)
	}
	if err := s.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(s.unitPath); !os.IsNotExist(err) {
		t.Errorf("our own unit survived uninstall")
	}
}

// TestSystemdEscapesPercent covers a specifier in the binary path.
// systemd expands "%c", "%h" and friends inside ExecStart, so a literal
// percent has to be doubled or the unit runs the wrong command.
func TestSystemdEscapesPercent(t *testing.T) {
	s, _ := newTestSystemd(t)
	got := string(s.unit("/opt/100%cool/stoplight"))

	if !strings.Contains(got, "%%cool") {
		t.Errorf("the percent was not doubled:\n%s", got)
	}
	// No single percent may survive: every one must be part of a "%%".
	if strings.Contains(strings.ReplaceAll(got, "%%", ""), "%") {
		t.Errorf("an unescaped specifier survived:\n%s", got)
	}
}

// TestEscapeUnitExecPercent pins the escaping down at the unit level.
func TestEscapeUnitExecPercent(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"/opt/100%cool/stoplight", "/opt/100%%cool/stoplight"},
		{"/home/50% off/stoplight", `"/home/50%% off/stoplight"`},
		{"/plain/stoplight", "/plain/stoplight"},
	}
	for _, tc := range tests {
		if got := escapeUnitExec(tc.in); got != tc.want {
			t.Errorf("escapeUnitExec(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSystemdEscapesTheLogPath covers a home directory with a space or a
// percent in it, which is reachable by any user whose username has one.
// StandardOutput=append: interpolated the path with no escaping at all.
func TestSystemdEscapesTheLogPath(t *testing.T) {
	dir := t.TempDir()
	rec := &recorder{}
	s := &systemd{
		unitPath: filepath.Join(dir, "stoplight.service"),
		logFile:  "/home/jo blogs/100%state/stoplight.log",
		run:      rec.run,
	}
	got := string(s.unit("/usr/local/bin/stoplight"))

	for _, line := range strings.Split(got, "\n") {
		if !strings.HasPrefix(line, "StandardOutput=") && !strings.HasPrefix(line, "StandardError=") {
			continue
		}
		// The percent must be doubled wherever it appears.
		if strings.Contains(strings.ReplaceAll(line, "%%", ""), "%") {
			t.Errorf("an unescaped specifier in %q", line)
		}
		// The space must not split the value: systemd needs it quoted.
		value := strings.SplitN(line, ":", 2)[1]
		if strings.Contains(value, " ") && !strings.HasPrefix(value, `"`) {
			t.Errorf("the path with a space was not quoted in %q", line)
		}
	}
}

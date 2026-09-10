//go:build linux

package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// call is one recorded invocation of the fake command runner.
type call struct {
	name string
	args []string
}

// recorder is a fake command runner. Tests use it so no real systemctl
// ever runs against the developer's own session.
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

// newTestSystemd returns a manager pointed at a temporary directory with a
// fake command runner.
func newTestSystemd(t *testing.T) (*systemd, *recorder) {
	t.Helper()
	dir := t.TempDir()
	rec := &recorder{}
	s := &systemd{
		unitPath: filepath.Join(dir, "stoplight.service"),
		logFile:  filepath.Join(dir, "state", "stoplight.log"),
		run:      rec.run,
	}
	return s, rec
}

func TestSystemdUnitContent(t *testing.T) {
	s, _ := newTestSystemd(t)
	got := string(s.unit("/usr/local/bin/stoplight"))

	// Restart=always with RestartSec=5 is the systemd equivalent of
	// launchd's KeepAlive, and WantedBy=default.target is what makes
	// `enable` start the relay at login.
	for _, want := range []string{
		"[Unit]",
		"[Service]",
		"[Install]",
		"ExecStart=/usr/local/bin/stoplight relay",
		"Restart=always",
		"RestartSec=5",
		"WantedBy=default.target",
		"Type=simple",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the unit is missing %q:\n%s", want, got)
		}
	}

	if !strings.Contains(got, s.logFile) {
		t.Errorf("the log path is missing:\n%s", got)
	}
}

func TestSystemdUnitIsUserLevel(t *testing.T) {
	// A user unit must not carry system-level directives.
	s, _ := newTestSystemd(t)
	got := string(s.unit("/usr/local/bin/stoplight"))

	for _, forbidden := range []string{"User=", "Group=", "WantedBy=multi-user.target"} {
		if strings.Contains(got, forbidden) {
			t.Errorf("the unit contains the system-level directive %q:\n%s", forbidden, got)
		}
	}
}

func TestSystemdUnitQuotesAPathWithSpaces(t *testing.T) {
	// systemd splits ExecStart on whitespace, so an unquoted path under a
	// directory with a space would be read as two arguments.
	s, _ := newTestSystemd(t)
	got := string(s.unit("/opt/my tools/stoplight"))

	if !strings.Contains(got, `ExecStart="/opt/my tools/stoplight" relay`) {
		t.Errorf("the path was not quoted:\n%s", got)
	}
}

func TestEscapeUnitExec(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"/usr/bin/stoplight", "/usr/bin/stoplight"},
		{"/a b/stoplight", `"/a b/stoplight"`},
		{`/a"b/stoplight`, `"/a\"b/stoplight"`},
	}
	for _, tc := range tests {
		if got := escapeUnitExec(tc.in); got != tc.want {
			t.Errorf("escapeUnitExec(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestSystemdInstallWritesTheUnit(t *testing.T) {
	s, rec := newTestSystemd(t)

	if err := s.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(s.unitPath); err != nil {
		t.Fatalf("the unit was not written: %v", err)
	}
	// A rewrite is invisible to systemd until daemon-reload.
	if !rec.ran("systemctl", "--user", "daemon-reload") {
		t.Errorf("daemon-reload was not called, calls: %+v", rec.calls)
	}
	if !rec.ran("systemctl", "--user", "enable") {
		t.Errorf("enable was not called, calls: %+v", rec.calls)
	}
}

func TestSystemdInstallIsIdempotent(t *testing.T) {
	s, _ := newTestSystemd(t)

	if err := s.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	first, err := os.ReadFile(s.unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	second, err := os.ReadFile(s.unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("the unit changed on the second install")
	}
}

func TestSystemdUninstallRemovesTheUnit(t *testing.T) {
	s, rec := newTestSystemd(t)

	if err := s.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatal(err)
	}
	if err := s.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(s.unitPath); !os.IsNotExist(err) {
		t.Error("the unit survived uninstall")
	}
	if !rec.ran("systemctl", "--user", "stop") {
		t.Errorf("stop was not called, calls: %+v", rec.calls)
	}
	if !rec.ran("systemctl", "--user", "disable") {
		t.Errorf("disable was not called, calls: %+v", rec.calls)
	}
}

func TestSystemdUninstallWhenNotInstalled(t *testing.T) {
	s, _ := newTestSystemd(t)
	if err := s.Uninstall(); err != nil {
		t.Errorf("Uninstall with no unit: %v", err)
	}
}

func TestSystemdStartWithoutInstallFails(t *testing.T) {
	s, _ := newTestSystemd(t)
	err := s.Start()
	if err == nil {
		t.Fatal("Start succeeded with no unit")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("the error does not explain the cause: %v", err)
	}
}

func TestSystemdRunning(t *testing.T) {
	tests := []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"active", "active\n", nil, true},
		// is-active exits non-zero for every inactive state, which is an
		// answer rather than a failure.
		{"inactive", "inactive\n", errFake, false},
		{"failed", "failed\n", errFake, false},
		{"unknown unit", "inactive\n", errFake, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s, rec := newTestSystemd(t)
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

func TestSystemdDisableKeepsTheUnitFile(t *testing.T) {
	s, rec := newTestSystemd(t)
	if err := s.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatal(err)
	}
	rec.calls = nil

	if err := s.Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if !rec.ran("systemctl", "--user", "disable", "--now") {
		t.Errorf("disable --now was not called, calls: %+v", rec.calls)
	}
	// The unit stays so Start can bring it back without a reinstall.
	if _, err := os.Stat(s.unitPath); err != nil {
		t.Errorf("Disable removed the unit: %v", err)
	}
}

func TestSystemdAlwaysPassesUserFlag(t *testing.T) {
	// Every invocation must be user-level. A missing --user would target
	// the system manager and prompt for a password.
	s, rec := newTestSystemd(t)

	if err := s.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatal(err)
	}
	_ = s.Start()
	_ = s.Stop()
	_ = s.Disable()
	_, _ = s.Running()
	_ = s.Uninstall()

	if len(rec.calls) == 0 {
		t.Fatal("no commands were recorded")
	}
	for _, c := range rec.calls {
		if c.name == "sudo" {
			t.Errorf("a command was run with sudo: %+v", c)
		}
		if c.name == "systemctl" && (len(c.args) == 0 || c.args[0] != "--user") {
			t.Errorf("systemctl was called without --user: %+v", c)
		}
	}
}

func TestSystemdCommandIsTransparent(t *testing.T) {
	s, _ := newTestSystemd(t)
	got := s.Command()
	if !strings.Contains(got, "systemctl --user") {
		t.Errorf("Command = %q, want it to name systemctl --user", got)
	}
	if !strings.Contains(got, systemdUnit) {
		t.Errorf("Command = %q, want it to name the unit", got)
	}
}

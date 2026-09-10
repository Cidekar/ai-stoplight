//go:build darwin

package service

import (
	"encoding/xml"
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

// recorder is a fake command runner. Tests use it so no real launchctl
// ever runs: this suite must not touch the developer's own login agents.
type recorder struct {
	calls []call
	// out is returned for every call.
	out []byte
	// err is returned for every call.
	err error
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

// newTestLaunchd returns a manager pointed at a temporary directory with a
// fake command runner.
func newTestLaunchd(t *testing.T) (*launchd, *recorder) {
	t.Helper()
	dir := t.TempDir()
	rec := &recorder{}
	l := &launchd{
		plistPath: filepath.Join(dir, "com.stoplight.relay.plist"),
		logFile:   filepath.Join(dir, "state", "stoplight.log"),
		uid:       501,
		run:       rec.run,
	}
	return l, rec
}

func TestLaunchdPlistIsValidXML(t *testing.T) {
	l, _ := newTestLaunchd(t)
	var v any
	if err := xml.Unmarshal(l.plist("/usr/local/bin/stoplight"), &v); err != nil {
		t.Fatalf("the plist is not valid XML: %v", err)
	}
}

func TestLaunchdPlistContent(t *testing.T) {
	l, _ := newTestLaunchd(t)
	got := string(l.plist("/usr/local/bin/stoplight"))

	// RunAtLoad starts the relay at login and KeepAlive restarts it after
	// a crash. Together they are the reason this package exists.
	for _, want := range []string{
		"<key>Label</key>",
		"<string>com.stoplight.relay</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
		"<string>/usr/local/bin/stoplight</string>",
		"<string>relay</string>",
		"<key>StandardOutPath</key>",
		"<key>StandardErrorPath</key>",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the plist is missing %q:\n%s", want, got)
		}
	}

	// Both flags must be true, not merely present.
	for _, key := range []string{"RunAtLoad", "KeepAlive"} {
		idx := strings.Index(got, "<key>"+key+"</key>")
		if idx < 0 {
			t.Fatalf("%s is missing", key)
		}
		rest := got[idx:]
		if !strings.Contains(rest[:60], "<true/>") {
			t.Errorf("%s is not set to true:\n%s", key, rest[:60])
		}
	}

	if !strings.Contains(got, l.logFile) {
		t.Errorf("the log path is missing:\n%s", got)
	}
}

func TestLaunchdPlistEscapesXML(t *testing.T) {
	// A home directory can contain an ampersand, which would otherwise
	// produce a plist launchd silently refuses to load.
	l, _ := newTestLaunchd(t)
	got := string(l.plist("/Users/a&b/stoplight"))

	if strings.Contains(got, "a&b") {
		t.Errorf("the ampersand was not escaped:\n%s", got)
	}
	if !strings.Contains(got, "a&amp;b") {
		t.Errorf("the escaped path is missing:\n%s", got)
	}
	var v any
	if err := xml.Unmarshal([]byte(got), &v); err != nil {
		t.Errorf("the escaped plist is not valid XML: %v", err)
	}
}

func TestLaunchdInstallWritesThePlist(t *testing.T) {
	l, rec := newTestLaunchd(t)

	if err := l.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if _, err := os.Stat(l.plistPath); err != nil {
		t.Fatalf("the plist was not written: %v", err)
	}
	if !rec.ran("launchctl", "bootstrap") {
		t.Errorf("bootstrap was not called, calls: %+v", rec.calls)
	}
	// The log directory must exist or launchd refuses to start the job.
	if _, err := os.Stat(filepath.Dir(l.logFile)); err != nil {
		t.Errorf("the log directory was not created: %v", err)
	}
}

func TestLaunchdInstallIsIdempotent(t *testing.T) {
	l, _ := newTestLaunchd(t)

	if err := l.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	first, err := os.ReadFile(l.plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	second, err := os.ReadFile(l.plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("the plist changed on the second install")
	}
}

func TestLaunchdUninstallRemovesThePlist(t *testing.T) {
	l, rec := newTestLaunchd(t)

	if err := l.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatal(err)
	}
	if err := l.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(l.plistPath); !os.IsNotExist(err) {
		t.Error("the plist survived uninstall")
	}
	if !rec.ran("launchctl", "bootout") {
		t.Errorf("bootout was not called, calls: %+v", rec.calls)
	}
}

func TestLaunchdUninstallWhenNotInstalled(t *testing.T) {
	// Already absent is the desired state, not an error.
	l, _ := newTestLaunchd(t)
	if err := l.Uninstall(); err != nil {
		t.Errorf("Uninstall with no plist: %v", err)
	}
}

func TestLaunchdStartWithoutInstallFails(t *testing.T) {
	l, _ := newTestLaunchd(t)
	err := l.Start()
	if err == nil {
		t.Fatal("Start succeeded with no plist")
	}
	if !strings.Contains(err.Error(), "not installed") {
		t.Errorf("the error does not explain the cause: %v", err)
	}
}

func TestLaunchdTargetsTheUserDomain(t *testing.T) {
	// gui/<uid> is the per-user domain. A system domain would need root.
	l, _ := newTestLaunchd(t)
	if got := l.domain(); got != "gui/501" {
		t.Errorf("domain = %q, want gui/501", got)
	}
	if got := l.target(); got != "gui/501/com.stoplight.relay" {
		t.Errorf("target = %q, want gui/501/com.stoplight.relay", got)
	}
}

func TestLaunchdCommandIsTransparent(t *testing.T) {
	l, _ := newTestLaunchd(t)
	got := l.Command()
	if !strings.Contains(got, "launchctl") || !strings.Contains(got, l.plistPath) {
		t.Errorf("Command = %q, want it to name launchctl and the plist", got)
	}
}

func TestLaunchdRunning(t *testing.T) {
	tests := []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"running", "state = running\n", nil, true},
		{"loaded but stopped", "state = not running\n", nil, false},
		{"not loaded", "Could not find service", errFake, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			l, rec := newTestLaunchd(t)
			rec.out = []byte(tc.out)
			rec.err = tc.err

			got, err := l.Running()
			if err != nil {
				t.Fatalf("Running: %v", err)
			}
			if got != tc.want {
				t.Errorf("Running = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestLaunchdBootstrapToleratesAlreadyLoaded(t *testing.T) {
	// launchctl returns a generic failure for this, but the desired state
	// is already met.
	l, rec := newTestLaunchd(t)
	rec.out = []byte("Load failed: 5: Input/output error: already bootstrapped")
	rec.err = errFake

	if err := l.bootstrap(); err != nil {
		t.Errorf("bootstrap treated an already-loaded agent as an error: %v", err)
	}
}

func TestLaunchdBootoutToleratesNotLoaded(t *testing.T) {
	l, rec := newTestLaunchd(t)
	rec.out = []byte("Boot-out failed: 3: No such process")
	rec.err = errFake

	if err := l.bootout(); err != nil {
		t.Errorf("bootout treated a missing agent as an error: %v", err)
	}
}

func TestLaunchdDisableStopsAndDisables(t *testing.T) {
	l, rec := newTestLaunchd(t)
	if err := l.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatal(err)
	}
	rec.calls = nil

	if err := l.Disable(); err != nil {
		t.Fatalf("Disable: %v", err)
	}
	if !rec.ran("launchctl", "bootout") {
		t.Errorf("bootout was not called, calls: %+v", rec.calls)
	}
	if !rec.ran("launchctl", "disable") {
		t.Errorf("disable was not called, calls: %+v", rec.calls)
	}
	// The plist stays so Start can re-enable without a reinstall.
	if _, err := os.Stat(l.plistPath); err != nil {
		t.Errorf("Disable removed the plist: %v", err)
	}
}

func TestLaunchdNeverUsesSudo(t *testing.T) {
	l, rec := newTestLaunchd(t)
	if err := l.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatal(err)
	}
	_ = l.Start()
	_ = l.Stop()
	_ = l.Disable()
	_ = l.Uninstall()

	for _, c := range rec.calls {
		if c.name == "sudo" {
			t.Errorf("a command was run with sudo: %+v", c)
		}
		for _, a := range c.args {
			if strings.Contains(a, "/Library/LaunchDaemons") {
				t.Errorf("a system-level path was used: %+v", c)
			}
		}
	}
}

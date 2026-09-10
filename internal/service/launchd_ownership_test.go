//go:build darwin

package service

import (
	"os"
	"strings"
	"testing"
)

// foreignPlist is a plist that did not come from this package: no marker.
const foreignPlist = `<?xml version="1.0" encoding="UTF-8"?>
<plist version="1.0">
<dict>
	<key>Label</key>
	<string>com.stoplight.relay</string>
	<key>ProgramArguments</key>
	<array><string>/opt/somebody-else/relay</string></array>
</dict>
</plist>
`

// TestLaunchdPlistCarriesTheMarker is the precondition for every ownership
// check: our own output must be recognisable as ours.
func TestLaunchdPlistCarriesTheMarker(t *testing.T) {
	l, _ := newTestLaunchd(t)
	if got := string(l.plist("/usr/local/bin/stoplight")); !strings.Contains(got, serviceMarker) {
		t.Errorf("the generated plist has no marker:\n%s", got)
	}
}

// TestLaunchdInstallRefusesAForeignPlist covers a plist at our path that
// another tool wrote. Overwriting it destroys someone else's job
// definition.
func TestLaunchdInstallRefusesAForeignPlist(t *testing.T) {
	l, rec := newTestLaunchd(t)
	if err := os.WriteFile(l.plistPath, []byte(foreignPlist), 0o644); err != nil {
		t.Fatal(err)
	}

	err := l.Install("/usr/local/bin/stoplight")
	if err == nil {
		t.Fatalf("Install overwrote a plist it did not write")
	}
	if !strings.Contains(err.Error(), "not written by Stoplight") {
		t.Errorf("the error does not explain the problem: %v", err)
	}

	got, rerr := os.ReadFile(l.plistPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != foreignPlist {
		t.Errorf("the foreign plist was modified:\n%s", got)
	}
	if rec.ran("launchctl", "bootstrap") {
		t.Errorf("launchctl ran despite the refusal")
	}
}

// TestLaunchdUninstallRefusesAForeignPlist covers the same file at
// uninstall, where the damage is a deletion rather than an overwrite.
func TestLaunchdUninstallRefusesAForeignPlist(t *testing.T) {
	l, _ := newTestLaunchd(t)
	if err := os.WriteFile(l.plistPath, []byte(foreignPlist), 0o644); err != nil {
		t.Fatal(err)
	}

	err := l.Uninstall()
	if err == nil {
		t.Fatalf("Uninstall deleted a plist it did not write")
	}
	if !strings.Contains(err.Error(), "not written by Stoplight") {
		t.Errorf("the error does not explain the problem: %v", err)
	}
	if _, serr := os.Stat(l.plistPath); serr != nil {
		t.Errorf("the foreign plist was deleted: %v", serr)
	}
}

// TestLaunchdReinstallKeepsTightenedPermissions covers a user who
// chmodded the generated plist to 0600.
func TestLaunchdReinstallKeepsTightenedPermissions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits")
	}
	l, _ := newTestLaunchd(t)
	if err := l.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	if err := os.Chmod(l.plistPath, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := l.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatalf("second Install: %v", err)
	}

	info, err := os.Stat(l.plistPath)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %04o, want 0600: the user's chmod was reset", perm)
	}
}

// TestLaunchdInstallAndUninstallRoundTrip confirms the ownership checks do
// not block the normal path.
func TestLaunchdInstallAndUninstallRoundTrip(t *testing.T) {
	l, _ := newTestLaunchd(t)
	if err := l.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := l.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatalf("reinstall over our own plist: %v", err)
	}
	if err := l.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(l.plistPath); !os.IsNotExist(err) {
		t.Errorf("our own plist survived uninstall")
	}
}

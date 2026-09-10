package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests cover the ways this package can destroy a file it did not
// write. A plist or unit file at our path may have come from a package
// manager, a colleague's dotfiles, or the user's own hand, and neither
// Install nor Uninstall may assume it is ours.

// TestWriteFileAtomicCarriesExistingPermissions covers a user who tightened
// a generated file to 0600. A reinstall must not widen it back to 0644.
func TestWriteFileAtomicCarriesExistingPermissions(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits")
	}
	path := filepath.Join(t.TempDir(), "unit")
	if err := writeFileAtomic(path, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeFileAtomic(path, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("permissions = %04o, want 0600: the user's chmod was reset", perm)
	}
}

// TestOwnedRecognisesOurOwnOutput checks the marker test against content
// this package generates, and against content it does not.
func TestOwnedRecognisesOurOwnOutput(t *testing.T) {
	dir := t.TempDir()

	ours := filepath.Join(dir, "ours")
	if err := os.WriteFile(ours, []byte("prefix\n"+serviceMarker+"\nrest\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !owned(ours) {
		t.Errorf("a file carrying the marker was not recognised as ours")
	}

	theirs := filepath.Join(dir, "theirs")
	if err := os.WriteFile(theirs, []byte("[Unit]\nDescription=Someone else\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if owned(theirs) {
		t.Errorf("a foreign file was claimed as ours")
	}

	// A file that is not there is not someone else's, so writing is fine.
	if !owned(filepath.Join(dir, "missing")) {
		t.Errorf("a missing path should not block a write")
	}
}

// TestOwnedIgnoresTheMarkerQuotedMidLine covers the collision an
// unanchored substring test creates. A foreign file that merely mentions
// the marker, in a description or a comment of its own, is not ours, and
// claiming it would let Install overwrite it and Uninstall delete it.
func TestOwnedIgnoresTheMarkerQuotedMidLine(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"quoted in a description": "[Unit]\nDescription=Unlike Stoplight, which says \"" + serviceMarker + "\"\n",
		"quoted in a comment":     "# my notes: upstream writes \"" + serviceMarker + "\" at the top\n[Unit]\n",
		"trailing on a directive": "[Service]\nExecStart=/opt/relay # " + serviceMarker + "\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if owned(path) {
				t.Errorf("a foreign file mentioning the marker was claimed as ours:\n%s", content)
			}
		})
	}
}

// TestOwnedAcceptsTheMarkerOnItsOwnLine covers the shapes this package
// actually writes, so the anchoring does not lock us out of our own files.
func TestOwnedAcceptsTheMarkerOnItsOwnLine(t *testing.T) {
	dir := t.TempDir()
	cases := map[string]string{
		"systemd comment": "# " + serviceMarker + "\n[Unit]\n",
		"plist comment":   "<plist>\n<!-- " + serviceMarker + " -->\n<dict>\n",
		"bare line":       "prefix\n" + serviceMarker + "\nrest\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(dir, strings.ReplaceAll(name, " ", "-"))
			if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
			if !owned(path) {
				t.Errorf("a file this package writes was not recognised as ours:\n%s", content)
			}
		})
	}
}

// TestCheckOwnedRefusesAForeignFile is the guard Install and Uninstall
// both go through.
func TestCheckOwnedRefusesAForeignFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "stoplight.service")
	if err := os.WriteFile(path, []byte("[Unit]\nDescription=Not ours\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	err := checkOwned(path)
	if err == nil {
		t.Fatalf("checkOwned accepted a file without the marker")
	}
	if !strings.Contains(err.Error(), "not written by Stoplight") {
		t.Errorf("the error does not explain the problem: %v", err)
	}
}

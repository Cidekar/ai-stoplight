package service

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// errFake stands in for a non-zero exit from a platform tool. The
// platform managers key off the tool's output rather than the error
// itself, so the value only needs to be non-nil.
var errFake = errors.New("exit status 1")

func TestNewReturnsAManager(t *testing.T) {
	// Every platform this project builds for has a manager, so New must
	// not fail on the machine running the tests.
	m, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m == nil {
		t.Fatal("New returned a nil Manager")
	}
}

func TestCommandIsNotEmpty(t *testing.T) {
	// `stoplight status` prints this so the wrapper is never a black box.
	m, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cmd := m.Command()
	if strings.TrimSpace(cmd) == "" {
		t.Error("Command returned an empty string")
	}
}

func TestCommandNamesThePlatformTool(t *testing.T) {
	m, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	cmd := m.Command()
	// One of the three platform tools must appear, whichever we are on.
	tools := []string{"launchctl", "systemctl", "schtasks"}
	found := false
	for _, tool := range tools {
		if strings.Contains(cmd, tool) {
			found = true
		}
	}
	if !found {
		t.Errorf("Command %q names none of %v", cmd, tools)
	}
}

func TestCommandNeverUsesSudo(t *testing.T) {
	// Every manager is user-level. The relay needs no privileges, and a
	// system service would prompt for a password and gain nothing.
	m, err := New()
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if strings.Contains(m.Command(), "sudo") {
		t.Errorf("Command uses sudo: %q", m.Command())
	}
}

func TestLogPathIsUnderTheStateDir(t *testing.T) {
	// The relay and the service definition must agree on one path, or
	// `stoplight logs` tails a file nothing writes to.
	got, err := logPath()
	if err != nil {
		t.Fatalf("logPath: %v", err)
	}
	if filepath.Base(got) != "stoplight.log" {
		t.Errorf("logPath = %q, want it to end in stoplight.log", got)
	}
	if !strings.Contains(filepath.ToSlash(got), ".local/state/stoplight") {
		t.Errorf("logPath = %q, want it under .local/state/stoplight", got)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "unit")

	if err := writeFileAtomic(path, []byte("hello"), 0o644); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if string(got) != "hello" {
		t.Errorf("contents = %q, want hello", got)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o644 {
		t.Errorf("permissions = %o, want 644", perm)
	}
}

func TestWriteFileAtomicOverwrites(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unit")

	if err := writeFileAtomic(path, []byte("first"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, []byte("second"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "second" {
		t.Errorf("contents = %q, want second", got)
	}
}

func TestWriteFileAtomicLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	if err := writeFileAtomic(filepath.Join(dir, "unit"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".stoplight-") {
			t.Errorf("a temporary file was left behind: %s", e.Name())
		}
	}
}

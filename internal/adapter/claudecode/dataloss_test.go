package claudecode

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// These tests hold the line on the ways this package can destroy a real
// user's settings.json. Every fixture here is deliberately shaped unlike
// anything Stoplight itself writes: a file that already existed, an empty
// array the user put there, a command that merely mentions the marker, a
// symlink into a dotfiles repo, a read-only file, CRLF line endings.

// TestUninstallKeepsAFileItDidNotCreate covers the case where the file was
// already on disk before the first install. Uninstall may empty it, but it
// must never unlink a file another tool or the user created.
func TestUninstallKeepsAFileItDidNotCreate(t *testing.T) {
	for _, original := range []string{
		`{}`,
		`{"hooks":{}}`,
		`{"hooks":{"Stop":[]}}`,
		"{\n}\n",
	} {
		path := write(t, original)
		a := newForPath(path)

		if err := a.Install(testBin); err != nil {
			t.Fatalf("Install into %q: %v", original, err)
		}
		if err := a.Uninstall(); err != nil {
			t.Fatalf("Uninstall from %q: %v", original, err)
		}

		if _, err := os.Stat(path); err != nil {
			t.Errorf("original %q: the settings file was deleted: %v", original, err)
			continue
		}
		// Whatever survives must still parse as a JSON object.
		if _, err := decodeObject([]byte(read(t, path))); err != nil {
			t.Errorf("original %q: the surviving file does not parse: %v", original, err)
		}
	}
}

// TestUninstallKeepsAnEmptyArrayTheUserWrote covers an event key the user
// left as an empty array. Install appends to it, so uninstall must put it
// back the way it was rather than deleting the key.
func TestUninstallKeepsAnEmptyArrayTheUserWrote(t *testing.T) {
	original := `{"model":"opus","hooks":{"Stop":[]}}`
	path := write(t, original)
	a := newForPath(path)

	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := a.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	root, err := decodeObject([]byte(read(t, path)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if _, ok := root.Get("model"); !ok {
		t.Errorf("the \"model\" key was lost:\n%s", read(t, path))
	}
	hooksVal, ok := root.Get("hooks")
	if !ok {
		t.Fatalf("the \"hooks\" key was removed, but the user wrote it:\n%s", read(t, path))
	}
	hooks, ok := hooksVal.(*object)
	if !ok {
		t.Fatalf("\"hooks\" is not an object: %T", hooksVal)
	}
	stop, ok := hooks.Get("Stop")
	if !ok {
		t.Fatalf("the user's \"Stop\": [] was deleted:\n%s", read(t, path))
	}
	groups, ok := stop.([]any)
	if !ok {
		t.Fatalf("\"Stop\" is not an array: %T", stop)
	}
	if len(groups) != 0 {
		t.Errorf("\"Stop\" should be back to empty, got %d groups:\n%s", len(groups), read(t, path))
	}
}

// TestUninstallKeepsAnEventAnotherToolLeftEmpty is the case the existing
// comment in Uninstall claims to handle: an event that arrived empty and
// that we never touched.
func TestUninstallKeepsAnEventAnotherToolLeftEmpty(t *testing.T) {
	// PreToolUse is not in our mapping table, so install never touches it.
	original := `{"hooks":{"PreToolUse":[]}}`
	path := write(t, original)
	a := newForPath(path)

	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := a.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	got := read(t, path)
	if !strings.Contains(got, "PreToolUse") {
		t.Errorf("an event another tool left empty was deleted:\n%s", got)
	}
}

// userMarkerHook is a hook the user wrote that happens to mention the
// marker token. It is not ours: it does not start with the generated
// prefix.
const userMarkerHook = `echo my own stoplight-hook wrapper >> ~/log`

// TestUninstallKeepsAUserHookMentioningTheMarker guards the substring
// match. A user command that merely contains "stoplight-hook" is not ours
// to remove.
func TestUninstallKeepsAUserHookMentioningTheMarker(t *testing.T) {
	original := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"` + userMarkerHook + `"}]}]}}`
	path := write(t, original)
	a := newForPath(path)

	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := read(t, path); !strings.Contains(got, userMarkerHook) {
		t.Fatalf("install destroyed the user's hook:\n%s", got)
	}
	if err := a.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	got := read(t, path)
	if !strings.Contains(got, userMarkerHook) {
		t.Errorf("uninstall removed the user's own hook:\n%s", got)
	}
}

// TestInstallKeepsSiblingFieldsOfALookalike covers the in-place overwrite.
// A user entry that mentions the marker carries fields we do not know
// about, and replacing the whole object drops them.
func TestInstallKeepsSiblingFieldsOfALookalike(t *testing.T) {
	original := `{"hooks":{"Stop":[{"hooks":[{"type":"command","command":"echo stoplight-hook mine","timeout":99}]}]}}`
	path := write(t, original)
	a := newForPath(path)

	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}

	got := read(t, path)
	if !strings.Contains(got, `"timeout"`) || !strings.Contains(got, "99") {
		t.Errorf("the sibling \"timeout\": 99 was dropped:\n%s", got)
	}
	if !strings.Contains(got, "echo stoplight-hook mine") {
		t.Errorf("the user's command was overwritten:\n%s", got)
	}
}

// TestInstallCollapsesDuplicateEntries covers a file that already holds
// two of our entries, as an interrupted install or a hand-merged config
// can leave behind. Install must end with exactly one, or Claude Code
// fires the hook twice per event.
func TestInstallCollapsesDuplicateEntries(t *testing.T) {
	ours := hookCommand("/old/stoplight", "finished")
	original := `{"hooks":{"Stop":[` +
		`{"hooks":[{"type":"command","command":` + quoteJSON(ours) + `}]},` +
		`{"hooks":[{"type":"command","command":` + quoteJSON(ours) + `}]}` +
		`]}}`
	path := write(t, original)
	a := newForPath(path)

	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if got := countOurs(t, path, "Stop"); got != 1 {
		t.Errorf("got %d Stoplight entries under Stop, want exactly 1:\n%s", got, read(t, path))
	}
}

// TestInstallCollapsesDuplicatesInOneGroup covers the same duplication
// inside a single matcher group.
func TestInstallCollapsesDuplicatesInOneGroup(t *testing.T) {
	ours := hookCommand("/old/stoplight", "finished")
	original := `{"hooks":{"Stop":[{"hooks":[` +
		`{"type":"command","command":` + quoteJSON(ours) + `},` +
		`{"type":"command","command":` + quoteJSON(ours) + `}` +
		`]}]}}`
	path := write(t, original)
	a := newForPath(path)

	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}

	if got := countOurs(t, path, "Stop"); got != 1 {
		t.Errorf("got %d Stoplight entries under Stop, want exactly 1:\n%s", got, read(t, path))
	}
}

// quoteJSON renders a Go string as a JSON string literal for a fixture.
func quoteJSON(s string) string {
	b, err := marshalValue(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestSaveFollowsASymlink covers a settings.json symlinked into a dotfiles
// repo. Renaming onto the link replaces it with a regular file, and the
// repo copy silently stops taking effect.
func TestSaveFollowsASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need extra privileges on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "settings.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(`{"editorMode":"vim"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	a := newForPath(link)
	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat the link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced by a regular file")
	}
	if got := read(t, target); !strings.Contains(got, marker) {
		t.Errorf("the symlink target was not updated:\n%s", got)
	}
}

// TestUninstallKeepsASymlink checks the same for the write-back path.
func TestUninstallKeepsASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need extra privileges on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "dotfiles", "settings.json")
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte(`{"editorMode":"vim"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "settings.json")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	a := newForPath(link)
	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := a.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	info, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("lstat the link: %v", err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink was replaced by a regular file")
	}
	if got := read(t, target); !strings.Contains(got, "vim") {
		t.Errorf("the symlink target lost its contents:\n%s", got)
	}
}

// TestInstallRefusesAReadOnlyFile covers a config the user chmodded to
// 0444. A rename needs directory write permission, not file write
// permission, so without a check the chmod is silently bypassed.
func TestInstallRefusesAReadOnlyFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the permission bits")
	}
	original := `{"editorMode":"vim"}`
	path := write(t, original)
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o644) })

	a := newForPath(path)
	err := a.Install(testBin)
	if err == nil {
		t.Fatalf("Install succeeded against a read-only file:\n%s", read(t, path))
	}
	if !strings.Contains(err.Error(), "read-only") && !strings.Contains(err.Error(), "not writable") {
		t.Errorf("the error does not explain the problem: %v", err)
	}
	if got := read(t, path); got != original {
		t.Errorf("the read-only file was changed:\nwant %q\ngot  %q", original, got)
	}
}

// TestCRLFIsPreserved covers a config written on Windows or by an editor
// configured for CRLF. Rewriting it with LF is a whole-file diff.
func TestCRLFIsPreserved(t *testing.T) {
	original := "{\r\n  \"editorMode\": \"vim\"\r\n}\r\n"
	path := write(t, original)
	a := newForPath(path)

	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}

	got := read(t, path)
	if strings.Contains(strings.ReplaceAll(got, "\r\n", ""), "\n") {
		t.Errorf("a bare LF survived in a CRLF file:\n%q", got)
	}
	if !strings.Contains(got, "\r\n") {
		t.Errorf("CRLF was converted to LF:\n%q", got)
	}
}

// TestLFStaysLF is the other half: an LF file must not gain carriage
// returns.
func TestLFStaysLF(t *testing.T) {
	path := write(t, "{\n  \"editorMode\": \"vim\"\n}\n")
	a := newForPath(path)

	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := read(t, path); strings.Contains(got, "\r") {
		t.Errorf("a carriage return was introduced:\n%q", got)
	}
}

// TestIsOursRequiresThePrefix pins the anchoring rule down at the unit
// level, so a future change to hookCommand cannot loosen it by accident.
func TestIsOursRequiresThePrefix(t *testing.T) {
	ours := newObject()
	ours.Set("type", "command")
	ours.Set("command", hookCommand(testBin, "idle"))
	if !isOurs(ours) {
		t.Errorf("a command this package generated was not recognised as ours")
	}

	for _, cmd := range []string{
		userMarkerHook,
		"echo stoplight-hook mine",
		"stoplight-hook",
		"# : stoplight-hook; something",
		"",
	} {
		e := newObject()
		e.Set("type", "command")
		e.Set("command", cmd)
		if isOurs(e) {
			t.Errorf("a foreign command was claimed as ours: %q", cmd)
		}
	}
}

// TestGeneratedCommandStartsWithTheMarkerPrefix is the invariant the
// anchored match depends on. If hookCommand ever stops opening with the
// prefix, isOurs stops recognising our own entries and uninstall leaves
// them behind, so this must fail loudly.
func TestGeneratedCommandStartsWithTheMarkerPrefix(t *testing.T) {
	for _, m := range mappings {
		cmd := hookCommand(testBin, m.Event)
		if !strings.HasPrefix(cmd, markerPrefix) {
			t.Errorf("hook %s: the command does not start with %q:\n%s", m.Hook, markerPrefix, cmd)
		}
	}
}

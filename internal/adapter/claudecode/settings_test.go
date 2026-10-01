package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testBin = "/usr/local/bin/stoplight"

// write puts contents at a fresh temporary settings path and returns it.
func write(t *testing.T, contents string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "settings.json")
	if contents != "" {
		if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
			t.Fatalf("seed the settings file: %v", err)
		}
	}
	return path
}

// read returns the file's contents as a string.
func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// decode parses the file into a generic map.
func decode(t *testing.T, path string) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(read(t, path)), &m); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return m
}

// countOurs returns how many Stoplight entries exist under one hook event.
func countOurs(t *testing.T, path, event string) int {
	t.Helper()
	root, err := decodeObject([]byte(read(t, path)))
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	hooksVal, ok := root.Get("hooks")
	if !ok {
		return 0
	}
	hooks, ok := hooksVal.(*object)
	if !ok {
		return 0
	}
	groupsVal, ok := hooks.Get(event)
	if !ok {
		return 0
	}
	groups, ok := groupsVal.([]any)
	if !ok {
		return 0
	}
	n := 0
	for _, g := range groups {
		group, ok := g.(*object)
		if !ok {
			continue
		}
		entriesVal, ok := group.Get("hooks")
		if !ok {
			continue
		}
		entries, ok := entriesVal.([]any)
		if !ok {
			continue
		}
		for _, e := range entries {
			if isOurs(e) {
				n++
			}
		}
	}
	return n
}

func TestInstallCreatesMissingFile(t *testing.T) {
	// A settings file that does not exist yet is the common first-run
	// case and must not be an error.
	path := filepath.Join(t.TempDir(), "nested", "settings.json")
	a := newForPath(path)

	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}

	for _, m := range mappings {
		if got := countOurs(t, path, m.Hook); got != 1 {
			t.Errorf("hook %s: got %d entries, want 1", m.Hook, got)
		}
	}
}

func TestInstallIntoEmptyFile(t *testing.T) {
	// Some tools touch the file before writing to it.
	for _, contents := range []string{"", "   \n\t\n", "{}"} {
		path := write(t, contents)
		if contents == "" {
			// write skips creating the file for "", so make it empty here.
			if err := os.WriteFile(path, nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		a := newForPath(path)
		if err := a.Install(testBin); err != nil {
			t.Fatalf("Install into %q: %v", contents, err)
		}
		if got := countOurs(t, path, "Stop"); got != 1 {
			t.Errorf("contents %q: got %d Stop entries, want 1", contents, got)
		}
	}
}

func TestInstallPreservesUnknownTopLevelKeys(t *testing.T) {
	// The whole risk of this package: a user's real config carries keys
	// this adapter has never heard of, and every one must survive.
	original := `{
  "statusLine": {
    "type": "command",
    "command": "~/.claude/statusline.sh"
  },
  "editorMode": "vim",
  "alwaysThinkingEnabled": false,
  "someFutureKey": {"nested": [1, 2, 3]},
  "cleanupPeriodDays": 30
}`
	path := write(t, original)
	a := newForPath(path)
	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}

	root := decode(t, path)
	status, ok := root["statusLine"].(map[string]any)
	if !ok || status["command"] != "~/.claude/statusline.sh" {
		t.Errorf("statusLine was not preserved: %#v", root["statusLine"])
	}
	if root["editorMode"] != "vim" {
		t.Errorf("editorMode = %v, want vim", root["editorMode"])
	}
	if root["alwaysThinkingEnabled"] != false {
		t.Errorf("alwaysThinkingEnabled = %v, want false", root["alwaysThinkingEnabled"])
	}
	if root["someFutureKey"] == nil {
		t.Error("someFutureKey was dropped")
	}
	// A number must not come back as 1e+01 or similar.
	if got := read(t, path); !strings.Contains(got, `"cleanupPeriodDays": 30`) {
		t.Errorf("cleanupPeriodDays was not preserved verbatim:\n%s", got)
	}
}

func TestInstallPreservesOtherToolsHooks(t *testing.T) {
	// Another tool's hook on the same event must survive, including its
	// matcher and any fields we do not understand.
	original := `{
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {"type": "command", "command": "afplay /System/Library/Sounds/Glass.aiff"}
        ]
      }
    ],
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {"type": "command", "command": "my-linter", "timeout": 30}
        ]
      }
    ]
  }
}`
	path := write(t, original)
	a := newForPath(path)
	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}

	got := read(t, path)
	if !strings.Contains(got, "Glass.aiff") {
		t.Errorf("the other tool's Stop hook was dropped:\n%s", got)
	}
	if !strings.Contains(got, "my-linter") {
		t.Errorf("the PreToolUse hook was dropped:\n%s", got)
	}
	if !strings.Contains(got, `"matcher": "Bash"`) {
		t.Errorf("the matcher was dropped:\n%s", got)
	}
	if !strings.Contains(got, `"timeout": 30`) {
		t.Errorf("the timeout was dropped:\n%s", got)
	}
	if n := countOurs(t, path, "Stop"); n != 1 {
		t.Errorf("got %d Stoplight Stop entries, want 1", n)
	}
}

func TestInstallIsIdempotent(t *testing.T) {
	// Running install twice must produce a byte-identical file. This is
	// the invariant in design.md.
	path := write(t, `{"editorMode":"vim","hooks":{"Stop":[{"hooks":[{"type":"command","command":"other"}]}]}}`)
	a := newForPath(path)

	if err := a.Install(testBin); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	first := read(t, path)

	if err := a.Install(testBin); err != nil {
		t.Fatalf("second Install: %v", err)
	}
	second := read(t, path)

	if first != second {
		t.Errorf("install is not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
	for _, m := range mappings {
		if n := countOurs(t, path, m.Hook); n != 1 {
			t.Errorf("hook %s: got %d entries after two installs, want 1", m.Hook, n)
		}
	}
}

func TestInstallRefreshesBinaryPath(t *testing.T) {
	// Reinstalling from a new location must rewrite the command in place
	// rather than leave a stale path or add a second entry.
	path := write(t, "{}")
	a := newForPath(path)

	if err := a.Install("/old/path/stoplight"); err != nil {
		t.Fatalf("first Install: %v", err)
	}
	if err := a.Install("/new/path/stoplight"); err != nil {
		t.Fatalf("second Install: %v", err)
	}

	got := read(t, path)
	if strings.Contains(got, "/old/path/stoplight") {
		t.Errorf("the stale binary path survived:\n%s", got)
	}
	if !strings.Contains(got, "/new/path/stoplight") {
		t.Errorf("the new binary path is missing:\n%s", got)
	}
	if n := countOurs(t, path, "Stop"); n != 1 {
		t.Errorf("got %d Stop entries, want 1", n)
	}
}

func TestUninstallRestoresOriginal(t *testing.T) {
	// The headline guarantee: with other entries present, uninstall
	// returns the file to what it was.
	original := `{
  "editorMode": "vim",
  "hooks": {
    "Stop": [
      {
        "hooks": [
          {
            "type": "command",
            "command": "afplay /System/Library/Sounds/Glass.aiff"
          }
        ]
      }
    ],
    "PreToolUse": [
      {
        "matcher": "Bash",
        "hooks": [
          {
            "type": "command",
            "command": "my-linter"
          }
        ]
      }
    ]
  }
}
`
	path := write(t, original)
	a := newForPath(path)

	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := a.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	if got := read(t, path); got != original {
		t.Errorf("uninstall did not restore the original:\nwant:\n%s\ngot:\n%s", original, got)
	}
}

func TestUninstallRemovesFileItCreated(t *testing.T) {
	// Installing into a missing file and then uninstalling should leave
	// no stub behind.
	path := filepath.Join(t.TempDir(), "settings.json")
	a := newForPath(path)

	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := a.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the settings file survived with contents: %q", read(t, path))
	}
}

func TestUninstallFromAFreshProcessRemovesFileItCreated(t *testing.T) {
	// `stoplight install` and `stoplight uninstall` are separate processes, so
	// uninstall runs on a brand new adapter with none of install's in-memory
	// state. Installing into a missing file and uninstalling from a second
	// adapter must still leave no stub behind.
	path := filepath.Join(t.TempDir(), "settings.json")

	if err := newForPath(path).Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := newForPath(path).Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("a fresh-process uninstall left a stub behind: %q", read(t, path))
	}
}

func TestUninstallFromAFreshProcessRemovesEmptyArrays(t *testing.T) {
	// With a file that holds other settings, uninstall cannot delete the file,
	// but it must still restore it to what install found rather than leaving a
	// row of empty hook arrays and an empty "hooks" object behind.
	path := write(t, `{"model":"opus"}`)

	if err := newForPath(path).Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := newForPath(path).Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	root := decode(t, path)
	if root["model"] != "opus" {
		t.Errorf("the \"model\" key was lost:\n%s", read(t, path))
	}
	if _, ok := root["hooks"]; ok {
		t.Errorf("an emptied \"hooks\" object was left behind:\n%s", read(t, path))
	}
}

func TestUninstallKeepsForeignGroupInSameEvent(t *testing.T) {
	// A group carrying a matcher and another tool's entry must not be
	// removed just because we pruned nothing from it.
	original := `{"hooks":{"Notification":[{"matcher":"permission_prompt","hooks":[{"type":"command","command":"notify-send hi"}]}]}}`
	path := write(t, original)
	a := newForPath(path)

	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := a.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}

	got := read(t, path)
	if !strings.Contains(got, "notify-send hi") {
		t.Errorf("the foreign entry was removed:\n%s", got)
	}
	if !strings.Contains(got, "permission_prompt") {
		t.Errorf("the matcher was removed:\n%s", got)
	}
	if strings.Contains(got, marker) {
		t.Errorf("a Stoplight entry survived uninstall:\n%s", got)
	}
}

func TestUninstallIsIdempotent(t *testing.T) {
	path := write(t, `{"editorMode":"vim"}`)
	a := newForPath(path)

	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if err := a.Uninstall(); err != nil {
		t.Fatalf("first Uninstall: %v", err)
	}
	first := read(t, path)

	if err := a.Uninstall(); err != nil {
		t.Fatalf("second Uninstall: %v", err)
	}
	if second := read(t, path); first != second {
		t.Errorf("uninstall is not idempotent:\nfirst:\n%s\nsecond:\n%s", first, second)
	}
}

func TestUninstallMissingFileIsNoOp(t *testing.T) {
	a := newForPath(filepath.Join(t.TempDir(), "settings.json"))
	if err := a.Uninstall(); err != nil {
		t.Errorf("Uninstall on a missing file: %v", err)
	}
}

func TestInvalidJSONIsAnErrorAndLeavesTheFileAlone(t *testing.T) {
	// Overwriting a file we cannot parse would destroy hand-written
	// settings, so every entry point must refuse.
	const broken = `{"editorMode": "vim",  << not json >> }`
	path := write(t, broken)
	a := newForPath(path)

	if err := a.Install(testBin); err == nil {
		t.Error("Install accepted invalid JSON")
	} else if !strings.Contains(err.Error(), "parse") {
		t.Errorf("the error does not mention parsing: %v", err)
	}
	if got := read(t, path); got != broken {
		t.Errorf("the invalid file was modified:\n%s", got)
	}

	if err := a.Uninstall(); err == nil {
		t.Error("Uninstall accepted invalid JSON")
	}
	if _, err := a.Installed(); err == nil {
		t.Error("Installed accepted invalid JSON")
	}
	if got := read(t, path); got != broken {
		t.Errorf("the invalid file was modified:\n%s", got)
	}
}

func TestTrailingGarbageIsAnError(t *testing.T) {
	// Decoding only the first object would silently drop the rest.
	path := write(t, `{"a":1}{"b":2}`)
	if _, err := loadSettings(path); err == nil {
		t.Error("loadSettings accepted trailing data")
	}
}

func TestNonObjectHooksKeyIsAnError(t *testing.T) {
	// Replacing a hooks key holding something unexpected would lose data,
	// so report it instead.
	path := write(t, `{"hooks": "surprise"}`)
	a := newForPath(path)
	if err := a.Install(testBin); err == nil {
		t.Error("Install accepted a string hooks key")
	}
}

func TestInstalled(t *testing.T) {
	path := write(t, `{"editorMode":"vim"}`)
	a := newForPath(path)

	if got, err := a.Installed(); err != nil || got {
		t.Errorf("Installed before install = %v, %v; want false, nil", got, err)
	}
	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got, err := a.Installed(); err != nil || !got {
		t.Errorf("Installed after install = %v, %v; want true, nil", got, err)
	}
	if err := a.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if got, err := a.Installed(); err != nil || got {
		t.Errorf("Installed after uninstall = %v, %v; want false, nil", got, err)
	}
}

func TestInstalledMissingFile(t *testing.T) {
	a := newForPath(filepath.Join(t.TempDir(), "settings.json"))
	got, err := a.Installed()
	if err != nil {
		t.Fatalf("Installed: %v", err)
	}
	if got {
		t.Error("Installed reported true for a missing file")
	}
}

func TestIndentIsPreserved(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{"two spaces", "{\n  \"editorMode\": \"vim\"\n}", "  "},
		{"four spaces", "{\n    \"editorMode\": \"vim\"\n}", "    "},
		{"tabs", "{\n\t\"editorMode\": \"vim\"\n}", "\t"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := write(t, tc.input)
			a := newForPath(path)
			if err := a.Install(testBin); err != nil {
				t.Fatalf("Install: %v", err)
			}
			got := read(t, path)
			// The second line is the first indented one.
			lines := strings.Split(got, "\n")
			if len(lines) < 2 {
				t.Fatalf("output is too short:\n%s", got)
			}
			line := lines[1]
			indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
			if indent != tc.expect {
				t.Errorf("indent = %q, want %q", indent, tc.expect)
			}
		})
	}
}

func TestDefaultIndentIsTwoSpaces(t *testing.T) {
	// A compact file gives no signal, so the written file should match
	// Claude Code's own style.
	path := write(t, `{"editorMode":"vim"}`)
	a := newForPath(path)
	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if got := read(t, path); !strings.Contains(got, "\n  \"") {
		t.Errorf("the file is not indented with two spaces:\n%s", got)
	}
}

func TestFilePermissionsArePreserved(t *testing.T) {
	// CreateTemp makes a 0600 file, which would silently tighten a config
	// the user left group-readable.
	path := write(t, `{"editorMode":"vim"}`)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	a := newForPath(path)
	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Errorf("permissions = %o, want 644", got)
	}
}

func TestNoTempFilesLeftBehind(t *testing.T) {
	// A crash-safe write must still clean up after itself.
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	a := newForPath(path)
	if err := a.Install(testBin); err != nil {
		t.Fatalf("Install: %v", err)
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

func TestDetectIndent(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"two spaces", "{\n  \"a\": 1\n}", "  "},
		{"four spaces", "{\n    \"a\": 1\n}", "    "},
		{"tab", "{\n\t\"a\": 1\n}", "\t"},
		{"compact falls back", `{"a":1}`, defaultIndent},
		{"empty falls back", "", defaultIndent},
		{"blank lines are skipped", "{\n\n   \"a\": 1\n}", "   "},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := detectIndent([]byte(tc.in)); got != tc.want {
				t.Errorf("detectIndent = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoadSettingsNullDocument(t *testing.T) {
	// A file holding "null" decodes to a nil map, which must not panic on
	// the first write.
	path := write(t, "null")
	s, err := loadSettings(path)
	if err != nil {
		t.Fatalf("loadSettings: %v", err)
	}
	if s.root == nil {
		t.Fatal("root is nil")
	}
	if _, err := s.hooksObject(true); err != nil {
		t.Fatalf("hooksObject: %v", err)
	}
}

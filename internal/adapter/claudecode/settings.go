package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// marker identifies an entry as ours.
//
// Every command this adapter writes contains it, which is how Uninstall
// finds its own work in a file full of other tools' hooks. It lives in
// the command string rather than a sibling JSON key because Claude Code
// rewrites this file itself and would drop a field it does not know,
// leaving entries we could no longer clean up.
//
// It is a fixed token rather than the invocation itself. The binary path
// is quoted and varies per install, so any marker containing it would
// stop matching the moment the path changed.
const marker = "stoplight-hook"

// markerPrefix is the exact opening of every command this adapter writes:
// the shell's no-op builtin, the marker, and the separator.
//
// Ownership is decided on this prefix rather than on the marker appearing
// anywhere in the command. A substring test claims any hook that merely
// mentions the token, so a user command such as
// `echo my own stoplight-hook wrapper >> ~/log` would be overwritten by
// Install and deleted by Uninstall. Anchoring at the start cannot collide
// by accident: a command that begins with `: stoplight-hook; ` is one we
// generated.
//
// TestGeneratedCommandStartsWithTheMarkerPrefix holds hookCommand to this
// shape, because a generated command that stopped matching would be left
// behind by every future uninstall.
const markerPrefix = ": " + marker + "; "

// settings is a Claude Code settings.json decoded far enough to edit the
// hooks and no further.
//
// The document is held as an ordered object so that keys this adapter has
// never heard of survive the round trip in their original positions. A
// typed struct would drop any field it did not declare, and a plain map
// would reorder every line; against a real user config either one is
// destructive.
type settings struct {
	// root is the whole document, including the hooks key.
	root *object
	// indent is the indentation detected in the original file, so a
	// rewrite matches the surrounding formatting.
	indent string
	// existed records whether the file was on disk at load time.
	existed bool
	// createdByUs records that loadSettings found nothing at the path, so
	// any file now there is one this adapter wrote. Uninstall removes only
	// such a file. A file that was already on disk is left in place even
	// when we empty it, because deleting a file we did not create destroys
	// whatever the user or another tool intended by having it there.
	createdByUs bool
	// createdHooks records that the "hooks" key exists only because this
	// adapter added it, so emptying it may remove it again. A hooks object
	// the user wrote stays, even when nothing is left inside it.
	createdHooks bool
	// trailingNewline records whether the original ended with a newline,
	// so the rewrite does not add or drop one.
	trailingNewline bool
	// crlf records that the original used CRLF line endings, so a rewrite
	// does not silently convert the whole file to LF.
	crlf bool
}

// defaultIndent matches the two-space style Claude Code writes.
const defaultIndent = "  "

// loadSettings reads and decodes path.
//
// A missing or empty file yields an empty document, because installing
// into a config that does not exist yet is normal. Invalid JSON is an
// error: this adapter refuses to guess at a file it cannot parse, since
// overwriting it would destroy settings the user wrote by hand.
func loadSettings(path string) (*settings, error) {
	s := &settings{root: newObject(), indent: defaultIndent, trailingNewline: true}

	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			// Nothing here, so a file at this path afterwards is ours.
			s.createdByUs = true
			return s, nil
		}
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	s.existed = true

	// An empty or whitespace-only file is treated as an empty object.
	// Some tools create the file before writing anything to it. It still
	// counts as a file we did not create: something made it deliberately.
	if len(bytes.TrimSpace(raw)) == 0 {
		s.crlf = bytes.Contains(raw, []byte("\r\n"))
		return s, nil
	}

	root, err := decodeObject(raw)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w (fix or move the file, it has not been changed)", path, err)
	}
	s.root = root
	s.indent = detectIndent(raw)
	s.trailingNewline = bytes.HasSuffix(raw, []byte("\n"))
	s.crlf = bytes.Contains(raw, []byte("\r\n"))
	return s, nil
}

// detectIndent finds the indentation of the first indented line, so a
// rewritten file keeps the style the user or the tool was using.
func detectIndent(raw []byte) string {
	for line := range strings.SplitSeq(string(raw), "\n") {
		// A CRLF file leaves a carriage return on the end of every line,
		// which would otherwise be measured as part of the indentation.
		line = strings.TrimSuffix(line, "\r")
		trimmed := strings.TrimLeft(line, " \t")
		if trimmed == "" || len(trimmed) == len(line) {
			continue
		}
		switch indent := line[:len(line)-len(trimmed)]; indent[0] {
		case '\t':
			return "\t"
		case ' ':
			return indent
		}
	}
	return defaultIndent
}

// save writes the document back to path atomically.
//
// The temp file is created in the same directory so that the rename is
// within one filesystem and therefore atomic. A reader either sees the
// old file or the new one, never a half-written config.
//
// Formatting note. The document is re-indented as a whole with
// json.Indent, so a hand-compacted or irregularly formatted file comes
// back uniformly indented and the diff covers the whole file. Only the
// indent width, the trailing newline and the line ending are carried
// over from the original. Preserving byte-level layout would mean editing
// the raw text in place rather than decoding and re-encoding, which is a
// much larger change for a file that Claude Code itself rewrites in this
// same uniform style. This behaviour is documented in CONTRIBUTING.md.
func (s *settings) save(path string) error {
	// Resolve a symlink and write to the file it points at. Renaming onto
	// the link itself would replace it with a regular file, which breaks a
	// settings.json symlinked into a dotfiles repo: the repo copy stays
	// behind and silently stops taking effect.
	target, err := resolveTarget(path)
	if err != nil {
		return err
	}
	if err := checkWritable(target); err != nil {
		return err
	}

	compact, err := s.root.MarshalJSON()
	if err != nil {
		return fmt.Errorf("encode %s: %w", path, err)
	}

	var buf bytes.Buffer
	if err := json.Indent(&buf, compact, "", s.indent); err != nil {
		return fmt.Errorf("format %s: %w", path, err)
	}
	if s.trailingNewline {
		buf.WriteByte('\n')
	}

	out := buf.Bytes()
	if s.crlf {
		out = toCRLF(out)
	}

	path = target
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dir, err)
	}

	tmp, err := os.CreateTemp(dir, ".stoplight-settings-*.json")
	if err != nil {
		return fmt.Errorf("create a temporary file in %s: %w", dir, err)
	}
	tmpName := tmp.Name()
	// Best effort cleanup. Once the rename succeeds there is nothing at
	// this path and the remove fails harmlessly.
	defer os.Remove(tmpName)

	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		return fmt.Errorf("write %s: %w", tmpName, err)
	}
	// Flush to disk before the rename. Without this a crash can leave the
	// renamed file present but empty.
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync %s: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close %s: %w", tmpName, err)
	}

	// Carry over the original permissions. CreateTemp makes the file 0600,
	// which would silently tighten a config the user had left readable.
	perm := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		perm = info.Mode().Perm()
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		return fmt.Errorf("set permissions on %s: %w", tmpName, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	return nil
}

// resolveTarget follows a symlink to the file that should actually be
// written.
//
// A path that does not exist yet is returned unchanged: EvalSymlinks
// reports an error for a missing file, but creating one is normal here.
// A broken symlink resolves to the path it names, so installing through
// it creates the intended target rather than clobbering the link.
func resolveTarget(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err == nil {
		return resolved, nil
	}
	if !os.IsNotExist(err) {
		return "", fmt.Errorf("resolve %s: %w", path, err)
	}

	// The path itself is missing, or it is a link pointing at something
	// missing. Follow one level of link so a broken link is written
	// through rather than replaced.
	info, lerr := os.Lstat(path)
	if lerr != nil || info.Mode()&os.ModeSymlink == 0 {
		return path, nil
	}
	dest, lerr := os.Readlink(path)
	if lerr != nil {
		return "", fmt.Errorf("read the link %s: %w", path, lerr)
	}
	if !filepath.IsAbs(dest) {
		dest = filepath.Join(filepath.Dir(path), dest)
	}
	return dest, nil
}

// checkWritable refuses to modify a file the user has marked read-only.
//
// The atomic rename needs write permission on the directory, not on the
// file, so a 0444 settings.json would otherwise be replaced without a
// word. A chmod is a deliberate instruction and this package honours it
// rather than working around it.
func checkWritable(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		// A file that does not exist yet is not read-only. Any real
		// problem surfaces when the write is attempted.
		return nil
	}
	if info.Mode().Perm()&0o200 == 0 {
		return fmt.Errorf("%s is read-only (mode %04o): change the permissions or move the file, it has not been changed", path, info.Mode().Perm())
	}
	return nil
}

// toCRLF converts LF line endings to CRLF, so a file that arrived with
// Windows line endings does not come back with Unix ones. The encoder
// only ever emits bare LF, so there are no CRLFs to double up.
func toCRLF(b []byte) []byte {
	return bytes.ReplaceAll(b, []byte("\n"), []byte("\r\n"))
}

// hooksObject returns the top-level hooks object, creating it when create
// is true. A hooks key holding something other than an object is reported
// as an error rather than replaced.
func (s *settings) hooksObject(create bool) (*object, error) {
	v, ok := s.root.Get("hooks")
	if !ok || v == nil {
		if !create {
			return nil, nil
		}
		o := newObject()
		s.root.Set("hooks", o)
		// The key was not there a moment ago, so it is ours.
		s.createdHooks = true
		return o, nil
	}
	o, ok := v.(*object)
	if !ok {
		return nil, fmt.Errorf("the \"hooks\" key is %s, not an object", jsonKind(v))
	}
	return o, nil
}

// jsonKind names a decoded value for an error message, in JSON terms
// rather than Go ones.
func jsonKind(v any) string {
	switch v.(type) {
	case *object:
		return "an object"
	case []any:
		return "an array"
	case string:
		return "a string"
	case json.Number:
		return "a number"
	case bool:
		return "a boolean"
	case nil:
		return "null"
	}
	return fmt.Sprintf("%T", v)
}

// groupsFor returns the matcher groups registered for one hook event.
// A missing event yields a nil slice, so callers can append.
func groupsFor(hooks *object, event string) ([]any, error) {
	v, ok := hooks.Get(event)
	if !ok || v == nil {
		return nil, nil
	}
	groups, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("the %q hook is %s, not an array", event, jsonKind(v))
	}
	return groups, nil
}

// isOurs reports whether a single hook entry is one this adapter wrote.
//
// The test is markerPrefix at the start of the command, not the marker
// anywhere inside it. A user hook that mentions the token in an echo or a
// comment belongs to the user, and claiming it would let Install overwrite
// it and Uninstall delete it.
func isOurs(entry any) bool {
	o, ok := entry.(*object)
	if !ok {
		return false
	}
	v, ok := o.Get("command")
	if !ok {
		return false
	}
	cmd, ok := v.(string)
	return ok && strings.HasPrefix(cmd, markerPrefix)
}

// pruneGroup removes our entries from one matcher group and reports
// whether the group still holds anything.
//
// A group emptied of our hooks is dropped by the caller, but only if we
// were the ones who emptied it: a group that arrived empty is left alone,
// because it is not ours to tidy.
func pruneGroup(group *object) (changed bool, empty bool) {
	v, ok := group.Get("hooks")
	if !ok {
		return false, false
	}
	entries, ok := v.([]any)
	if !ok {
		return false, false
	}
	kept := make([]any, 0, len(entries))
	for _, e := range entries {
		if isOurs(e) {
			changed = true
			continue
		}
		kept = append(kept, e)
	}
	if !changed {
		return false, false
	}
	group.Set("hooks", kept)
	return true, len(kept) == 0
}

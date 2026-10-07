// Package claudecode adapts Claude Code to the Stoplight protocol.
//
// Claude Code emits hooks on session events, so the adapter is a mapping
// table and an installer. This package is the only place in the project
// where the word "hook" or a Claude Code event name appears; everything
// below it sees the semantic events of RFC 1 section 3.
package claudecode

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/cidekar/stoplight/internal/adapter"
)

func init() {
	adapter.Register(New())
}

// hookMapping is one row of the table in RFC 1 section 10.1: a Claude
// Code hook name and the protocol event it means.
type hookMapping struct {
	// Hook is the Claude Code event name, the key under "hooks".
	Hook string
	// Event is the RFC 1 event posted to the relay.
	Event string
	// Matcher narrows a hook to the sub-events it should fire on, written
	// as the group's "matcher" key. Empty means every occurrence, which is
	// correct only for a hook whose every occurrence means the same event.
	//
	// Claude Code tests the matcher against a different field per hook:
	// a notification type for Notification, a session-start reason for
	// SessionStart, a tool name for PreToolUse. The value is a regexp.
	Matcher string
}

// mappings is the vendor boundary. Adding a row here is the only change
// needed to report another Claude Code event, and no other package is
// affected.
//
// Ordered as in the RFC so a written file reads like the spec.
//
// The matchers are not decoration: without them two of these hooks report
// a colour the session is not in.
//
//   - SessionStart fires on five reasons, not one. Only "startup" means a
//     session that is idle and waiting for its first prompt. "resume",
//     "clear", "fork" and above all "compact" fire mid-session, and an
//     auto-compaction drops a working session to idle until the next Stop.
//     Matching "startup" keeps `idle` to the one reason that is idle.
//
//   - Notification fires on every notification type. "permission_prompt"
//     is the one that means "waiting for you". "idle_prompt" fires about a
//     minute after a turn ends and would turn a green session red; and
//     "auth_success" flashes at login. Matching "permission_prompt" keeps
//     `blocked` to a session that is really blocked.
//
//   - PreToolUse has no colour of its own. It exists only to clear
//     `blocked`: when the user approves a permission prompt the agent runs
//     the tool, and the next thing Claude Code emits is this hook, long
//     before Stop. Without it an approved prompt stays red for the rest of
//     the turn, because nothing between Notification and Stop sends an
//     event. `started` returns the session to yellow, which the state
//     machine accepts from blocked (color.go, State.Next). It carries no
//     matcher because it must fire for every tool, and an absent matcher
//     means every tool. The poll side agrees: a running session reports
//     "working", which maps to `started` too (poll.go).
var mappings = []hookMapping{
	{Hook: "SessionStart", Event: "idle", Matcher: "startup"},
	{Hook: "UserPromptSubmit", Event: "started"},
	{Hook: "PreToolUse", Event: "started"},
	{Hook: "Notification", Event: "blocked", Matcher: "permission_prompt"},
	{Hook: "Stop", Event: "finished"},
	{Hook: "SessionEnd", Event: "ended"},
}

// Adapter installs Stoplight's hooks into Claude Code's settings.json.
type Adapter struct {
	// path is the settings file to edit. Tests point it at a temporary
	// directory; the zero value resolves to ~/.claude/settings.json.
	path string
	// createdFile records that this adapter's own Install brought the
	// settings file into being, which is the only thing that entitles
	// Uninstall to delete it again.
	//
	// The flag lives for as long as the process does. `stoplight install`
	// and `stoplight uninstall` are separate runs, so an uninstall from
	// the command line always finds a file it cannot prove it created and
	// leaves it in place, holding "{}". That is the intended bias: an
	// empty file the user can delete beats deleting a file that was not
	// ours, which no one can undo.
	createdFile bool
	// createdHooks records that this adapter's own Install added the
	// top-level "hooks" key, which is likewise the only thing that lets
	// Uninstall remove it again. Same lifetime and same bias as
	// createdFile.
	createdHooks bool
	// createdEvents holds the event keys this adapter's own Install added
	// to the hooks object. Only these may be deleted at uninstall.
	//
	// An event the user left as an empty array is the case this exists
	// for: Install appends to it, so after pruning it looks exactly like
	// an event we introduced and emptied. Only a record taken before the
	// edit can tell the two apart.
	createdEvents map[string]bool
}

// New returns an adapter that edits the user's ~/.claude/settings.json.
func New() *Adapter { return &Adapter{} }

// newForPath returns an adapter bound to a specific settings file. It
// exists so tests never touch the real config.
func newForPath(path string) *Adapter { return &Adapter{path: path} }

// Name implements adapter.Adapter.
func (a *Adapter) Name() string { return "claude-code" }

// settingsPath resolves the settings file this adapter edits.
func (a *Adapter) settingsPath() (string, error) {
	if a.path != "" {
		return a.path, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("find the home directory: %w", err)
	}
	return filepath.Join(home, ".claude", "settings.json"), nil
}

// hookCommand builds the shell command for one event.
//
// The session id and working directory come from the JSON payload Claude
// Code writes to the hook's stdin. There is no CLAUDE_SESSION_ID
// environment variable, so reading stdin is the only way to tell one
// session from another; without it every session would share an id and
// the relay would track them as one.
//
// Parsing is done with sed rather than a JSON tool because a hook runs on
// every prompt and must not depend on jq being installed. The payload is
// machine-generated and the session id is an opaque token, so the field
// is safe to pull out with a pattern.
//
// The command opens with ": marker;". The colon is the shell's no-op
// builtin, which ignores its arguments, so the marker costs nothing at
// runtime and gives Uninstall a stable string to recognise the entry by.
//
// The trailing "|| true" is the protocol's rule from RFC 1 section 6: a
// status light must never break its caller.
func hookCommand(binPath, event string) string {
	return fmt.Sprintf(
		`: %s; `+
			`payload=$(cat); `+
			`sid=$(printf '%%s' "$payload" | sed -n 's/.*"session_id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'); `+
			`dir=$(printf '%%s' "$payload" | sed -n 's/.*"cwd"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'); `+
			`%s notify %s --session-id "${sid:-$PPID}" --cwd "${dir:-$PWD}" `+
			`--provider %s >/dev/null 2>&1 || true`,
		marker, shellQuote(binPath), event, ProviderName,
	)
}

// shellQuote wraps a path in single quotes so a space or a shell
// metacharacter in an install location cannot change the command.
func shellQuote(s string) string {
	var out []rune
	out = append(out, '\'')
	for _, r := range s {
		if r == '\'' {
			// Close the quote, emit an escaped quote, reopen.
			out = append(out, '\'', '\\', '\'', '\'')
			continue
		}
		out = append(out, r)
	}
	out = append(out, '\'')
	return string(out)
}

// Install writes a hook entry for every row of the mapping table.
//
// It is idempotent. An existing Stoplight entry for an event is rewritten
// in place, which also refreshes the binary path after the binary moves,
// and no duplicate is added. Every other key and every other tool's hooks
// are preserved.
func (a *Adapter) Install(binPath string) error {
	path, err := a.settingsPath()
	if err != nil {
		return err
	}
	s, err := loadSettings(path)
	if err != nil {
		return err
	}
	hooks, err := s.hooksObject(true)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	// Remember only a first creation. A later install into a file that by
	// then exists must not clear the record from the one that made it.
	if s.createdByUs {
		a.createdFile = true
	}
	if s.createdHooks {
		a.createdHooks = true
	}

	// Note which event keys are absent before anything is written. After
	// the append an event the user left empty is indistinguishable from
	// one we created, so this has to be captured first.
	if a.createdEvents == nil {
		a.createdEvents = map[string]bool{}
	}
	for _, m := range mappings {
		if _, present := hooks.Get(m.Hook); !present {
			a.createdEvents[m.Hook] = true
		}
	}

	for _, m := range mappings {
		groups, err := groupsFor(hooks, m.Hook)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}

		entry := newObject()
		entry.Set("type", "command")
		entry.Set("command", hookCommand(binPath, m.Event))

		// Remove every entry of ours, then put exactly one back at the
		// first place one was found. Overwriting each match in place would
		// leave a duplicate for every duplicate already present, and an
		// interrupted install or a hand-merged config can leave two. Claude
		// Code fires each entry, so a duplicate is a hook that runs twice.
		replaced := false
		for _, g := range groups {
			group, ok := g.(*object)
			if !ok {
				continue
			}
			v, ok := group.Get("hooks")
			if !ok {
				continue
			}
			entries, ok := v.([]any)
			if !ok {
				continue
			}
			kept := make([]any, 0, len(entries))
			for _, e := range entries {
				if !isOurs(e) {
					kept = append(kept, e)
					continue
				}
				// Keep the first one's position, so the file's existing
				// ordering survives, and drop the rest.
				if !replaced {
					kept = append(kept, entry)
					replaced = true
				}
			}
			group.Set("hooks", kept)
		}
		if replaced {
			// A group we emptied of duplicates may now hold nothing. Drop
			// it if it was only ever a container for our entry, and keep it
			// if it carries a matcher or another tool's hooks.
			hooks.Set(m.Hook, dropEmptyOwnGroups(groups))
			continue
		}

		// No entry of ours yet. Append a group of our own rather than
		// joining another tool's group, so uninstall can remove it
		// cleanly.
		//
		// A mapping with a matcher writes it as the group's "matcher" key,
		// so the hook fires only on the sub-event this event means.
		// UserPromptSubmit, Stop and SessionEnd carry no matcher: every
		// occurrence of each means the same event, so narrowing would only
		// drop a light the producer asked for.
		//
		// The matcher is set before "hooks" so a fresh group reads
		// matcher-then-hooks, the order Claude Code writes its own.
		group := newObject()
		if m.Matcher != "" {
			group.Set("matcher", m.Matcher)
		}
		group.Set("hooks", []any{entry})
		hooks.Set(m.Hook, append(groups, group))
	}

	return s.save(path)
}

// isBareGroup reports whether a group holds nothing but the keys Install
// writes: "hooks", and optionally "matcher".
//
// This is the shape test that decides a group is a container we built
// rather than another tool's. Once our hooks were matcher-less, that was
// the single key "hooks"; now a matched hook adds a "matcher" sibling, so
// a group of ours may hold two keys. Any third key means a tool wrote
// fields we do not understand, and the group is not ours to touch.
//
// It says nothing about the entries inside: a caller that cares pairs this
// with isOurs on each entry.
func isBareGroup(group *object) bool {
	if _, ok := group.Get("hooks"); !ok {
		return false
	}
	for _, k := range group.Keys() {
		if k != "hooks" && k != "matcher" {
			return false
		}
	}
	return true
}

// dropEmptyOwnGroups removes groups that collapsing duplicates left with
// no entries at all.
//
// Only a bare group goes: one holding nothing but "hooks" and perhaps our
// "matcher". A group that carries any other key belongs to whoever wrote
// it, and an empty group that arrived that way is not ours to tidy.
func dropEmptyOwnGroups(groups []any) []any {
	kept := make([]any, 0, len(groups))
	for _, g := range groups {
		group, ok := g.(*object)
		if !ok {
			kept = append(kept, g)
			continue
		}
		if entries, ok := group.Get("hooks"); ok && isBareGroup(group) {
			if list, ok := entries.([]any); ok && len(list) == 0 {
				continue
			}
		}
		kept = append(kept, group)
	}
	return kept
}

// wasAddedByUs reports whether an event's groups are all bare containers
// for an entry of ours, which is the shape Install leaves behind when it
// introduces an event key.
//
// An empty slice is not ours: an event the user left as an empty array
// pre-existed the install, and its key must survive uninstall.
func wasAddedByUs(groups []any) bool {
	if len(groups) == 0 {
		return false
	}
	for _, g := range groups {
		group, ok := g.(*object)
		if !ok {
			return false
		}
		// Only "hooks" and our own "matcher" belong in a group we built.
		// Any other sibling key means someone else wrote it.
		if !isBareGroup(group) {
			return false
		}
		v, ok := group.Get("hooks")
		if !ok {
			return false
		}
		entries, ok := v.([]any)
		if !ok || len(entries) == 0 {
			return false
		}
		for _, e := range entries {
			if !isOurs(e) {
				return false
			}
		}
	}
	return true
}

// Uninstall removes every entry this adapter wrote and leaves the rest of
// the file as it was found.
//
// A settings file that does not exist is already in the desired state, so
// this is a no-op rather than an error. A file we created and are now
// emptying is removed, so uninstall does not leave a stub behind.
func (a *Adapter) Uninstall() error {
	path, err := a.settingsPath()
	if err != nil {
		return err
	}
	s, err := loadSettings(path)
	if err != nil {
		return err
	}
	if !s.existed {
		return nil
	}
	hooks, err := s.hooksObject(false)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if hooks == nil {
		return nil
	}

	// The hooks key is removed only when this adapter added it, on the
	// same reasoning as the file itself. A hooks object the user wrote
	// stays, even once our entries are out of it and nothing is left
	// inside: an empty "hooks": {} the user can see and delete is better
	// than silently dropping a key that was theirs.
	s.createdHooks = a.createdHooks

	changed := false
	for _, event := range hooks.Keys() {
		groups, err := groupsFor(hooks, event)
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}

		// Decide, before mutating, whether this event key may be deleted.
		//
		// Two things have to agree. Install must have recorded the key as
		// absent when it ran, which rules out an event the user left as an
		// empty array. And the groups still present must all be bare
		// containers for our entries, which rules out an event that has
		// since gained hooks from another tool. Anything else keeps its
		// key, emptied but present.
		ours := a.createdEvents[event] && wasAddedByUs(groups)

		kept := make([]any, 0, len(groups))
		for _, g := range groups {
			group, ok := g.(*object)
			if !ok {
				kept = append(kept, g)
				continue
			}
			touched, empty := pruneGroup(group)
			if touched {
				changed = true
			}
			// Drop a group only when we are the ones who emptied it, and
			// only when it holds nothing else worth keeping. A group that
			// carries a foreign key stays even once empty: isBareGroup
			// accepts our own "matcher" sibling but nothing else.
			if touched && empty && isBareGroup(group) {
				continue
			}
			kept = append(kept, group)
		}

		if len(kept) == 0 && ours {
			// The event has no groups left and we are the reason it was
			// there at all, so the key goes with it.
			hooks.Delete(event)
			continue
		}
		hooks.Set(event, kept)
	}

	if !changed {
		return nil
	}

	// Leave no empty scaffolding behind: a hooks object we emptied goes
	// away, but only when we were the ones who introduced it.
	if hooks.Len() == 0 && s.createdHooks {
		s.root.Delete("hooks")
	}

	// Delete the file only when this adapter created it in this same run.
	//
	// An empty document is not permission to unlink. A settings.json that
	// was already on disk, even one holding just "{}", was put there by
	// the user or by Claude Code, and deleting it destroys whatever that
	// was for. Such a file is written back as an empty object instead.
	//
	// a.createdFile is set only by an Install on this same adapter that
	// found nothing at the path. A separate `stoplight uninstall` run
	// starts with a fresh adapter, finds the file present, and therefore
	// leaves it in place holding "{}" rather than guessing at who created
	// it. Leaving an empty file is a cosmetic flaw the user can undo with
	// one `rm`; deleting the wrong file is not recoverable.
	if s.root.Len() == 0 && a.createdFile {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("remove %s: %w", path, err)
		}
		a.createdFile = false
		return nil
	}

	return s.save(path)
}

// Installed reports whether any of this adapter's entries are present.
func (a *Adapter) Installed() (bool, error) {
	path, err := a.settingsPath()
	if err != nil {
		return false, err
	}
	s, err := loadSettings(path)
	if err != nil {
		return false, err
	}
	if !s.existed {
		return false, nil
	}
	hooks, err := s.hooksObject(false)
	if err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	if hooks == nil {
		return false, nil
	}

	for _, event := range hooks.Keys() {
		groups, err := groupsFor(hooks, event)
		if err != nil {
			return false, fmt.Errorf("%s: %w", path, err)
		}
		for _, g := range groups {
			group, ok := g.(*object)
			if !ok {
				continue
			}
			v, ok := group.Get("hooks")
			if !ok {
				continue
			}
			entries, ok := v.([]any)
			if !ok {
				continue
			}
			if slices.ContainsFunc(entries, isOurs) {
				return true, nil
			}
		}
	}
	return false, nil
}

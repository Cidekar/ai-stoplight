package claudecode

import (
	"strings"
	"testing"

	"github.com/cidekar/stoplight/internal/stoplight"
)

// The tests here span the boundary the hook command crosses: this package
// generates a shell command, and the relay's event parser has to understand
// the event name inside it.
//
// TestMappingMatchesTheRFC pins the table against a literal copy of itself in
// the same file, which agrees with whatever is written next to it. It cannot
// catch an event name that no longer parses, because it never asks the parser.

// TestGeneratedEventsParse checks that every event this adapter emits is one
// stoplight.ParseEvent accepts.
//
// An event name that does not parse is silently dropped by the tracker, so
// the hook fires, the relay answers, and the light never changes. Nothing
// reports an error anywhere along that path, which is exactly why this has to
// be asserted rather than observed.
func TestGeneratedEventsParse(t *testing.T) {
	for _, m := range mappings {
		if _, ok := stoplight.ParseEvent(m.Event); !ok {
			t.Errorf("the %s hook emits event %q, which stoplight.ParseEvent rejects: "+
				"the relay would ignore it and the light would never change",
				m.Hook, m.Event)
		}
	}
}

// TestGeneratedEventsAppearInTheCommand checks the event actually reaches the
// generated command line, rather than only living in the mapping table.
func TestGeneratedEventsAppearInTheCommand(t *testing.T) {
	for _, m := range mappings {
		cmd := hookCommand("/usr/local/bin/stoplight", m.Event)
		if !strings.Contains(cmd, " notify "+m.Event+" ") {
			t.Errorf("the %s hook command does not invoke `notify %s`:\n%s",
				m.Hook, m.Event, cmd)
		}
	}
}

// TestGeneratedFlagsAreAccepted checks every flag the hook command emits
// against the set cmdNotify defines.
//
// notify exits zero on an unknown flag by design, so a flag this adapter
// emits and the command does not define fails completely silently: the hook
// succeeds, the report is never sent, and the light stays as it was. The
// wanted list is kept here rather than imported because package main cannot
// be imported by a test in this package; the assertion that keeps the two in
// step lives in package main, where the real flag set is reachable.
func TestGeneratedFlagsAreAccepted(t *testing.T) {
	// The flags cmdNotify defines. Mirrored in the boundary test in package
	// main, which checks this list against the real flag set.
	accepted := map[string]bool{
		"session-id": true,
		"cwd":        true,
		"label":      true,
		"provider":   true,
		"addr":       true,
	}

	for _, m := range mappings {
		cmd := hookCommand("/usr/local/bin/stoplight", m.Event)

		// Only the part after the binary is the invocation. Everything before
		// it is the marker and the shell that extracts the payload fields.
		_, invocation, ok := strings.Cut(cmd, " notify ")
		if !ok {
			t.Fatalf("the %s hook command does not invoke notify:\n%s", m.Hook, cmd)
		}

		for _, field := range strings.Fields(invocation) {
			if !strings.HasPrefix(field, "--") {
				continue
			}
			name := strings.TrimPrefix(field, "--")
			// A --flag=value form carries its value in the same token.
			name, _, _ = strings.Cut(name, "=")

			if !accepted[name] {
				t.Errorf("the %s hook command passes --%s, which notify does not define: "+
					"notify exits zero on an unknown flag, so the report would be "+
					"dropped without any error:\n%s", m.Hook, name, cmd)
			}
		}
	}
}

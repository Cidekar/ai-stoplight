// Package adapter connects a specific agent to the Stoplight protocol.
//
// An adapter knows two things: how its agent signals state, and how to
// install itself into that agent's configuration. Nothing below this
// boundary knows which tool an event came from, which is what keeps one
// vendor's vocabulary out of the core. See RFC 1 section 10.
package adapter

import "sort"

// Adapter installs and removes the hook entries that make one agent
// report to the relay.
//
// Install and Uninstall both edit a configuration file the user cares
// about. Both are therefore required to be idempotent and to leave
// entries they did not create exactly as they found them.
type Adapter interface {
	// Name is the stable identifier used on the command line, such as
	// "claude-code".
	Name() string

	// Install writes the agent's hook entries, each invoking binPath.
	// Running it twice must produce the same configuration.
	Install(binPath string) error

	// Uninstall removes only the entries this adapter added and
	// restores what it found.
	Uninstall() error

	// Installed reports whether this adapter's entries are present.
	Installed() (bool, error)
}

// registry holds every adapter compiled into the binary.
//
// Adapters register from init, which runs before main. The map is
// therefore written during single-threaded startup and only read after,
// so it needs no lock.
var registry = map[string]Adapter{}

// Register adds an adapter to the registry. Call it from a package init
// function.
//
// Registering the same name twice panics. Two adapters fighting over one
// config file is a mistake in the build, not a condition to recover from
// at runtime.
func Register(a Adapter) {
	name := a.Name()
	if _, dup := registry[name]; dup {
		panic("adapter: duplicate registration for " + name)
	}
	registry[name] = a
}

// All returns every registered adapter, ordered by name so that command
// output stays in the same order between runs.
func All() []Adapter {
	out := make([]Adapter, 0, len(registry))
	for _, a := range registry {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Get returns the adapter with the given name.
func Get(name string) (Adapter, bool) {
	a, ok := registry[name]
	return a, ok
}

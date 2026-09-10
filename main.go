// Command stoplight runs the relay that drives a desk light, and manages the
// adapters and background service that keep it running.
//
// This file is dispatch only. Each command parses its own flags and then hands
// off to a package under internal/. No behaviour lives in package main: the
// state machine, the service managers and the adapters all have to be testable
// without spawning a process, so none of them may live behind a func main.
package main

import (
	"os"

	// Adapters register themselves from init, so an adapter package that
	// nothing imports is not linked into the binary and never registers.
	//
	// DO NOT REMOVE. This import has no symbol reference on purpose, so a
	// tool or a reader looking for one concludes it is unused. It is not:
	// deleting it empties adapter.All(), and `stoplight install` then prints
	// "no supported agents found" and installs no hooks at all, while every
	// test of the adapter keeps passing, because those tests import the
	// package directly. That exact bug shipped once. adapter_wiring_test.go
	// fails if this line goes.
	//
	// The import is blank rather than a call to a constructor so that main
	// stays ignorant of which agents exist: naming the adapter's type here
	// would put a vendor's name above the boundary the adapter package is
	// there to hold. See internal/adapter/adapter.go and RFC 1 section 10.
	_ "github.com/cidekar/stoplight/internal/adapter/claudecode"
)

func main() {
	// The only os.Exit in the program. Every other path returns a code up to
	// here, so deferred closes and flushes actually run.
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

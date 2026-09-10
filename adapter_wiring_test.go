package main

import (
	"os/exec"
	"strings"
	"testing"

	"github.com/cidekar/stoplight/internal/adapter"
)

// This file guards one property: the adapters are linked into the binary that
// ships, not merely into the test binary.
//
// The bug it exists for shipped and survived every review. Adapters register
// from init, so a package that nothing imports never registers, and package
// main imported the registry but never any adapter. adapter.All() therefore
// returned nothing in the real binary: `stoplight install` printed "no
// supported agents found" and wrote no hooks at all, while every adapter test
// passed, because a test in internal/adapter/claudecode imports the adapter
// under test by definition and so always sees a populated registry.
//
// That asymmetry is the defect, and it is why the checks below are written the
// way they are. A test that imports an adapter to assert the adapter is
// registered proves only that Go imports work. Both tests here deliberately
// avoid importing any adapter package: one reads the registry that main's own
// imports produced, the other reads the link graph from outside the process.

// wantAdapters are the adapters that must reach the shipped binary.
//
// This list is the one place a new adapter has to be named. That is the point:
// adding an adapter package without wiring it into main is exactly the failure
// this file catches, so the list is maintained by hand on purpose.
var wantAdapters = []string{"claude-code"}

// TestAdaptersAreLinkedIntoTheBinary asserts the registry is populated in a
// binary built from package main.
//
// This file must never import an adapter package. A test file in package main
// shares main's import set, so a blank import here would register the adapter
// in the test binary and pass whether or not the real binary links it, which
// is precisely the hole the original bug fell through. The registry is read
// through internal/adapter alone, which holds the map but registers nothing.
func TestAdaptersAreLinkedIntoTheBinary(t *testing.T) {
	all := adapter.All()
	if len(all) == 0 {
		t.Fatal("adapter.All() is empty in package main: no adapter package is " +
			"imported, so no init ran and no adapter registered. " +
			"`stoplight install` reports \"no supported agents found\" and installs " +
			"nothing. Import the adapter packages from main.go.")
	}

	got := make(map[string]bool, len(all))
	for _, a := range all {
		got[a.Name()] = true
	}
	for _, name := range wantAdapters {
		if !got[name] {
			t.Errorf("adapter %q is not registered in package main; registered: %v",
				name, names(all))
		}
	}
}

// names renders the registered adapter names for a failure message.
func names(all []adapter.Adapter) []string {
	out := make([]string, 0, len(all))
	for _, a := range all {
		out = append(out, a.Name())
	}
	return out
}

// TestAdapterPackagesReachMain checks the same property from outside the
// process, against the real build rather than the test build.
//
// go list -deps reports the packages a plain `go build .` links. A test binary
// links strictly more than that, because it also links every test file's
// imports, so an in-process check can be satisfied by a test-only import while
// the shipped binary stays empty. This one cannot: it never loads the adapter
// at all, it asks the toolchain what main depends on.
//
// It is the assertion that would have failed on the original code even if a
// test file in this package had imported the adapter.
func TestAdapterPackagesReachMain(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain on PATH")
	}

	// -deps lists the full transitive import graph of package main, one path
	// per line. The command is run in the package directory, which is this
	// test's own directory, so no path juggling is needed.
	out, err := exec.Command("go", "list", "-deps", ".").Output()
	if err != nil {
		t.Fatalf("go list -deps .: %v", err)
	}

	deps := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			deps[line] = true
		}
	}

	const adapterPkgPrefix = "github.com/cidekar/stoplight/internal/adapter/"

	// Every adapter package under internal/adapter must be in main's graph.
	// Discovering them rather than listing them means a new adapter package is
	// covered the day it is added, with no second list to keep in step.
	listed, err := exec.Command("go", "list", "./internal/adapter/...").Output()
	if err != nil {
		t.Fatalf("go list ./internal/adapter/...: %v", err)
	}
	for _, pkg := range strings.Split(string(listed), "\n") {
		pkg = strings.TrimSpace(pkg)
		if pkg == "" || !strings.HasPrefix(pkg, adapterPkgPrefix) {
			continue
		}
		if !deps[pkg] {
			t.Errorf("%s is not in the dependency graph of package main, so its "+
				"init never runs and it never registers: the shipped binary has no "+
				"such adapter even though its tests pass. Import it from main.go.", pkg)
		}
	}
}

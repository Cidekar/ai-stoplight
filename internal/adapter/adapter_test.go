package adapter

import "testing"

// fake is a test double. It records calls so idempotency and dispatch can
// be asserted without touching a real config file.
type fake struct {
	name      string
	installed bool
	binPath   string
	installs  int
}

func (f *fake) Name() string { return f.name }

func (f *fake) Install(binPath string) error {
	f.installs++
	f.binPath = binPath
	f.installed = true
	return nil
}

func (f *fake) Uninstall() error {
	f.installed = false
	return nil
}

func (f *fake) Installed() (bool, error) { return f.installed, nil }

// withRegistry swaps in an empty registry for the duration of a test and
// restores the real one afterwards, so tests cannot see each other's
// registrations or the claudecode adapter's init.
func withRegistry(t *testing.T) {
	t.Helper()
	saved := registry
	registry = map[string]Adapter{}
	t.Cleanup(func() { registry = saved })
}

func TestRegisterAndGet(t *testing.T) {
	withRegistry(t)

	a := &fake{name: "alpha"}
	Register(a)

	got, ok := Get("alpha")
	if !ok {
		t.Fatal("Get did not find the registered adapter")
	}
	if got != a {
		t.Errorf("Get returned %v, want %v", got, a)
	}
}

func TestGetUnknown(t *testing.T) {
	withRegistry(t)

	if _, ok := Get("nope"); ok {
		t.Error("Get found an adapter that was never registered")
	}
}

func TestAllIsSortedByName(t *testing.T) {
	// Command output must not shuffle between runs, and map iteration
	// order in Go is deliberately random.
	withRegistry(t)

	Register(&fake{name: "zulu"})
	Register(&fake{name: "alpha"})
	Register(&fake{name: "mike"})

	all := All()
	if len(all) != 3 {
		t.Fatalf("All returned %d adapters, want 3", len(all))
	}
	want := []string{"alpha", "mike", "zulu"}
	for i, name := range want {
		if all[i].Name() != name {
			t.Errorf("All()[%d] = %q, want %q", i, all[i].Name(), name)
		}
	}
}

func TestAllOnAnEmptyRegistry(t *testing.T) {
	withRegistry(t)

	if got := All(); len(got) != 0 {
		t.Errorf("All returned %d adapters, want 0", len(got))
	}
}

func TestRegisterDuplicatePanics(t *testing.T) {
	// Two adapters editing one config file is a build-time mistake, so it
	// should fail loudly rather than silently drop one.
	withRegistry(t)

	Register(&fake{name: "alpha"})

	defer func() {
		if recover() == nil {
			t.Error("registering a duplicate name did not panic")
		}
	}()
	Register(&fake{name: "alpha"})
}

func TestAdapterRoundTrip(t *testing.T) {
	withRegistry(t)

	a := &fake{name: "alpha"}
	Register(a)

	got, _ := Get("alpha")
	if installed, _ := got.Installed(); installed {
		t.Error("the adapter reported installed before Install")
	}
	if err := got.Install("/usr/local/bin/stoplight"); err != nil {
		t.Fatalf("Install: %v", err)
	}
	if installed, _ := got.Installed(); !installed {
		t.Error("the adapter did not report installed after Install")
	}
	if a.binPath != "/usr/local/bin/stoplight" {
		t.Errorf("binPath = %q, want /usr/local/bin/stoplight", a.binPath)
	}
	if err := got.Uninstall(); err != nil {
		t.Fatalf("Uninstall: %v", err)
	}
	if installed, _ := got.Installed(); installed {
		t.Error("the adapter still reported installed after Uninstall")
	}
}

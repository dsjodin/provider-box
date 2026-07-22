package deploy

import (
	"context"
	"path/filepath"
	"slices"
	"testing"

	"github.com/dsjodin/provider-box/services/control-plane/internal/envfile"
)

type fakeService struct {
	name string
	deps []string
}

func (f fakeService) Name() string                          { return f.name }
func (f fakeService) Deps() []string                        { return f.deps }
func (f fakeService) Deploy(context.Context, *RunCtx) error { return nil }
func (f fakeService) Remove(context.Context, *RunCtx) error { return nil }

func testEngine(t *testing.T) *Engine {
	t.Helper()
	e := NewEngine(envfile.Store{}, &StateStore{Path: filepath.Join(t.TempDir(), "state.json")}, nil)
	e.Register(fakeService{name: "ca"})
	e.Register(fakeService{name: "technitium", deps: []string{"ca"}})
	e.Register(fakeService{name: "netbox", deps: []string{"ca"}})
	e.Register(fakeService{name: "dns-sync", deps: []string{"netbox", "technitium"}})
	return e
}

func TestResolveExpandsDeps(t *testing.T) {
	e := testEngine(t)
	got, err := e.Resolve([]string{"dns-sync"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ca", "technitium", "netbox", "dns-sync"}
	if !slices.Equal(got, want) {
		t.Errorf("Resolve = %v, want %v", got, want)
	}
}

func TestSkipDeployedDeps(t *testing.T) {
	e := testEngine(t)

	// Nothing deployed yet: selecting technitium pulls in ca.
	ordered, _ := e.Resolve([]string{"technitium"})
	got := e.skipDeployedDeps([]string{"technitium"}, ordered)
	if !slices.Equal(got, []string{"ca", "technitium"}) {
		t.Errorf("fresh host: %v, want [ca technitium]", got)
	}

	// ca deployed ok: selecting technitium runs only technitium.
	if err := e.State.Record("ca", "deploy", "ok"); err != nil {
		t.Fatal(err)
	}
	got = e.skipDeployedDeps([]string{"technitium"}, ordered)
	if !slices.Equal(got, []string{"technitium"}) {
		t.Errorf("ca deployed: %v, want [technitium]", got)
	}

	// A failed dependency is re-run.
	if err := e.State.Record("ca", "deploy", "failed: boom"); err != nil {
		t.Fatal(err)
	}
	got = e.skipDeployedDeps([]string{"technitium"}, ordered)
	if !slices.Equal(got, []string{"ca", "technitium"}) {
		t.Errorf("ca failed: %v, want [ca technitium]", got)
	}

	// Explicit selection always runs, deployed or not.
	if err := e.State.Record("ca", "deploy", "ok"); err != nil {
		t.Fatal(err)
	}
	ordered, _ = e.Resolve([]string{"ca", "technitium"})
	got = e.skipDeployedDeps([]string{"ca", "technitium"}, ordered)
	if !slices.Equal(got, []string{"ca", "technitium"}) {
		t.Errorf("explicit ca: %v, want [ca technitium]", got)
	}

	// "all" never skips.
	ordered, _ = e.Resolve([]string{"all"})
	got = e.skipDeployedDeps([]string{"all"}, ordered)
	if len(got) != len(ordered) {
		t.Errorf("all: %v, want everything (%v)", got, ordered)
	}
}

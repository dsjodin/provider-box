// Package deploy is the control plane's deploy engine: a static registry of
// services with explicit dependencies, executed sequentially in dependency
// order, streaming progress events to SSE subscribers. Each deployer is a Go
// port of its bootstrap/*.sh module with identical data-preservation
// semantics.
package deploy

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/dsjodin/provider-box/services/control-plane/internal/envfile"
)

// Service is one deployable unit. Deploy and Remove must be idempotent.
type Service interface {
	Name() string
	Deps() []string
	Deploy(ctx context.Context, rc *RunCtx) error
	Remove(ctx context.Context, rc *RunCtx) error
}

// RunCtx carries everything a deployer needs for one run.
type RunCtx struct {
	Env map[string]string // parsed provider-box.env plus derived fields
	Log func(format string, args ...any)
	eng *Engine
	svc string
}

// Workdir returns ${WORKDIR}/<sub>.
func (rc *RunCtx) Workdir(sub string) string {
	return filepath.Join(rc.Env["WORKDIR"], sub)
}

// Compose returns a compose runner rooted at ${WORKDIR}/<sub> whose output
// streams into the deploy log.
func (rc *RunCtx) Compose(sub string) Compose {
	return Compose{Dir: rc.Workdir(sub), Out: func(line string) { rc.Log("%s", line) }}
}

// Engine owns the registry, the single-flight deploy loop, and state.
type Engine struct {
	Store  envfile.Store
	State  *StateStore
	Logger *slog.Logger

	services []Service // registration order IS the --all deploy order

	mu      sync.Mutex
	nextID  int
	current *Run         // nil when idle
	runs    map[int]*Run // by id, for SSE replay
}

func NewEngine(store envfile.Store, state *StateStore, logger *slog.Logger) *Engine {
	return &Engine{Store: store, State: state, Logger: logger, runs: map[int]*Run{}}
}

// Register appends a service; call in dependency order (deps first).
func (e *Engine) Register(s Service) {
	e.services = append(e.services, s)
}

// Services returns the registry in deploy order.
func (e *Engine) Services() []Service {
	return e.services
}

func (e *Engine) find(name string) Service {
	for _, s := range e.services {
		if s.Name() == name {
			return s
		}
	}
	return nil
}

// Resolve expands the selection with transitive dependencies and returns it
// in registry (dependency) order. "all" selects everything.
func (e *Engine) Resolve(selection []string) ([]string, error) {
	want := map[string]bool{}
	if slices.Contains(selection, "all") {
		for _, s := range e.services {
			want[s.Name()] = true
		}
	} else {
		var addDeps func(name string) error
		addDeps = func(name string) error {
			s := e.find(name)
			if s == nil {
				return fmt.Errorf("unknown service: %s", name)
			}
			if want[name] {
				return nil
			}
			want[name] = true
			for _, dep := range s.Deps() {
				if err := addDeps(dep); err != nil {
					return err
				}
			}
			return nil
		}
		for _, name := range selection {
			if err := addDeps(name); err != nil {
				return nil, err
			}
		}
	}

	var ordered []string
	for _, s := range e.services {
		if want[s.Name()] {
			ordered = append(ordered, s.Name())
		}
	}
	return ordered, nil
}

// Start validates the request and launches a deploy (or removal) in the
// background. It returns the run ID, or an error when a deploy is already in
// flight (single-flight) or validation fails.
//
// Explicitly selected services always run (idempotent redeploy). A dependency
// that was only pulled in by expansion is skipped when its last deploy
// succeeded - selecting technitium after ca is already up deploys just
// technitium. If the recorded state is stale (the dependency is actually
// down), the dependent deployer's own readiness gate fails with a pointed
// "deploy <dep> first" error rather than silently misbehaving.
func (e *Engine) Start(selection []string, remove bool) (int, error) {
	ordered, err := e.Resolve(selection)
	if err != nil {
		return 0, err
	}
	var skipped []string
	if remove {
		slices.Reverse(ordered)
	} else {
		kept := e.skipDeployedDeps(selection, ordered)
		for _, name := range ordered {
			if !slices.Contains(kept, name) {
				skipped = append(skipped, name)
			}
		}
		ordered = kept
	}

	content, ok, err := e.Store.Load()
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("no configuration uploaded yet; save one in the config wizard first")
	}
	env := envfile.Parse(content)
	if example, err := e.Store.Example(); err == nil {
		if missing := envfile.MissingFromExample(content, example); len(missing) > 0 {
			return 0, fmt.Errorf("config is outdated; missing variables: %v", missing)
		}
	}
	if !remove {
		if issues := envfile.Validate(env, ordered); len(issues) > 0 {
			return 0, fmt.Errorf("config validation failed: %v", issues)
		}
	}
	ipv4, network, err := envfile.DeriveHostIP(env["HOST_IP"])
	if err != nil {
		return 0, err
	}
	env["HOST_IPV4"] = ipv4
	env["HOST_NETWORK_CIDR"] = network

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.current != nil && !e.current.Done() {
		return 0, ErrBusy
	}
	e.nextID++
	run := newRun(e.nextID, ordered, remove)
	run.Skipped = skipped
	e.current = run
	e.runs[run.ID] = run

	go e.execute(run, env)
	return run.ID, nil
}

var ErrBusy = fmt.Errorf("a deploy is already running")

// skipDeployedDeps drops services that were added only by dependency
// expansion and whose last recorded deploy succeeded. "all" selects
// everything explicitly, so nothing is skipped there.
func (e *Engine) skipDeployedDeps(selection, ordered []string) []string {
	if slices.Contains(selection, "all") || e.State == nil {
		return ordered
	}
	state := e.State.Snapshot()
	var out []string
	for _, name := range ordered {
		if slices.Contains(selection, name) {
			out = append(out, name)
			continue
		}
		st, ok := state.Services[name]
		if ok && st.LastAction == "deploy" && st.Result == "ok" {
			continue // dependency already deployed; its consumer's gate re-verifies
		}
		out = append(out, name)
	}
	return out
}

// Run returns a run by ID for SSE subscription.
func (e *Engine) Run(id int) *Run {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runs[id]
}

func (e *Engine) execute(run *Run, env map[string]string) {
	ctx := context.Background()
	verb := "deploy"
	if run.Remove {
		verb = "remove"
	}
	if len(run.Skipped) > 0 {
		run.emit(Event{Type: "log", Line: fmt.Sprintf("Skipping already-deployed dependencies: %s (tick them explicitly to redeploy).", strings.Join(run.Skipped, ", "))})
	}

	failed := false
	for _, name := range run.Services {
		svc := e.find(name)
		run.emit(Event{Type: "step-start", Service: name})
		rc := &RunCtx{
			Env: env,
			Log: func(format string, args ...any) {
				run.emit(Event{Type: "log", Service: name, Line: fmt.Sprintf(format, args...)})
			},
			eng: e,
			svc: name,
		}
		var err error
		if run.Remove {
			err = svc.Remove(ctx, rc)
		} else {
			err = svc.Deploy(ctx, rc)
		}
		if err != nil {
			run.emit(Event{Type: "step-failed", Service: name, Line: err.Error()})
			e.recordResult(name, verb, "failed: "+err.Error())
			failed = true
			break
		}
		run.emit(Event{Type: "step-done", Service: name})
		e.recordResult(name, verb, "ok")
	}

	if failed {
		run.finish("deploy-failed")
	} else {
		run.finish("deploy-done")
	}
}

func (e *Engine) recordResult(service, verb, result string) {
	if e.State == nil {
		return
	}
	if err := e.State.Record(service, verb, result); err != nil {
		e.Logger.Warn("record deploy state", "service", service, "err", err)
	}
}

// EnsureDir creates a directory with the given mode and optional uid/gid
// ownership (-1 skips chown), the Go equivalent of install -d + chown.
func EnsureDir(path string, mode os.FileMode, uid, gid int) error {
	if err := os.MkdirAll(path, mode); err != nil {
		return err
	}
	if err := os.Chmod(path, mode); err != nil {
		return err
	}
	if uid >= 0 {
		if err := os.Chown(path, uid, gid); err != nil {
			return err
		}
	}
	return nil
}

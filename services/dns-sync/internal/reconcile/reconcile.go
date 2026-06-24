package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/dsjodin/provider-box/services/dns-sync/internal/model"
)

// Source produces the desired record set. NetBox is the only implementation
// in this fork; the interface exists so the loop is testable and so a future
// alternate IPAM stays a one-file change.
type Source interface {
	Desired(ctx context.Context) ([]model.Record, error)
}

// Target is the DNS server being reconciled into. The Technitium implementation
// is deferred until the API endpoint/token flow is verified against a running
// container; until then the scaffold uses LogTarget for dry-run.
type Target interface {
	List(ctx context.Context) (records []model.Record, zones []string, err error)
	Apply(ctx context.Context, ops []model.Op) error
}

type Reconciler struct {
	Source   Source
	Target   Target
	Interval time.Duration
	Logger   *slog.Logger
}

func (r *Reconciler) Run(ctx context.Context) error {
	if r.Interval <= 0 {
		r.Interval = 30 * time.Second
	}
	if r.Logger == nil {
		r.Logger = slog.Default()
	}

	r.tick(ctx)
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
			r.tick(ctx)
		}
	}
}

func (r *Reconciler) tick(ctx context.Context) {
	start := time.Now()
	ops, err := r.Once(ctx)
	if err != nil {
		r.Logger.Error("reconcile failed", "err", err, "elapsed", time.Since(start))
		return
	}
	r.Logger.Info("reconcile ok",
		"ops", len(ops),
		"creates", count(ops, model.OpCreate),
		"deletes", count(ops, model.OpDelete),
		"ensure_zones", count(ops, model.OpEnsureZone),
		"elapsed", time.Since(start),
	)
}

// Once performs a single reconcile pass and returns the ops it applied.
func (r *Reconciler) Once(ctx context.Context) ([]model.Op, error) {
	desired, err := r.Source.Desired(ctx)
	if err != nil {
		return nil, err
	}
	current, currentZones, err := r.Target.List(ctx)
	if err != nil {
		return nil, err
	}
	ops := Diff(desired, current, currentZones)
	if len(ops) == 0 {
		return ops, nil
	}
	if err := r.Target.Apply(ctx, ops); err != nil {
		return ops, err
	}
	return ops, nil
}

// Diff computes the ops needed to converge current onto desired. Order:
// EnsureZone for any desired zone not in currentZones, then Creates, then
// Deletes. Target implementations must treat EnsureZone as idempotent.
func Diff(desired, current []model.Record, currentZones []string) []model.Op {
	desiredZones := model.Zones(desired)
	have := map[string]struct{}{}
	for _, z := range currentZones {
		have[z] = struct{}{}
	}

	ops := []model.Op{}
	for _, z := range desiredZones {
		if _, ok := have[z]; ok {
			continue
		}
		ops = append(ops, model.Op{Kind: model.OpEnsureZone, Zone: z})
	}

	desiredSet := keySet(desired)
	currentSet := keySet(current)
	for _, r := range desired {
		if _, ok := currentSet[r.Key()]; ok {
			continue
		}
		ops = append(ops, model.Op{Kind: model.OpCreate, Zone: r.Zone, Record: r})
	}
	for _, r := range current {
		if _, ok := desiredSet[r.Key()]; ok {
			continue
		}
		ops = append(ops, model.Op{Kind: model.OpDelete, Zone: r.Zone, Record: r})
	}
	return ops
}

func keySet(rs []model.Record) map[string]model.Record {
	m := make(map[string]model.Record, len(rs))
	for _, r := range rs {
		m[r.Key()] = r
	}
	return m
}

func count(ops []model.Op, kind model.OpKind) int {
	n := 0
	for _, o := range ops {
		if o.Kind == kind {
			n++
		}
	}
	return n
}

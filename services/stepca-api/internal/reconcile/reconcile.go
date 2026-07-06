package reconcile

import (
	"context"
	"log/slog"
	"time"

	"github.com/dsjodin/provider-box/services/stepca-api/internal/store"
)

// Source enumerates step-ca's authoritative cert state. Implementations are
// version-specific (e.g. BadgerDB layout); keep them behind this interface so a
// step-ca upgrade only touches one file.
type Source interface {
	Issued(ctx context.Context, yield func(store.Cert) error) error
	Revoked(ctx context.Context, yield func(serial, reason string, at time.Time) error) error
}

type Reconciler struct {
	Store    *store.Store
	Source   Source
	Interval time.Duration
	Logger   *slog.Logger
}

// Run executes a reconcile pass immediately, then on Interval until ctx is done.
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
	issued, revoked, err := r.Once(ctx)
	if err != nil {
		r.Logger.Error("reconcile failed", "err", err, "elapsed", time.Since(start))
		return
	}
	r.Logger.Info("reconcile ok",
		"issued_upserts", issued,
		"revocations_applied", revoked,
		"elapsed", time.Since(start),
	)
}

// Once performs a single reconcile pass and returns counts.
func (r *Reconciler) Once(ctx context.Context) (issued, revoked int, err error) {
	err = r.Source.Issued(ctx, func(c store.Cert) error {
		c.Source = "reconcile"
		if c.Status == "" {
			c.Status = store.StatusActive
		}
		if err := r.Store.Upsert(ctx, c); err != nil {
			return err
		}
		issued++
		return nil
	})
	if err != nil {
		return issued, revoked, err
	}

	err = r.Source.Revoked(ctx, func(serial, reason string, at time.Time) error {
		switch markErr := r.Store.MarkRevoked(ctx, serial, reason, at); markErr {
		case nil:
			revoked++
			return nil
		case store.ErrNotFound:
			// Revoked cert not seen on the issued pass yet; skip and let a
			// later pass catch it once the issued record lands.
			return nil
		default:
			return markErr
		}
	})
	return issued, revoked, err
}

package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dsjodin/provider-box/services/stepca-api/internal/reconcile"
	"github.com/dsjodin/provider-box/services/stepca-api/internal/store"
)

func main() {
	dbPath := flag.String("db", envOr("STEPCA_API_DB", "/data/stepca-api.db"), "SQLite inventory path")
	caDB := flag.String("ca-db", envOr("STEPCA_API_CA_DB", "/home/step/db"), "step-ca BadgerDB path")
	interval := flag.Duration("reconcile-interval", envDuration("STEPCA_API_RECONCILE_INTERVAL", 30*time.Second), "reconcile interval")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	s, err := store.Open(*dbPath)
	if err != nil {
		logger.Error("open store", "err", err, "path", *dbPath)
		os.Exit(1)
	}
	defer s.Close()

	r := &reconcile.Reconciler{
		Store:    s,
		Source:   &reconcile.BadgerSource{Path: *caDB},
		Interval: *interval,
		Logger:   logger,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger.Info("starting reconcile loop", "db", *dbPath, "ca_db", *caDB, "interval", interval.String())
	if err := r.Run(ctx); err != nil && err != context.Canceled {
		logger.Error("reconcile loop exited", "err", err)
		os.Exit(1)
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return def
	}
	return d
}

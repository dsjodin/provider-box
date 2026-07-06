package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dsjodin/provider-box/services/stepca-api/internal/api"
	"github.com/dsjodin/provider-box/services/stepca-api/internal/reconcile"
	"github.com/dsjodin/provider-box/services/stepca-api/internal/store"
)

func main() {
	dbPath := flag.String("db", envOr("STEPCA_API_DB", "/data/stepca-api.db"), "SQLite inventory path")
	caDB := flag.String("ca-db", envOr("STEPCA_API_CA_DB", "/home/step/db"), "step-ca BadgerDB path (never opened directly; snapshotted per read)")
	snapshotDir := flag.String("snapshot-dir", envOr("STEPCA_API_SNAPSHOT_DIR", ""), "Parent directory for per-read step-ca DB snapshots (default: os.TempDir())")
	interval := flag.Duration("reconcile-interval", envDuration("STEPCA_API_RECONCILE_INTERVAL", 30*time.Second), "reconcile interval")
	addr := flag.String("addr", envOr("STEPCA_API_ADDR", ":8443"), "HTTP listen address")
	tokenFile := flag.String("token-file", os.Getenv("STEPCA_API_TOKEN_FILE"), "Path to a file containing the API bearer token (preferred over STEPCA_API_TOKEN env)")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	token := readToken(*tokenFile)
	if token == "" {
		logger.Error("API token required: set STEPCA_API_TOKEN_FILE (preferred) or STEPCA_API_TOKEN")
		os.Exit(1)
	}

	s, err := store.Open(*dbPath)
	if err != nil {
		logger.Error("open store", "err", err, "path", *dbPath)
		os.Exit(1)
	}
	defer s.Close()

	rec := &reconcile.Reconciler{
		Store:    s,
		Source:   &reconcile.BadgerSource{Path: *caDB, SnapshotRoot: *snapshotDir},
		Interval: *interval,
		Logger:   logger,
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           api.New(s, token, logger).Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Reconcile loop runs an immediate pass on start and then on Interval.
	// Failures are logged inside the loop; the HTTP server stays up regardless
	// so /healthz remains useful when step-ca is briefly unreachable.
	go func() {
		if err := rec.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("reconcile loop exited", "err", err)
		}
	}()

	go func() {
		<-ctx.Done()
		shutdownCtx, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelShutdown()
		_ = srv.Shutdown(shutdownCtx)
	}()

	logger.Info("starting http server",
		"addr", *addr,
		"db", *dbPath,
		"ca_db", *caDB,
		"snapshot_dir", *snapshotDir,
		"interval", interval.String(),
	)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("http server exited", "err", err)
		os.Exit(1)
	}
}

func readToken(tokenFile string) string {
	if tokenFile != "" {
		b, err := os.ReadFile(tokenFile)
		if err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return strings.TrimSpace(os.Getenv("STEPCA_API_TOKEN"))
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

package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dsjodin/provider-box/services/dns-sync/internal/netbox"
	"github.com/dsjodin/provider-box/services/dns-sync/internal/reconcile"
	"github.com/dsjodin/provider-box/services/dns-sync/internal/technitium"
)

func main() {
	netboxURL := flag.String("netbox-url", os.Getenv("NETBOX_URL"), "NetBox base URL")
	netboxToken := flag.String("netbox-token", os.Getenv("NETBOX_TOKEN"), "NetBox API token")
	netboxCABundle := flag.String("netbox-ca-bundle", os.Getenv("NETBOX_CA_BUNDLE"), "Optional PEM bundle for NetBox TLS")
	techURL := flag.String("technitium-url", os.Getenv("TECHNITIUM_URL"), "Technitium base URL, e.g. http://dns.sddc.lab:5380")
	techToken := flag.String("technitium-token", os.Getenv("TECHNITIUM_TOKEN"), "Technitium API token")
	techCABundle := flag.String("technitium-ca-bundle", os.Getenv("TECHNITIUM_CA_BUNDLE"), "Optional PEM bundle for Technitium TLS")
	interval := flag.Duration("interval", envDuration("DNS_SYNC_INTERVAL", 30*time.Second), "Reconcile interval")
	dryRun := flag.Bool("dry-run", false, "Log the diff via LogTarget instead of writing to Technitium")
	once := flag.Bool("once", false, "Run a single reconcile pass and exit")
	flag.Parse()

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if *netboxURL == "" || *netboxToken == "" {
		logger.Error("NETBOX_URL and NETBOX_TOKEN are required")
		os.Exit(1)
	}

	nb, err := netbox.New(*netboxURL, *netboxToken, *netboxCABundle)
	if err != nil {
		logger.Error("init netbox client", "err", err)
		os.Exit(1)
	}

	var target reconcile.Target
	if *dryRun {
		target = &reconcile.LogTarget{Logger: logger}
		logger.Info("target: dry-run (LogTarget)")
	} else {
		if *techURL == "" || *techToken == "" {
			logger.Error("TECHNITIUM_URL and TECHNITIUM_TOKEN are required (or pass --dry-run)")
			os.Exit(1)
		}
		tt, err := technitium.New(*techURL, *techToken, *techCABundle)
		if err != nil {
			logger.Error("init technitium client", "err", err)
			os.Exit(1)
		}
		target = tt
		logger.Info("target: technitium", "url", *techURL)
	}

	r := &reconcile.Reconciler{
		Source:   nb,
		Target:   target,
		Interval: *interval,
		Logger:   logger,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if *once {
		ops, err := r.Once(ctx)
		if err != nil {
			logger.Error("reconcile failed", "err", err)
			os.Exit(1)
		}
		logger.Info("reconcile complete", "ops", len(ops))
		return
	}

	logger.Info("starting dns-sync", "netbox_url", *netboxURL, "interval", interval.String())
	if err := r.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("reconcile loop exited", "err", err)
		os.Exit(1)
	}
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

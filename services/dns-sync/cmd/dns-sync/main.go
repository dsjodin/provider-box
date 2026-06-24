package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/dsjodin/provider-box/services/dns-sync/internal/netbox"
	"github.com/dsjodin/provider-box/services/dns-sync/internal/reconcile"
)

func main() {
	netboxURL := flag.String("netbox-url", os.Getenv("NETBOX_URL"), "NetBox base URL, e.g. https://netbox.sddc.lab")
	netboxToken := flag.String("netbox-token", os.Getenv("NETBOX_TOKEN"), "NetBox API token")
	netboxCABundle := flag.String("netbox-ca-bundle", os.Getenv("NETBOX_CA_BUNDLE"), "Optional PEM bundle to trust NetBox's TLS cert (e.g. step-ca root)")
	interval := flag.Duration("interval", envDuration("DNS_SYNC_INTERVAL", 30*time.Second), "Reconcile interval")
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

	// Target is the dry-run LogTarget until the Technitium API endpoint and
	// token flow are verified against a running container.
	target := &reconcile.LogTarget{Logger: logger}

	r := &reconcile.Reconciler{
		Source:   nb,
		Target:   target,
		Interval: *interval,
		Logger:   logger,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	logger.Info("starting dns-sync (dry-run target)",
		"netbox_url", *netboxURL,
		"interval", interval.String(),
	)
	if err := r.Run(ctx); err != nil && err != context.Canceled {
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

package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dsjodin/provider-box/services/dns-sync/internal/netbox"
	"github.com/dsjodin/provider-box/services/dns-sync/internal/reconcile"
	"github.com/dsjodin/provider-box/services/dns-sync/internal/technitium"
)

func main() {
	netboxURL := flag.String("netbox-url", os.Getenv("NETBOX_URL"), "NetBox base URL")
	netboxToken := flag.String("netbox-token", "", "NetBox API token (or NETBOX_TOKEN env; prefer NETBOX_TOKEN_FILE for SOPS/age)")
	netboxTokenFile := flag.String("netbox-token-file", os.Getenv("NETBOX_TOKEN_FILE"), "Path to file containing the NetBox API token")
	netboxCABundle := flag.String("netbox-ca-bundle", os.Getenv("NETBOX_CA_BUNDLE"), "Optional PEM bundle for NetBox TLS")
	techURL := flag.String("technitium-url", os.Getenv("TECHNITIUM_URL"), "Technitium base URL, e.g. http://dns.sddc.lab:5380")
	techToken := flag.String("technitium-token", "", "Technitium API token (or TECHNITIUM_TOKEN env; prefer TECHNITIUM_TOKEN_FILE for SOPS/age)")
	techTokenFile := flag.String("technitium-token-file", os.Getenv("TECHNITIUM_TOKEN_FILE"), "Path to file containing the Technitium API token")
	techCABundle := flag.String("technitium-ca-bundle", os.Getenv("TECHNITIUM_CA_BUNDLE"), "Optional PEM bundle for Technitium TLS")
	interval := flag.Duration("interval", envDuration("DNS_SYNC_INTERVAL", 30*time.Second), "Reconcile interval")
	dryRun := flag.Bool("dry-run", false, "Log the diff via LogTarget instead of writing to Technitium")
	once := flag.Bool("once", false, "Run a single reconcile pass and exit")
	flag.Parse()

	nbToken := readTokenSource(*netboxToken, *netboxTokenFile, "NETBOX_TOKEN")

	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if *netboxURL == "" || nbToken == "" {
		logger.Error("NETBOX_URL and a NetBox token (NETBOX_TOKEN_FILE preferred) are required")
		os.Exit(1)
	}

	nb, err := netbox.New(*netboxURL, nbToken, *netboxCABundle)
	if err != nil {
		logger.Error("init netbox client", "err", err)
		os.Exit(1)
	}

	var target reconcile.Target
	if *dryRun {
		target = &reconcile.LogTarget{Logger: logger}
		logger.Info("target: dry-run (LogTarget)")
	} else {
		ttToken := readTokenSource(*techToken, *techTokenFile, "TECHNITIUM_TOKEN")
		if *techURL == "" || ttToken == "" {
			logger.Error("TECHNITIUM_URL and a Technitium token (TECHNITIUM_TOKEN_FILE preferred) are required (or pass --dry-run)")
			os.Exit(1)
		}
		tt, err := technitium.New(*techURL, ttToken, *techCABundle)
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

// readTokenSource prefers a file-based token (SOPS/age path), falls back to a
// flag-passed token, then to the env var. Empty result means no token.
func readTokenSource(flagValue, fileValue, envKey string) string {
	if fileValue != "" {
		if b, err := os.ReadFile(fileValue); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	if flagValue != "" {
		return flagValue
	}
	return strings.TrimSpace(os.Getenv(envKey))
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

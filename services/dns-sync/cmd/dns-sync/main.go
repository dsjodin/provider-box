package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dsjodin/provider-box/services/dns-sync/internal/model"
	"github.com/dsjodin/provider-box/services/dns-sync/internal/netbox"
	"github.com/dsjodin/provider-box/services/dns-sync/internal/reconcile"
	"github.com/dsjodin/provider-box/services/dns-sync/internal/technitium"
)

// sourceWithBuiltins appends the built-in Provider Box service records to the
// NetBox-derived desired set on every pass. The built-ins cannot live in
// NetBox as separate IP objects (global IP uniqueness allows only the one
// canonical host IP object), so they are synthesized from the environment.
// A records only: the host IP's PTR stays the NetBox-derived canonical
// PROVIDER_BOX_FQDN, and service FQDNs must not be PTR targets.
type sourceWithBuiltins struct {
	base     reconcile.Source
	builtins []model.Record
}

func (s *sourceWithBuiltins) Desired(ctx context.Context) ([]model.Record, error) {
	desired, err := s.base.Desired(ctx)
	if err != nil {
		return nil, err
	}
	return append(desired, s.builtins...), nil
}

// parseBuiltinRecords parses "fqdn=ipv4,fqdn=ipv4" into A records.
func parseBuiltinRecords(v string) ([]model.Record, error) {
	if v == "" {
		return nil, nil
	}
	var out []model.Record
	for _, pair := range strings.Split(v, ",") {
		pair = strings.TrimSpace(pair)
		if pair == "" {
			continue
		}
		fqdn, ip, ok := strings.Cut(pair, "=")
		if !ok {
			return nil, fmt.Errorf("builtin record %q: expected fqdn=ip", pair)
		}
		addr, err := netip.ParseAddr(ip)
		if err != nil || !addr.Is4() {
			return nil, fmt.Errorf("builtin record %q: invalid IPv4 address", pair)
		}
		name := model.NormalizeFQDN(fqdn)
		if name == "" {
			return nil, fmt.Errorf("builtin record %q: empty FQDN", pair)
		}
		out = append(out, model.Record{
			Zone: model.ForwardZoneFor(name),
			Name: name,
			Type: "A",
			Data: addr.String(),
		})
	}
	return out, nil
}

func main() {
	netboxURL := flag.String("netbox-url", os.Getenv("NETBOX_URL"), "NetBox base URL")
	netboxToken := flag.String("netbox-token", "", "NetBox API token (or NETBOX_TOKEN env; prefer NETBOX_TOKEN_FILE for SOPS/age). NetBox 4.6+ v2 tokens must be the full composite nbt_<key>.<token>")
	netboxTokenFile := flag.String("netbox-token-file", os.Getenv("NETBOX_TOKEN_FILE"), "Path to file containing the NetBox API token")
	netboxCABundle := flag.String("netbox-ca-bundle", os.Getenv("NETBOX_CA_BUNDLE"), "Optional PEM bundle for NetBox TLS")
	techURL := flag.String("technitium-url", os.Getenv("TECHNITIUM_URL"), "Technitium base URL, e.g. http://dns.sddc.lab:5380")
	techToken := flag.String("technitium-token", "", "Technitium API token (or TECHNITIUM_TOKEN env; prefer TECHNITIUM_TOKEN_FILE for SOPS/age)")
	techTokenFile := flag.String("technitium-token-file", os.Getenv("TECHNITIUM_TOKEN_FILE"), "Path to file containing the Technitium API token")
	techCABundle := flag.String("technitium-ca-bundle", os.Getenv("TECHNITIUM_CA_BUNDLE"), "Optional PEM bundle for Technitium TLS")
	techDashboardUser := flag.String("technitium-dashboard-user", os.Getenv("DNS_SYNC_TECHNITIUM_DASHBOARD_USER"), "Optional non-admin Technitium username granted View on newly created zones so the read-only dashboard lists them; empty disables")
	interval := flag.Duration("interval", envDuration("DNS_SYNC_INTERVAL", 30*time.Second), "Reconcile interval")
	builtinRecords := flag.String("builtin-records", os.Getenv("DNS_SYNC_BUILTIN_RECORDS"), "Comma-separated fqdn=ipv4 built-in service records merged into the desired set")
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

	var source reconcile.Source = nb
	builtins, err := parseBuiltinRecords(*builtinRecords)
	if err != nil {
		logger.Error("parse builtin records", "err", err)
		os.Exit(1)
	}
	if len(builtins) > 0 {
		source = &sourceWithBuiltins{base: nb, builtins: builtins}
		logger.Info("built-in service records merged into desired set", "count", len(builtins))
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
		tt.DashboardReadonlyUser = *techDashboardUser
		tt.Logger = logger
		target = tt
		logger.Info("target: technitium", "url", *techURL, "dashboard_zone_grant", *techDashboardUser != "")
	}

	r := &reconcile.Reconciler{
		Source:   source,
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

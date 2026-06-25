// dns-seed is the one-shot bootstrap CLI for the DNS layer:
//
//	dns-seed netbox-import <seedfile>   - import seed into NetBox IPAM (idempotent)
//	dns-seed set-forwarder <ip>...      - configure Technitium upstream forwarder (idempotent)
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/dsjodin/provider-box/services/dns-sync/internal/netbox"
	"github.com/dsjodin/provider-box/services/dns-sync/internal/seed"
	"github.com/dsjodin/provider-box/services/dns-sync/internal/technitium"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	sub := os.Args[1]
	args := os.Args[2:]
	switch sub {
	case "netbox-import":
		if err := runNetboxImport(ctx, args, logger); err != nil {
			logger.Error("netbox-import failed", "err", err)
			os.Exit(1)
		}
	case "set-forwarder":
		if err := runSetForwarder(ctx, args, logger); err != nil {
			logger.Error("set-forwarder failed", "err", err)
			os.Exit(1)
		}
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n", sub)
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: dns-seed <subcommand> [flags]")
	fmt.Fprintln(os.Stderr)
	fmt.Fprintln(os.Stderr, "subcommands:")
	fmt.Fprintln(os.Stderr, "  netbox-import   import config/dns.seed entries into NetBox IPAM (idempotent)")
	fmt.Fprintln(os.Stderr, "  set-forwarder   configure Technitium upstream forwarder (idempotent)")
}

func runNetboxImport(ctx context.Context, args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("netbox-import", flag.ExitOnError)
	netboxURL := fs.String("netbox-url", os.Getenv("NETBOX_URL"), "NetBox base URL")
	netboxToken := fs.String("netbox-token", "", "NetBox API token (or NETBOX_TOKEN env; prefer NETBOX_TOKEN_FILE)")
	netboxTokenFile := fs.String("netbox-token-file", os.Getenv("NETBOX_TOKEN_FILE"), "Path to file containing the NetBox API token")
	netboxCABundle := fs.String("netbox-ca-bundle", os.Getenv("NETBOX_CA_BUNDLE"), "Optional PEM bundle for NetBox TLS")
	seedPath := fs.String("seed", "", "Path to the seed file (default: <last positional arg>)")
	_ = fs.Parse(args)

	if *seedPath == "" && fs.NArg() > 0 {
		*seedPath = fs.Arg(0)
	}
	if *seedPath == "" {
		return fmt.Errorf("--seed <path> (or a positional path arg) is required")
	}

	token := readToken(*netboxToken, *netboxTokenFile)
	if *netboxURL == "" || token == "" {
		return fmt.Errorf("NETBOX_URL and a NetBox token (token-file preferred) are required")
	}

	nb, err := netbox.New(*netboxURL, token, *netboxCABundle)
	if err != nil {
		return err
	}

	f, err := os.Open(*seedPath)
	if err != nil {
		return fmt.Errorf("open seed: %w", err)
	}
	defer f.Close()
	entries, err := seed.Parse(f)
	if err != nil {
		return err
	}
	logger.Info("loaded seed", "path", *seedPath, "entries", len(entries))

	// First pass: prefixes. Dedupe to avoid repeated checks.
	seenPrefix := map[string]struct{}{}
	prefixCreated, prefixSkipped := 0, 0
	for _, e := range entries {
		if !e.HasPrefix() {
			continue
		}
		key := e.Prefix.String()
		if _, ok := seenPrefix[key]; ok {
			continue
		}
		seenPrefix[key] = struct{}{}
		created, err := nb.EnsurePrefix(ctx, key)
		if err != nil {
			return err
		}
		if created {
			prefixCreated++
			logger.Info("created prefix", "prefix", key)
		} else {
			prefixSkipped++
		}
	}

	// Second pass: IPs. AGENTS.md: one IP object per address. If a seed
	// repeats the same address with different FQDNs, lex-smallest FQDN wins.
	canonical := map[string]string{} // addr+mask -> chosen FQDN
	for _, e := range entries {
		key := ipKey(e)
		if cur, ok := canonical[key]; !ok || e.FQDN < cur {
			canonical[key] = e.FQDN
		}
	}

	ipCreated, ipSkipped := 0, 0
	processed := map[string]struct{}{}
	for _, e := range entries {
		key := ipKey(e)
		if _, ok := processed[key]; ok {
			continue
		}
		processed[key] = struct{}{}
		created, err := nb.EnsureIPAddress(ctx, key, canonical[key])
		if err != nil {
			return err
		}
		if created {
			ipCreated++
			logger.Info("created ip", "address", key, "dns_name", canonical[key])
		} else {
			ipSkipped++
		}
	}

	logger.Info("netbox-import complete",
		"prefix_created", prefixCreated, "prefix_skipped", prefixSkipped,
		"ip_created", ipCreated, "ip_skipped", ipSkipped,
	)
	return nil
}

func ipKey(e seed.Entry) string {
	if e.HasPrefix() {
		bits := e.Prefix.Bits()
		return fmt.Sprintf("%s/%d", e.Addr.String(), bits)
	}
	return e.Addr.String() + "/32"
}

func runSetForwarder(ctx context.Context, args []string, logger *slog.Logger) error {
	fs := flag.NewFlagSet("set-forwarder", flag.ExitOnError)
	techURL := fs.String("technitium-url", os.Getenv("TECHNITIUM_URL"), "Technitium base URL")
	techToken := fs.String("technitium-token", "", "Technitium API token (or TECHNITIUM_TOKEN env; prefer TECHNITIUM_TOKEN_FILE)")
	techTokenFile := fs.String("technitium-token-file", os.Getenv("TECHNITIUM_TOKEN_FILE"), "Path to file containing the Technitium API token")
	techCABundle := fs.String("technitium-ca-bundle", os.Getenv("TECHNITIUM_CA_BUNDLE"), "Optional PEM bundle for Technitium TLS")
	upstreamsFlag := fs.String("forwarders", os.Getenv("TECHNITIUM_FORWARDER"), "Comma-separated upstream forwarders (or positional args)")
	_ = fs.Parse(args)

	upstreams := []string{}
	if *upstreamsFlag != "" {
		for _, u := range strings.Split(*upstreamsFlag, ",") {
			if s := strings.TrimSpace(u); s != "" {
				upstreams = append(upstreams, s)
			}
		}
	}
	upstreams = append(upstreams, fs.Args()...)
	if len(upstreams) == 0 {
		return fmt.Errorf("at least one forwarder is required (--forwarders=8.8.8.8 or positional args)")
	}

	token := readTechnitiumToken(*techToken, *techTokenFile)
	if *techURL == "" || token == "" {
		return fmt.Errorf("TECHNITIUM_URL and a Technitium token (token-file preferred) are required")
	}

	tt, err := technitium.New(*techURL, token, *techCABundle)
	if err != nil {
		return err
	}
	if err := tt.SetForwarder(ctx, upstreams...); err != nil {
		return err
	}
	logger.Info("forwarder set", "upstreams", upstreams)
	return nil
}

func readTechnitiumToken(flagValue, fileValue string) string {
	if fileValue != "" {
		if b, err := os.ReadFile(fileValue); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	if flagValue != "" {
		return flagValue
	}
	return strings.TrimSpace(os.Getenv("TECHNITIUM_TOKEN"))
}

func readToken(flagValue, fileValue string) string {
	if fileValue != "" {
		if b, err := os.ReadFile(fileValue); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	if flagValue != "" {
		return flagValue
	}
	return strings.TrimSpace(os.Getenv("NETBOX_TOKEN"))
}

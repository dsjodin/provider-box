package deploy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// DNSSync deploys the NetBox-to-Technitium reconcile loop, the port of
// bootstrap/dns-sync.sh. The image is built from the dns-sync source baked
// into the control-plane image (no registry or repo checkout needed on the
// host). dns-sync deliberately never touches the host resolver; that belongs
// to the technitium deployer alone.
type DNSSync struct{}

func (DNSSync) Name() string   { return "dns-sync" }
func (DNSSync) Deps() []string { return []string{"netbox", "technitium"} }

// dnsSyncSourceDirs are the candidate build contexts: the image-baked copy
// first, the repo checkout as the fallback for non-container runs.
var dnsSyncSourceDirs = []string{
	"/usr/local/share/provider-box/services/dns-sync",
	"services/dns-sync",
}

var intervalRe = regexp.MustCompile(`^[0-9]+[smh]$`)

func (d DNSSync) Deploy(ctx context.Context, rc *RunCtx) error {
	env := rc.Env
	runtime := rc.Workdir("dns-sync")

	if !intervalRe.MatchString(env["DNS_SYNC_INTERVAL"]) {
		return fmt.Errorf("DNS_SYNC_INTERVAL must look like 30s, 5m, or 1h")
	}
	for _, u := range []string{env["DNS_SYNC_NETBOX_URL"], env["DNS_SYNC_TECHNITIUM_URL"]} {
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			return fmt.Errorf("DNS_SYNC_*_URL must be an http(s):// URL: %q", u)
		}
	}
	if strings.HasPrefix(env["DNS_SYNC_SECRETS_DIR"], runtime) {
		return fmt.Errorf("DNS_SYNC_SECRETS_DIR must not be inside %s so remove preserves the operator's secrets", runtime)
	}

	caRoot := filepath.Join(env["CA_DATA_DIR"], "certs", "root_ca.crt")
	if _, err := os.Stat(caRoot); err != nil {
		return fmt.Errorf("missing step-ca root certificate at %s; deploy ca first", caRoot)
	}
	// Both readiness gates pin the lab FQDNs to 127.0.0.1, so nothing depends
	// on the zone dns-sync is about to populate.
	if err := WaitHTTPSPinned(ctx, env["DNS_SYNC_NETBOX_URL"]+"/api/", caRoot, 3, 2*time.Second); err != nil {
		return fmt.Errorf("NetBox is not reachable on 127.0.0.1 (deploy netbox first): %w", err)
	}
	if err := WaitHTTPSPinned(ctx, env["DNS_SYNC_TECHNITIUM_URL"]+"/", caRoot, 3, 2*time.Second); err != nil {
		return fmt.Errorf("Technitium is not reachable on 127.0.0.1 (deploy technitium first): %w", err)
	}

	if err := EnsureDir(runtime, 0o755, -1, -1); err != nil {
		return err
	}
	if err := EnsureDir(env["DNS_SYNC_DIR"], 0o755, 1000, 1000); err != nil {
		return err
	}
	if err := EnsureDir(env["DNS_SYNC_SECRETS_DIR"], 0o700, 1000, 1000); err != nil {
		return err
	}
	for _, name := range []string{"netbox.token", "technitium.token"} {
		tokenFile := filepath.Join(env["DNS_SYNC_SECRETS_DIR"], name)
		fi, err := os.Stat(tokenFile)
		if err != nil || fi.Size() == 0 {
			return fmt.Errorf("missing or empty token at %s; it is auto-provisioned by the netbox/technitium deploy (re-run it), or place a token there manually (SOPS/age)", tokenFile)
		}
		_ = os.Chmod(tokenFile, 0o600)
		_ = os.Chown(tokenFile, 1000, 1000)
	}

	src, err := findDNSSyncSource()
	if err != nil {
		return err
	}
	cmp := rc.Compose("dns-sync")
	rc.Log("Building %s from %s.", env["DNS_SYNC_IMAGE"], src)
	if err := cmp.Build(ctx, env["DNS_SYNC_IMAGE"], src); err != nil {
		return err
	}

	if err := applyDNSSeedToNetbox(ctx, rc); err != nil {
		return err
	}

	builtins, err := builtinRecordsValue(env)
	if err != nil {
		return err
	}
	renderEnv := withDerived(env, map[string]string{
		"DNS_SYNC_NETBOX_HOST":     urlHost(env["DNS_SYNC_NETBOX_URL"]),
		"DNS_SYNC_TECHNITIUM_HOST": urlHost(env["DNS_SYNC_TECHNITIUM_URL"]),
		"DNS_SYNC_BUILTIN_RECORDS": builtins,
	})
	if err := Render("docker-compose.dns-sync.yml.tpl", renderEnv, runtime+"/docker-compose.yml", 0o644); err != nil {
		return err
	}
	if err := cmp.Down(ctx); err != nil {
		return err
	}
	if err := cmp.Up(ctx); err != nil {
		return err
	}

	// Real-DNS verification: after the first reconcile the lab zone must be
	// served by Technitium - exactly the output dns-sync exists to produce.
	rc.Log("Verifying dns-sync populated the lab zone (%s via 127.0.0.1).", env["PROVIDER_BOX_FQDN"])
	if err := waitRecordResolves(ctx, env["PROVIDER_BOX_FQDN"], 45, 2*time.Second); err != nil {
		return fmt.Errorf("dns-sync did not populate the lab zone (check the dns-sync logs and that NetBox holds the canonical host IP): %w", err)
	}
	for _, fqdn := range builtinServiceFQDNs(env) {
		if err := waitRecordResolves(ctx, fqdn, 15, 2*time.Second); err != nil {
			return fmt.Errorf("built-in service record %s does not resolve via Technitium: %w", fqdn, err)
		}
	}
	rc.Log("All built-in Provider Box service FQDNs resolve via Technitium.")
	rc.Log("dns-sync is running. Reconcile interval: %s.", env["DNS_SYNC_INTERVAL"])
	return nil
}

func (d DNSSync) Remove(ctx context.Context, rc *RunCtx) error {
	cmp := rc.Compose("dns-sync")
	if err := cmp.Down(ctx); err != nil {
		return err
	}
	if err := os.RemoveAll(rc.Workdir("dns-sync")); err != nil {
		return err
	}
	rc.Log("Removed dns-sync containers and runtime files. Operator secrets in %s were preserved.", rc.Env["DNS_SYNC_SECRETS_DIR"])
	return nil
}

func findDNSSyncSource() (string, error) {
	for _, dir := range dnsSyncSourceDirs {
		if _, err := os.Stat(filepath.Join(dir, "Dockerfile")); err == nil {
			return dir, nil
		}
	}
	return "", fmt.Errorf("dns-sync source not found (looked in %s)", strings.Join(dnsSyncSourceDirs, ", "))
}

func urlHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return u.Hostname()
}

// applyDNSSeedToNetbox imports the optional dns.seed into NetBox via the
// dns-seed binary (idempotent), so the reconcile loop sees the same records
// as desired state.
func applyDNSSeedToNetbox(ctx context.Context, rc *RunCtx) error {
	env := rc.Env
	seedFile := seedFilePath(env)
	if _, err := os.Stat(seedFile); err != nil {
		rc.Log("No dns.seed at %s; skipping NetBox seed import.", seedFile)
		return nil
	}
	rc.Log("Importing %s into NetBox (idempotent).", seedFile)
	runner := Compose{Out: func(line string) { rc.Log("%s", line) }}
	return runner.RunRM(ctx,
		"--user", "1000:1000",
		"--network", "host",
		"--add-host", urlHost(env["DNS_SYNC_NETBOX_URL"])+":127.0.0.1",
		"-e", "NETBOX_URL="+env["DNS_SYNC_NETBOX_URL"],
		"-e", "NETBOX_TOKEN_FILE=/run/provider-box/secrets/netbox.token",
		"-e", "NETBOX_CA_BUNDLE=/etc/provider-box/certs/root_ca.crt",
		"-v", env["DNS_SYNC_SECRETS_DIR"]+":/run/provider-box/secrets:ro",
		"-v", filepath.Join(env["CA_DATA_DIR"], "certs", "root_ca.crt")+":/etc/provider-box/certs/root_ca.crt:ro",
		"-v", seedFile+":/etc/provider-box/dns.seed:ro",
		env["DNS_SYNC_IMAGE"],
		"dns-seed", "netbox-import", "/etc/provider-box/dns.seed",
	)
}

// builtinRecordsValue synthesizes the fqdn=ip list dns-sync merges into its
// desired record set on every reconcile.
func builtinRecordsValue(env map[string]string) (string, error) {
	fqdns := builtinServiceFQDNs(env)
	all := append([]string{env["PROVIDER_BOX_FQDN"]}, fqdns...)
	var parts []string
	for _, fqdn := range all {
		if fqdn != "" {
			parts = append(parts, fqdn+"="+env["HOST_IPV4"])
		}
	}
	if len(parts) == 0 {
		return "", fmt.Errorf("no built-in service FQDNs are set; check the *_FQDN variables")
	}
	return strings.Join(parts, ","), nil
}

// waitRecordResolves requires an actual A answer from 127.0.0.1:53.
func waitRecordResolves(ctx context.Context, fqdn string, attempts int, interval time.Duration) error {
	r := resolverVia127()
	var lastErr error
	for i := 0; i < attempts; i++ {
		qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		addrs, err := r.LookupHost(qctx, fqdn)
		cancel()
		if err == nil && len(addrs) > 0 {
			return nil
		}
		if err == nil {
			err = errors.New("empty answer")
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	var dnsErr *net.DNSError
	if errors.As(lastErr, &dnsErr) {
		return fmt.Errorf("no A record for %s: %w", fqdn, lastErr)
	}
	return lastErr
}

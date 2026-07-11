package deploy

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	pkcs12 "software.sslmate.com/src/go-pkcs12"
)

// Technitium deploys the containerized DNS server, the port of
// bootstrap/technitium.sh. Deploying it is the explicit opt-in to running DNS
// on this host: after the listener, forwarder, HTTPS endpoint, and API tokens
// are all verified, the host resolver is pointed at Technitium. The
// systemd-resolved stub listener is already disabled by install.sh.
type Technitium struct{}

func (Technitium) Name() string   { return "technitium" }
func (Technitium) Deps() []string { return []string{"ca"} }

const technitiumResolvMarker = "Managed by Provider Box (technitium deploy)"

func (t Technitium) Deploy(ctx context.Context, rc *RunCtx) error {
	env := rc.Env
	certDir := env["TECHNITIUM_CERT_DIR"]
	runtime := rc.Workdir("technitium")

	for _, dir := range []string{env["TECHNITIUM_DATA_DIR"], certDir} {
		if dir == runtime || strings.HasPrefix(dir, runtime+"/") {
			return fmt.Errorf("%s must not be inside %s so remove preserves Technitium content", dir, runtime)
		}
	}
	if err := requireCAReady(ctx, env); err != nil {
		return err
	}
	if err := EnsureDir(runtime, 0o755, -1, -1); err != nil {
		return err
	}
	if err := EnsureDir(env["TECHNITIUM_DATA_DIR"], 0o755, 1000, 1000); err != nil {
		return err
	}

	if err := IssueCert(ctx, rc, env["DNS_FQDN"], certDir, "technitium"); err != nil {
		return err
	}
	if err := buildTechnitiumChainBundles(ctx, rc, certDir); err != nil {
		return err
	}
	if err := buildTechnitiumPfx(rc, certDir); err != nil {
		return err
	}
	if err := Render("docker-compose.technitium.yml.tpl", env, runtime+"/docker-compose.yml", 0o644); err != nil {
		return err
	}

	cmp := rc.Compose("technitium")
	// Pre-pull BEFORE stopping the running container: when Technitium is the
	// host resolver, stopping it first would take DNS down and an un-cached
	// image could not be pulled. A failed pull aborts with the old server
	// still running.
	if err := cmp.Pull(ctx); err != nil {
		return fmt.Errorf("pull %s failed; the running DNS server was left untouched: %w", env["TECHNITIUM_IMAGE"], err)
	}
	if err := cmp.Down(ctx); err != nil {
		return err
	}
	if err := preflightPort53(rc); err != nil {
		return err
	}
	if err := cmp.Up(ctx); err != nil {
		return err
	}

	rc.Log("Waiting for the Technitium DNS listener on 127.0.0.1:53.")
	if err := waitDNSListener(ctx, env["DNS_FQDN"], 60, 2*time.Second); err != nil {
		return err
	}

	api := newTechnitiumAPI(env)
	adminToken, err := api.AdminToken(ctx, rc)
	if err != nil {
		return err
	}

	// Upstream forwarder; this deployer is the only owner of the setting.
	out, err := api.callOK(ctx, "/api/settings/set", url.Values{
		"token": {adminToken}, "forwarders": {env["DNS_FORWARDER"]}, "forwarderProtocol": {"Udp"},
	})
	if err != nil {
		return fmt.Errorf("set the Technitium upstream forwarder %s: %w", env["DNS_FORWARDER"], err)
	}
	if resp, _ := out["response"].(map[string]any); resp != nil {
		if recursion, _ := resp["recursion"].(string); recursion == "Deny" {
			return fmt.Errorf("Technitium recursion is disabled (recursion=Deny); external names cannot be resolved")
		}
	}
	rc.Log("Technitium upstream forwarder set to %s (UDP).", env["DNS_FORWARDER"])

	rc.Log("Verifying Technitium can resolve external names via %s.", env["DNS_FORWARDER"])
	if err := waitExternalResolution(ctx, 30, 2*time.Second); err != nil {
		return err
	}

	// Web service TLS with the step-ca PKCS#12 bundle (container-internal port
	// 53443, published as TECHNITIUM_HTTPS_PORT).
	pfxPassword, err := os.ReadFile(filepath.Join(certDir, "technitium-pfx-password"))
	if err != nil {
		return err
	}
	if _, err := api.callOK(ctx, "/api/settings/set", url.Values{
		"token":                           {adminToken},
		"webServiceEnableTls":             {"true"},
		"webServiceTlsPort":               {"53443"},
		"webServiceTlsCertificatePath":    {"/etc/provider-box/technitium-certs/technitium.pfx"},
		"webServiceTlsCertificatePassword": {string(pfxPassword)},
	}); err != nil {
		return fmt.Errorf("enable Technitium web service TLS: %w", err)
	}
	rc.Log("Technitium web service TLS enabled with the step-ca certificate.")
	httpsURL := fmt.Sprintf("https://%s:%s/", env["DNS_FQDN"], env["TECHNITIUM_HTTPS_PORT"])
	if err := WaitHTTPSPinned(ctx, httpsURL, filepath.Join(env["CA_DATA_DIR"], "certs", "root_ca.crt"), 30, 2*time.Second); err != nil {
		return err
	}

	if err := provisionTechnitiumDNSSyncToken(ctx, rc, api, adminToken); err != nil {
		return err
	}
	if err := provisionTechnitiumDashboardToken(ctx, rc, api, adminToken); err != nil {
		return err
	}
	if err := pointHostResolverAtTechnitium(rc); err != nil {
		return err
	}

	rc.Log("Technitium is ready. Web console: http://%s:%s and https://%s:%s",
		env["DNS_FQDN"], env["TECHNITIUM_HTTP_PORT"], env["DNS_FQDN"], env["TECHNITIUM_HTTPS_PORT"])
	return nil
}

func (t Technitium) Remove(ctx context.Context, rc *RunCtx) error {
	cmp := rc.Compose("technitium")
	if err := cmp.Down(ctx); err != nil {
		return err
	}
	if err := os.RemoveAll(rc.Workdir("technitium")); err != nil {
		return err
	}
	restoreHostResolver(rc)
	rc.Log("Removed Technitium containers and runtime files. Persistent data in %s and certificates in %s were preserved.",
		rc.Env["TECHNITIUM_DATA_DIR"], rc.Env["TECHNITIUM_CERT_DIR"])
	return nil
}

// buildTechnitiumChainBundles writes the CA chain bundle (intermediate+root)
// and the roots bundle alongside the leaf.
func buildTechnitiumChainBundles(ctx context.Context, rc *RunCtx, certDir string) error {
	env := rc.Env
	intermediate, err := os.ReadFile(filepath.Join(env["CA_DATA_DIR"], "certs", "intermediate_ca.crt"))
	if err != nil {
		return err
	}
	root, err := os.ReadFile(filepath.Join(env["CA_DATA_DIR"], "certs", "root_ca.crt"))
	if err != nil {
		return err
	}
	chain := filepath.Join(certDir, "technitium-ca-chain.pem")
	if err := os.WriteFile(chain, append(intermediate, root...), 0o644); err != nil {
		return err
	}
	roots := filepath.Join(certDir, "technitium-ca-roots.pem")
	if err := os.WriteFile(roots, root, 0o644); err != nil {
		return err
	}
	for _, f := range []string{chain, roots} {
		_ = os.Chown(f, 1000, 1000)
	}
	return nil
}

// buildTechnitiumPfx converts the PEM material into technitium.pfx with a
// generated persisted password; rebuilt whenever the PEM is newer. Technitium
// (.NET) requires the web TLS certificate as PKCS#12; the Legacy-RC2 encoder
// matches what `openssl pkcs12 -export` produced for the bash module.
func buildTechnitiumPfx(rc *RunCtx, certDir string) error {
	pfxFile := filepath.Join(certDir, "technitium.pfx")
	passwordFile := filepath.Join(certDir, "technitium-pfx-password")

	if _, err := os.Stat(passwordFile); err != nil {
		raw := make([]byte, 24)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		if err := os.WriteFile(passwordFile, []byte(base64.StdEncoding.EncodeToString(raw)), 0o600); err != nil {
			return err
		}
		rc.Log("Generated Technitium PKCS#12 password at: %s", passwordFile)
	}
	_ = os.Chmod(passwordFile, 0o600)
	_ = os.Chown(passwordFile, 1000, 1000)
	password, err := os.ReadFile(passwordFile)
	if err != nil {
		return err
	}

	certPath := filepath.Join(certDir, "technitium.crt")
	keyPath := filepath.Join(certDir, "technitium.key")
	if fresh, err := fileNewer(pfxFile, certPath, keyPath); err == nil && fresh {
		_ = os.Chmod(pfxFile, 0o600)
		_ = os.Chown(pfxFile, 1000, 1000)
		return nil
	}

	rc.Log("Building the Technitium PKCS#12 bundle at %s.", pfxFile)
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return err
	}
	leaf, chain, err := parseCertChain(certPEM)
	if err != nil {
		return err
	}
	key, err := parsePrivateKey(keyPEM)
	if err != nil {
		return err
	}
	pfx, err := pkcs12.LegacyRC2.Encode(key, leaf, chain, string(password))
	if err != nil {
		return fmt.Errorf("encode PKCS#12: %w", err)
	}
	if err := os.WriteFile(pfxFile, pfx, 0o600); err != nil {
		return err
	}
	return os.Chown(pfxFile, 1000, 1000)
}

// fileNewer reports whether target exists and is newer than every source.
func fileNewer(target string, sources ...string) (bool, error) {
	ti, err := os.Stat(target)
	if err != nil {
		return false, err
	}
	for _, s := range sources {
		si, err := os.Stat(s)
		if err != nil {
			return false, err
		}
		if si.ModTime().After(ti.ModTime()) {
			return false, nil
		}
	}
	return true, nil
}

func parseCertChain(pemBytes []byte) (leaf *x509.Certificate, chain []*x509.Certificate, err error) {
	var certs []*x509.Certificate
	for {
		var block *pem.Block
		block, pemBytes = pem.Decode(pemBytes)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, nil, err
		}
		certs = append(certs, c)
	}
	if len(certs) == 0 {
		return nil, nil, fmt.Errorf("no certificates in PEM")
	}
	return certs[0], certs[1:], nil
}

func parsePrivateKey(pemBytes []byte) (any, error) {
	block, _ := pem.Decode(pemBytes)
	if block == nil {
		return nil, fmt.Errorf("no PEM block in key file")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// preflightPort53 test-binds TCP and UDP :53. install.sh disables the
// systemd-resolved stub listener, so a holder here is a real conflict.
func preflightPort53(rc *RunCtx) error {
	l, err := net.Listen("tcp", ":53")
	if err != nil {
		return fmt.Errorf("port 53/tcp is already in use and Provider Box will not stop the holder automatically (a leftover unbound or dnsmasq? if systemd-resolved holds it, re-run install.sh): %w", err)
	}
	l.Close()
	u, err := net.ListenPacket("udp", ":53")
	if err != nil {
		return fmt.Errorf("port 53/udp is already in use and Provider Box will not stop the holder automatically: %w", err)
	}
	u.Close()
	return nil
}

// resolverVia127 queries the DNS server on 127.0.0.1:53 directly, regardless
// of the host or container resolv.conf.
func resolverVia127() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 2 * time.Second}
			return d.DialContext(ctx, network, "127.0.0.1:53")
		},
	}
}

// waitDNSListener waits until the server ANSWERS at all: NXDOMAIN counts as
// up (the lab zone does not exist yet at first deploy), only timeouts and
// connection errors count as down.
func waitDNSListener(ctx context.Context, probeName string, attempts int, interval time.Duration) error {
	r := resolverVia127()
	var lastErr error
	for i := 0; i < attempts; i++ {
		qctx, cancel := context.WithTimeout(ctx, 3*time.Second)
		_, err := r.LookupHost(qctx, probeName)
		cancel()
		var dnsErr *net.DNSError
		if err == nil || (errors.As(err, &dnsErr) && dnsErr.IsNotFound) {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	return fmt.Errorf("Technitium DNS listener did not become ready on 127.0.0.1:53: %w", lastErr)
}

// waitExternalResolution requires an actual answer for an external name.
func waitExternalResolution(ctx context.Context, attempts int, interval time.Duration) error {
	r := resolverVia127()
	var lastErr error
	for i := 0; i < attempts; i++ {
		qctx, cancel := context.WithTimeout(ctx, 4*time.Second)
		addrs, err := r.LookupHost(qctx, "one.one.one.one")
		cancel()
		if err == nil && len(addrs) > 0 {
			return nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	return fmt.Errorf("Technitium cannot resolve external names - check DNS_FORWARDER reachability: %w", lastErr)
}

// provisionTechnitiumDNSSyncToken mints (or reuses) the dns-sync API token.
// The token now belongs to the admin user but is created via the admin
// session instead of raw first-boot credentials.
func provisionTechnitiumDNSSyncToken(ctx context.Context, rc *RunCtx, api technitiumAPI, adminToken string) error {
	env := rc.Env
	if err := EnsureDir(env["DNS_SYNC_SECRETS_DIR"], 0o700, -1, -1); err != nil {
		return err
	}
	tokenFile := filepath.Join(env["DNS_SYNC_SECRETS_DIR"], "technitium.token")
	if stored, err := os.ReadFile(tokenFile); err == nil && len(stored) > 0 {
		if api.TokenValid(ctx, string(stored), "/api/zones/list") {
			rc.Log("Reusing existing Technitium API token: %s", tokenFile)
			_ = os.Chmod(tokenFile, 0o600)
			_ = os.Chown(tokenFile, 1000, 1000)
			return nil
		}
		rc.Log("Stored Technitium API token is no longer valid; creating a replacement.")
	}
	token, err := api.CreateToken(ctx, adminToken, "admin", "provider-box-dns-sync")
	if err != nil {
		return fmt.Errorf("create the dns-sync Technitium token: %w", err)
	}
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		return err
	}
	_ = os.Chown(tokenFile, 1000, 1000)
	rc.Log("Provisioned a Technitium API token for dns-sync at: %s", tokenFile)
	return nil
}

// provisionTechnitiumDashboardToken creates the non-admin 'dashboard' user,
// grants it Settings:View plus per-zone View on every existing zone (zone
// visibility needs the explicit per-zone grant), and mints its scoped token
// for the control plane's DNS panel. Grants are re-applied on every run so
// zones created since the last run are picked up; a still-valid stored token
// (operator override included) is reused.
func provisionTechnitiumDashboardToken(ctx context.Context, rc *RunCtx, api technitiumAPI, adminToken string) error {
	env := rc.Env
	secretsDir := env["CONTROL_PLANE_SECRETS_DIR"]
	if secretsDir == "" {
		rc.Log("NOTICE: CONTROL_PLANE_SECRETS_DIR is not set; skipping dashboard Technitium token provisioning.")
		return nil
	}
	if err := EnsureDir(secretsDir, 0o700, 1000, 1000); err != nil {
		return err
	}
	tokenFile := filepath.Join(secretsDir, "technitium.token")

	if !api.UserExists(ctx, adminToken, "dashboard") {
		raw := make([]byte, 24)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		pass := base64.StdEncoding.EncodeToString(raw) + "Aa1!"
		if err := api.CreateUser(ctx, adminToken, "dashboard", "Provider Box Dashboard", pass); err != nil {
			return fmt.Errorf("create the Technitium dashboard user: %w", err)
		}
	}

	// Settings:View (the DNS panel reads settings/get); the admin groups are
	// re-sent so the section grant does not drop their access.
	if _, err := api.callOK(ctx, "/api/admin/permissions/set", url.Values{
		"token":            {adminToken},
		"section":          {"Settings"},
		"groupPermissions": {"Administrators|true|true|true|DNS Administrators|true|true|true"},
		"userPermissions":  {"dashboard|true|false|false"},
	}); err != nil {
		return fmt.Errorf("grant the Technitium dashboard user Settings:View: %w", err)
	}

	zones, err := api.ZoneNames(ctx, adminToken)
	if err != nil {
		return err
	}
	for _, zone := range zones {
		// Only userPermissions is sent: the API syncs user and group tables
		// independently, so the zone's admin-group access stays untouched.
		if _, err := api.callOK(ctx, "/api/zones/permissions/set", url.Values{
			"token":           {adminToken},
			"zone":            {zone},
			"userPermissions": {"admin|true|true|true|dashboard|true|false|false"},
		}); err != nil {
			rc.Log("NOTICE: could not grant the dashboard user View on Technitium zone %s: %v", zone, err)
		}
	}

	if stored, err := os.ReadFile(tokenFile); err == nil && len(stored) > 0 {
		if api.TokenValid(ctx, string(stored), "/api/settings/get") {
			rc.Log("Reusing existing dashboard Technitium token: %s", tokenFile)
			_ = os.Chmod(tokenFile, 0o600)
			_ = os.Chown(tokenFile, 1000, 1000)
			return nil
		}
		rc.Log("Stored dashboard Technitium token is no longer valid; creating a replacement.")
	}
	token, err := api.CreateToken(ctx, adminToken, "dashboard", "provider-box-dashboard")
	if err != nil {
		return fmt.Errorf("create the dashboard Technitium token: %w", err)
	}
	if !api.TokenValid(ctx, token, "/api/settings/get") {
		return fmt.Errorf("freshly minted Technitium dashboard token cannot read settings; check the Settings:View grant")
	}
	if err := os.WriteFile(tokenFile, []byte(token), 0o600); err != nil {
		return err
	}
	_ = os.Chown(tokenFile, 1000, 1000)
	rc.Log("Provisioned a read-only dashboard Technitium token at: %s", tokenFile)
	return nil
}

// hostEtc returns the host /etc path: /host/etc when running in the
// control-plane container (install.sh mounts it), /etc otherwise.
func hostEtc() string {
	if fi, err := os.Stat("/host/etc"); err == nil && fi.IsDir() {
		return "/host/etc"
	}
	return "/etc"
}

// pointHostResolverAtTechnitium rewrites the host resolv.conf to 127.0.0.1
// and verifies resolution still works through the new path.
func pointHostResolverAtTechnitium(rc *RunCtx) error {
	resolv := filepath.Join(hostEtc(), "resolv.conf")
	rc.Log("Pointing the host resolver at Technitium (127.0.0.1).")
	content := fmt.Sprintf("# %s. Removed when technitium is removed.\nnameserver 127.0.0.1\nsearch %s\n",
		technitiumResolvMarker, rc.Env["SEARCH_DOMAIN"])
	os.Remove(resolv) // may be a symlink to systemd-resolved's file
	if err := os.WriteFile(resolv, []byte(content), 0o644); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := waitExternalResolution(ctx, 3, 2*time.Second); err != nil {
		return fmt.Errorf("host DNS resolution is broken after pointing resolv.conf at Technitium: %w", err)
	}
	return nil
}

// restoreHostResolver points resolv.conf back at systemd-resolved's full
// resolver file when the marker is present (the stub listener stays disabled;
// install.sh owns that drop-in).
func restoreHostResolver(rc *RunCtx) {
	resolv := filepath.Join(hostEtc(), "resolv.conf")
	b, err := os.ReadFile(resolv)
	if err != nil || !strings.Contains(string(b), technitiumResolvMarker) {
		return
	}
	rc.Log("Restoring the host resolver to systemd-resolved.")
	os.Remove(resolv)
	if err := os.Symlink("/run/systemd/resolve/resolv.conf", resolv); err != nil {
		rc.Log("NOTICE: could not restore %s: %v (fix it manually)", resolv, err)
	}
}


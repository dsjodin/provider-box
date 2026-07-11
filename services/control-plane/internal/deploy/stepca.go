package deploy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// This file holds the step-ca helpers shared by every certificate-consuming
// deployer: readiness gating, the pinned-HTTPS probe (the Go equivalent of
// curl --resolve fqdn:port:127.0.0.1 --cacert root_ca.crt), leaf issuance via
// the step CLI container, and native cert-identity matching.

// caPasswordFile resolves CA_PASSWORD_FILE with the bash default of
// ${CA_DATA_DIR}/secrets/password.txt.
func caPasswordFile(env map[string]string) string {
	if v := env["CA_PASSWORD_FILE"]; v != "" {
		return v
	}
	return filepath.Join(env["CA_DATA_DIR"], "secrets", "password.txt")
}

// caPgpassFile resolves CA_PGPASSFILE with the bash default of
// ${CA_DATA_DIR}/secrets/pgpass.
func caPgpassFile(env map[string]string) string {
	if v := env["CA_PGPASSFILE"]; v != "" {
		return v
	}
	return filepath.Join(env["CA_DATA_DIR"], "secrets", "pgpass")
}

// pinnedHTTPSClient trusts only the step-ca root and resolves every host to
// 127.0.0.1, matching the single-node design where the CA and all services
// answer on the loopback regardless of DNS state.
func pinnedHTTPSClient(caRootPath string) (*http.Client, error) {
	rootPEM, err := os.ReadFile(caRootPath)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(rootPEM) {
		return nil, fmt.Errorf("no certificates parsed from %s", caRootPath)
	}
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			_, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
		},
	}
	return &http.Client{Transport: transport, Timeout: 10 * time.Second}, nil
}

// WaitHTTPSPinned polls url (whose host is pinned to 127.0.0.1) until it
// answers with a status < 500, trusting only the step-ca root.
func WaitHTTPSPinned(ctx context.Context, url, caRootPath string, attempts int, interval time.Duration) error {
	client, err := pinnedHTTPSClient(caRootPath)
	if err != nil {
		return err
	}
	var lastErr error
	for i := 0; i < attempts; i++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode < 500 {
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	return fmt.Errorf("%s did not become ready: %w", url, lastErr)
}

// requireCAReady is the shared gate every certificate-consuming deployer runs
// first (the port of the require_ca_ready_for_* functions).
func requireCAReady(ctx context.Context, env map[string]string) error {
	dataDir := env["CA_DATA_DIR"]
	for _, f := range []string{
		filepath.Join(dataDir, "config", "ca.json"),
		filepath.Join(dataDir, "certs", "root_ca.crt"),
		filepath.Join(dataDir, "certs", "intermediate_ca.crt"),
		caPasswordFile(env),
	} {
		if _, err := os.Stat(f); err != nil {
			return fmt.Errorf("step-ca is not initialized (%s missing); deploy ca first", f)
		}
	}
	url := fmt.Sprintf("https://%s:%s/roots.pem", env["CA_FQDN"], env["CA_PORT"])
	return WaitHTTPSPinned(ctx, url, filepath.Join(dataDir, "certs", "root_ca.crt"), 3, 2*time.Second)
}

// IssueCert issues a leaf for fqdn from step-ca via the step CLI container,
// writing <prefix>.crt (guaranteed full chain: leaf + intermediate) and
// <prefix>.key into certDir owned by uid 1000, the shared idiom every service
// module used. An existing valid cert for the same identity is reused.
func IssueCert(ctx context.Context, rc *RunCtx, fqdn, certDir, prefix string) error {
	env := rc.Env
	certFile := filepath.Join(certDir, prefix+".crt")
	keyFile := filepath.Join(certDir, prefix+".key")

	if err := EnsureDir(certDir, 0o755, 1000, 1000); err != nil {
		return err
	}
	if CertMatchesDNSIdentity(certFile, keyFile, fqdn) {
		rc.Log("Reusing existing certificate for %s.", fqdn)
		return nil
	}
	if _, err := os.Stat(certFile); err == nil {
		rc.Log("Existing certificate is not valid for %s; issuing a replacement.", fqdn)
	} else {
		rc.Log("Issuing certificate for %s.", fqdn)
	}
	os.Remove(certFile)
	os.Remove(keyFile)

	dataDir := env["CA_DATA_DIR"]
	passwordInContainer := "/home/step/" + strings.TrimPrefix(caPasswordFile(env), dataDir+"/")
	runner := Compose{Out: func(line string) { rc.Log("%s", line) }}
	err := runner.RunRM(ctx,
		"--network", "host",
		"--add-host", env["CA_FQDN"]+":127.0.0.1",
		"-v", dataDir+":/home/step",
		"-v", certDir+":/certs",
		env["CA_IMAGE"],
		"step", "ca", "certificate", fqdn,
		"/certs/"+prefix+".crt", "/certs/"+prefix+".key",
		"--san", fqdn,
		"--not-after", env["SERVICE_CERT_DURATION"],
		"--issuer", env["CA_PROVISIONER_NAME"],
		"--provisioner-password-file", passwordInContainer,
		"--ca-url", fmt.Sprintf("https://%s:%s", env["CA_FQDN"], env["CA_PORT"]),
		"--root", "/home/step/certs/root_ca.crt",
	)
	if err != nil {
		return fmt.Errorf("issue certificate for %s: %w", fqdn, err)
	}

	// Guarantee a FULL chain (leaf + intermediate): the served cert must
	// validate against the step-ca root on its own. A leaf-only cert bit the
	// dashboard during a CA rebuild.
	crt, err := os.ReadFile(certFile)
	if err != nil {
		return err
	}
	if bytes.Count(crt, []byte("BEGIN CERTIFICATE")) < 2 {
		intermediate, err := os.ReadFile(filepath.Join(dataDir, "certs", "intermediate_ca.crt"))
		if err != nil {
			return fmt.Errorf("%s has no intermediate and the CA intermediate is unreadable: %w", certFile, err)
		}
		if err := os.WriteFile(certFile, append(crt, intermediate...), 0o644); err != nil {
			return err
		}
		rc.Log("Appended the step-ca intermediate to %s (leaf + intermediate).", prefix+".crt")
	}
	for _, f := range []string{certFile, keyFile} {
		if err := os.Chown(f, 1000, 1000); err != nil {
			return err
		}
	}
	if err := os.Chmod(certFile, 0o644); err != nil {
		return err
	}
	return os.Chmod(keyFile, 0o600)
}

// CertMatchesDNSIdentity reports whether certFile/keyFile form a currently
// valid pair whose SANs include fqdn: the native port of
// certificate_matches_dns_identity (not expired, public keys match, SAN
// contains the FQDN).
func CertMatchesDNSIdentity(certFile, keyFile, fqdn string) bool {
	certPEM, err := os.ReadFile(certFile)
	if err != nil {
		return false
	}
	keyPEM, err := os.ReadFile(keyFile)
	if err != nil {
		return false
	}
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil { // pubkey/privkey match
		return false
	}
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return false
	}
	leaf, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return false
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return false
	}
	return leaf.VerifyHostname(fqdn) == nil
}

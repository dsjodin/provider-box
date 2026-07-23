package deploy

import (
	"bytes"
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SignCSR signs a PEM-encoded certificate signing request with step-ca via the
// step CLI container and returns the signed certificate as a full chain (leaf +
// intermediate) PEM. It reuses the same provisioner, password file, and
// full-chain guarantee as IssueCert; the difference is that the caller supplies
// the CSR (key never leaves the requester) instead of step-ca generating the
// key pair.
func SignCSR(ctx context.Context, env map[string]string, csrPEM []byte) ([]byte, error) {
	block, _ := pem.Decode(csrPEM)
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("input is not a PEM-encoded certificate signing request")
	}
	if _, err := x509.ParseCertificateRequest(block.Bytes); err != nil {
		return nil, fmt.Errorf("invalid certificate signing request: %w", err)
	}

	dataDir := env["CA_DATA_DIR"]
	root := filepath.Join(dataDir, "certs", "root_ca.crt")
	if _, err := os.Stat(root); err != nil {
		return nil, fmt.Errorf("step-ca is not initialized (%s missing); deploy ca first", root)
	}

	// The step container runs against the host daemon, so the request/output
	// files must live on a host-mounted path (under WORKDIR) it can bind in.
	workRoot := filepath.Join(env["WORKDIR"], "csr-sign")
	if err := EnsureDir(workRoot, 0o755, 1000, 1000); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp(workRoot, "req-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := os.Chown(dir, 1000, 1000); err != nil {
		return nil, err
	}
	csrPath := filepath.Join(dir, "request.csr")
	crtPath := filepath.Join(dir, "signed.crt")
	if err := os.WriteFile(csrPath, csrPEM, 0o644); err != nil {
		return nil, err
	}
	if err := os.Chown(csrPath, 1000, 1000); err != nil {
		return nil, err
	}

	passwordInContainer := "/home/step/" + strings.TrimPrefix(caPasswordFile(env), dataDir+"/")
	var out strings.Builder
	runner := Compose{Out: func(line string) { out.WriteString(line + "\n") }}
	err = runner.RunRM(ctx,
		"--network", "host",
		"--add-host", env["CA_FQDN"]+":127.0.0.1",
		"-v", dataDir+":/home/step",
		"-v", dir+":/csr",
		env["CA_IMAGE"],
		"step", "ca", "sign", "/csr/request.csr", "/csr/signed.crt",
		"--not-after", env["SERVICE_CERT_DURATION"],
		"--issuer", env["CA_PROVISIONER_NAME"],
		"--provisioner-password-file", passwordInContainer,
		"--ca-url", fmt.Sprintf("https://%s:%s", env["CA_FQDN"], env["CA_PORT"]),
		"--root", "/home/step/certs/root_ca.crt",
	)
	if err != nil {
		return nil, fmt.Errorf("sign csr: %w: %s", err, strings.TrimSpace(out.String()))
	}

	crt, err := os.ReadFile(crtPath)
	if err != nil {
		return nil, fmt.Errorf("read signed certificate: %w", err)
	}

	// Guarantee a full chain (leaf + intermediate) like IssueCert, so the
	// returned cert validates against the step-ca root on its own.
	if bytes.Count(crt, []byte("BEGIN CERTIFICATE")) < 2 {
		intermediate, err := os.ReadFile(filepath.Join(dataDir, "certs", "intermediate_ca.crt"))
		if err != nil {
			return nil, fmt.Errorf("signed cert has no intermediate and the CA intermediate is unreadable: %w", err)
		}
		crt = append(crt, intermediate...)
	}
	return crt, nil
}

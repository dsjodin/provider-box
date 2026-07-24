package deploy

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// Traefik deploys the single-ingress reverse proxy. It terminates TLS with one
// step-ca-issued *.<SEARCH_DOMAIN> wildcard leaf and routes each service by its
// bare FQDN on :443, so operators reach services without remembering ports.
// Bridge service stacks are discovered via docker labels on the shared "proxy"
// network (created by install.sh); the host-networked control plane and certsrv
// are wired through Traefik's file provider. Lab posture: Traefik talks plain
// HTTP to backends over the proxy network.
type Traefik struct{}

func (Traefik) Name() string   { return "traefik" }
func (Traefik) Deps() []string { return []string{"ca"} }

func (t Traefik) Deploy(ctx context.Context, rc *RunCtx) error {
	env := rc.Env
	runtime := rc.Workdir("traefik")
	certDir := filepath.Join(env["TRAEFIK_DIR"], "certs")

	if err := requireCAReady(ctx, env); err != nil {
		return err
	}

	// The file-provider routers reach the host-networked control plane and
	// certsrv by host IP and port; derive the control-plane port from its addr.
	_, cpPort, err := net.SplitHostPort(env["CONTROL_PLANE_ADDR"])
	if err != nil {
		return fmt.Errorf("CONTROL_PLANE_ADDR %q: %w", env["CONTROL_PLANE_ADDR"], err)
	}
	env["CONTROL_PLANE_PORT"] = cpPort

	for _, dir := range []string{runtime, filepath.Join(runtime, "dynamic"), certDir} {
		if err := EnsureDir(dir, 0o755, -1, -1); err != nil {
			return err
		}
	}

	// One wildcard leaf covers every one-label service name under the domain.
	if err := IssueCert(ctx, rc, "*."+env["SEARCH_DOMAIN"], certDir, "wildcard"); err != nil {
		return err
	}

	// Dashboard basic-auth users file (APR1, understood by Traefik's basicAuth).
	line, err := htpasswdLine(env["TRAEFIK_DASHBOARD_USER"], env["TRAEFIK_DASHBOARD_PASSWORD"])
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(runtime, "usersfile"), []byte(line+"\n"), 0o644); err != nil {
		return err
	}

	if err := Render("traefik.yml.tpl", env, filepath.Join(runtime, "traefik.yml"), 0o644); err != nil {
		return err
	}
	if err := Render("traefik-dynamic.yml.tpl", env, filepath.Join(runtime, "dynamic", "dynamic.yml"), 0o644); err != nil {
		return err
	}
	if err := Render("docker-compose.traefik.yml.tpl", env, runtime+"/docker-compose.yml", 0o644); err != nil {
		return err
	}

	cmp := rc.Compose("traefik")
	if err := cmp.Down(ctx); err != nil {
		return err
	}
	if err := cmp.Up(ctx); err != nil {
		return err
	}

	rc.Log("Waiting for Traefik to serve the wildcard leaf on :443.")
	caRoot := filepath.Join(env["CA_DATA_DIR"], "certs", "root_ca.crt")
	url := fmt.Sprintf("https://%s/", env["TRAEFIK_FQDN"])
	if err := WaitHTTPSPinned(ctx, url, caRoot, 60, 2*time.Second); err != nil {
		return err
	}
	rc.Log("Traefik is ready; dashboard at https://%s. Services are reachable by FQDN on :443.", env["TRAEFIK_FQDN"])
	return nil
}

func (t Traefik) Remove(ctx context.Context, rc *RunCtx) error {
	cmp := rc.Compose("traefik")
	if err := cmp.Down(ctx); err != nil {
		return err
	}
	if err := os.RemoveAll(rc.Workdir("traefik")); err != nil {
		return err
	}
	rc.Log("Removed Traefik containers and runtime files. The wildcard certificate in %s was preserved.",
		filepath.Join(rc.Env["TRAEFIK_DIR"], "certs"))
	return nil
}

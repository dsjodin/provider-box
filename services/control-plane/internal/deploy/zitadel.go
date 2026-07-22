package deploy

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Zitadel deploys the Zitadel identity provider (OIDC), a sibling of the
// Keycloak and Authentik deployers. Zitadel v4 uses the decoupled Login V2 UI,
// so the stack is four containers: a Postgres backend, the core server (plain
// HTTP), the login container, and an nginx terminator that serves the
// step-ca-issued certificate and routes /ui/v2/login to the login container and
// everything else to the core. Zitadel's FirstInstance init mints an admin
// service account (whose PAT the post-deploy step reads to provision the
// bootstrap project, OIDC application, lab user, and role via the Management
// API) and the login-client service account (whose PAT the login container
// authenticates with).
type Zitadel struct{}

func (Zitadel) Name() string   { return "zitadel" }
func (Zitadel) Deps() []string { return []string{"ca"} }

func (z Zitadel) Deploy(ctx context.Context, rc *RunCtx) error {
	env := rc.Env
	runtime := rc.Workdir("zitadel")

	if len(env["ZITADEL_MASTERKEY"]) != 32 {
		return fmt.Errorf("ZITADEL_MASTERKEY must be exactly 32 characters long")
	}
	for _, name := range []string{"ZITADEL_ADMIN_PASSWORD", "ZITADEL_PG_PASSWORD", "ZITADEL_BOOTSTRAP_USER_PASSWORD"} {
		if strings.ContainsAny(env[name], `"\`) {
			return fmt.Errorf("%s must not contain double quotes or backslashes", name)
		}
	}
	if _, err := zitadelRedirectURIs(env); err != nil {
		return err
	}
	if err := requireCAReady(ctx, env); err != nil {
		return err
	}

	certDir := filepath.Join(env["ZITADEL_DIR"], "certs", env["ZITADEL_FQDN"])
	machinekey := filepath.Join(runtime, "machinekey")
	for _, dir := range []string{runtime, env["ZITADEL_DIR"], certDir} {
		if err := EnsureDir(dir, 0o755, -1, -1); err != nil {
			return err
		}
	}
	if err := EnsureDir(filepath.Join(env["ZITADEL_DIR"], "postgres"), 0o700, -1, -1); err != nil {
		return err
	}
	if err := chownR(filepath.Join(env["ZITADEL_DIR"], "postgres"), 70, 70); err != nil {
		return err
	}
	// The Zitadel container runs as uid 1000 and writes the admin and
	// login-client PATs into this directory, so it must be owned by that uid.
	if err := EnsureDir(machinekey, 0o755, 1000, 1000); err != nil {
		return err
	}

	if err := IssueCert(ctx, rc, env["ZITADEL_FQDN"], certDir, "zitadel"); err != nil {
		return err
	}

	if err := Render("docker-compose.zitadel.yml.tpl", env, runtime+"/docker-compose.yml", 0o644); err != nil {
		return err
	}
	if err := Render("zitadel-nginx.conf.tpl", env, runtime+"/nginx.conf", 0o644); err != nil {
		return err
	}
	cmp := rc.Compose("zitadel")
	if err := cmp.Down(ctx); err != nil {
		return err
	}
	if err := cmp.Up(ctx); err != nil {
		return err
	}

	api, err := newZitadelAPI(env)
	if err != nil {
		return err
	}
	readyURL := api.base() + "/debug/ready"
	rc.Log("Waiting for Zitadel at %s.", readyURL)
	caRoot := filepath.Join(env["CA_DATA_DIR"], "certs", "root_ca.crt")
	if err := WaitHTTPSPinned(ctx, readyURL, caRoot, 90, 2*time.Second); err != nil {
		return fmt.Errorf("Zitadel did not become ready (check that it serves the step-ca certificate): %w", err)
	}

	pat, err := readZitadelPAT(ctx, filepath.Join(machinekey, "pat.txt"))
	if err != nil {
		return err
	}
	api.token = pat
	if err := provisionZitadel(ctx, rc, api); err != nil {
		return err
	}

	rc.Log("Zitadel is ready at %s (Login V2 UI at /ui/v2/login, OIDC discovery under /.well-known/openid-configuration).", api.base())
	return nil
}

func (z Zitadel) Remove(ctx context.Context, rc *RunCtx) error {
	cmp := rc.Compose("zitadel")
	if err := cmp.Down(ctx); err != nil {
		return err
	}
	if err := os.RemoveAll(rc.Workdir("zitadel")); err != nil {
		return err
	}
	rc.Log("Removed Zitadel containers and runtime files. Persistent data in %s was preserved.", rc.Env["ZITADEL_DIR"])
	return nil
}

// zitadelRedirectURIs validates and returns the bootstrap client redirect URIs.
func zitadelRedirectURIs(env map[string]string) ([]string, error) {
	var uris []string
	for _, uri := range strings.Split(env["ZITADEL_BOOTSTRAP_CLIENT_REDIRECT_URIS"], ",") {
		if uri == "" {
			return nil, fmt.Errorf("ZITADEL_BOOTSTRAP_CLIENT_REDIRECT_URIS contains an empty entry")
		}
		if !strings.HasPrefix(uri, "https://") {
			return nil, fmt.Errorf("ZITADEL_BOOTSTRAP_CLIENT_REDIRECT_URIS entries must start with https://: %s", uri)
		}
		uris = append(uris, uri)
	}
	if len(uris) == 0 {
		return nil, fmt.Errorf("ZITADEL_BOOTSTRAP_CLIENT_REDIRECT_URIS must not be empty")
	}
	return uris, nil
}

// readZitadelPAT waits for Zitadel's FirstInstance init to write the machine
// user's personal access token, then returns it.
func readZitadelPAT(ctx context.Context, path string) (string, error) {
	for i := 0; i < 45; i++ {
		if b, err := os.ReadFile(path); err == nil {
			if tok := strings.TrimSpace(string(b)); tok != "" {
				return tok, nil
			}
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return "", fmt.Errorf("Zitadel did not write the machine-user PAT to %s; check the server logs", path)
}

// zitadelAPI wraps the Management API calls: the FQDN is pinned to 127.0.0.1
// and the served certificate is verified against the step-ca root.
type zitadelAPI struct {
	env    map[string]string
	token  string
	caPool *x509.CertPool
}

func newZitadelAPI(env map[string]string) (*zitadelAPI, error) {
	root, err := os.ReadFile(filepath.Join(env["CA_DATA_DIR"], "certs", "root_ca.crt"))
	if err != nil {
		return nil, fmt.Errorf("read step-ca root: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(root) {
		return nil, fmt.Errorf("step-ca root is not valid PEM")
	}
	return &zitadelAPI{env: env, caPool: pool}, nil
}

func (a *zitadelAPI) base() string {
	return fmt.Sprintf("https://%s:%s", a.env["ZITADEL_FQDN"], a.env["ZITADEL_PORT"])
}

func (a *zitadelAPI) client() *http.Client {
	dialer := &net.Dialer{Timeout: 3 * time.Second}
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: a.caPool, ServerName: a.env["ZITADEL_FQDN"]},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				_, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				return dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", port))
			},
		},
	}
}

func (a *zitadelAPI) do(ctx context.Context, method, path string, payload any) (int, []byte, error) {
	var body io.Reader
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return 0, nil, err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base()+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+a.token)
	req.Header.Set("Accept", "application/json")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := a.client().Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, b, nil
}

// provisionZitadel creates the bootstrap project, OIDC application, lab user,
// and project role via the Management API. Every step tolerates a pre-existing
// object (HTTP 409) so a re-run is idempotent; the OIDC client credentials,
// which Zitadel returns only on creation, are written to a file the first time
// and that file guards against re-creating the application.
func provisionZitadel(ctx context.Context, rc *RunCtx, api *zitadelAPI) error {
	env := rc.Env
	rc.Log("Provisioning the Zitadel bootstrap project and OIDC client.")

	projectID, err := ensureZitadelProject(ctx, api, env["ZITADEL_BOOTSTRAP_CLIENT_ID"])
	if err != nil {
		return err
	}

	role := env["ZITADEL_BOOTSTRAP_GROUP_NAME"]
	status, _, err := api.do(ctx, http.MethodPost,
		"/management/v1/projects/"+projectID+"/roles",
		map[string]string{"roleKey": role, "displayName": role})
	if err != nil {
		return err
	}
	if !okOrExists(status) {
		return fmt.Errorf("create Zitadel project role %q: HTTP %d", role, status)
	}

	clientFile := filepath.Join(env["ZITADEL_DIR"], "certs", env["ZITADEL_FQDN"], "zitadel-oidc-client.txt")
	if _, err := os.Stat(clientFile); err == nil {
		rc.Log("Reusing existing Zitadel OIDC client (credentials in %s).", clientFile)
	} else {
		redirectURIs, _ := zitadelRedirectURIs(env)
		status, body, err := api.do(ctx, http.MethodPost,
			"/management/v1/projects/"+projectID+"/apps/oidc",
			map[string]any{
				"name":            env["ZITADEL_BOOTSTRAP_CLIENT_ID"],
				"redirectUris":    redirectURIs,
				"responseTypes":   []string{"OIDC_RESPONSE_TYPE_CODE"},
				"grantTypes":      []string{"OIDC_GRANT_TYPE_AUTHORIZATION_CODE"},
				"appType":         "OIDC_APP_TYPE_WEB",
				"authMethodType":  "OIDC_AUTH_METHOD_TYPE_BASIC",
				"accessTokenType": "OIDC_TOKEN_TYPE_JWT",
			})
		if err != nil {
			return err
		}
		if status != http.StatusOK && status != http.StatusCreated {
			return fmt.Errorf("create Zitadel OIDC application: HTTP %d: %.300s", status, body)
		}
		clientID := firstResult(body, "clientId")
		clientSecret := firstResult(body, "clientSecret")
		content := fmt.Sprintf("issuer=%s\nclient_id=%s\nclient_secret=%s\n", api.base(), clientID, clientSecret)
		if err := os.WriteFile(clientFile, []byte(content), 0o600); err != nil {
			return err
		}
		rc.Log("Zitadel OIDC client created; credentials written to %s (Zitadel generates the client id/secret, so use these for VCF SSO).", clientFile)
	}

	userID, err := ensureZitadelUser(ctx, api, env)
	if err != nil {
		return err
	}
	if userID != "" {
		status, _, err = api.do(ctx, http.MethodPost,
			"/management/v1/users/"+userID+"/grants",
			map[string]any{"projectId": projectID, "roleKeys": []string{role}})
		if err != nil {
			return err
		}
		if !okOrExists(status) {
			return fmt.Errorf("grant Zitadel role %q to the lab user: HTTP %d", role, status)
		}
	}
	return nil
}

func ensureZitadelProject(ctx context.Context, api *zitadelAPI, name string) (string, error) {
	status, body, err := api.do(ctx, http.MethodPost, "/management/v1/projects", map[string]string{"name": name})
	if err != nil {
		return "", err
	}
	if status == http.StatusOK || status == http.StatusCreated {
		if id := firstResult(body, "id"); id != "" {
			return id, nil
		}
		return "", fmt.Errorf("Zitadel project created but no id was returned: %.300s", body)
	}
	if status != http.StatusConflict {
		return "", fmt.Errorf("create Zitadel project: HTTP %d: %.300s", status, body)
	}
	// Already exists: look it up by name.
	status, body, err = api.do(ctx, http.MethodPost, "/management/v1/projects/_search",
		map[string]any{"queries": []map[string]any{
			{"nameQuery": map[string]string{"name": name, "method": "TEXT_QUERY_METHOD_EQUALS"}},
		}})
	if err != nil {
		return "", err
	}
	if id := firstResult(body, "id"); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("Zitadel project %q exists but could not be located (HTTP %d)", name, status)
}

func ensureZitadelUser(ctx context.Context, api *zitadelAPI, env map[string]string) (string, error) {
	username := env["ZITADEL_BOOTSTRAP_USERNAME"]
	status, body, err := api.do(ctx, http.MethodPost, "/management/v1/users/human",
		map[string]any{
			"userName": username,
			"profile": map[string]string{
				"firstName": username,
				"lastName":  "lab",
			},
			"email": map[string]any{
				"email":           username + "@" + env["ZITADEL_BOOTSTRAP_USER_EMAIL_DOMAIN"],
				"isEmailVerified": true,
			},
			"password": env["ZITADEL_BOOTSTRAP_USER_PASSWORD"],
		})
	if err != nil {
		return "", err
	}
	if status == http.StatusOK || status == http.StatusCreated {
		return firstResult(body, "userId"), nil
	}
	if status != http.StatusConflict {
		return "", fmt.Errorf("create Zitadel lab user: HTTP %d: %.300s", status, body)
	}
	status, body, err = api.do(ctx, http.MethodPost, "/management/v1/users/_search",
		map[string]any{"queries": []map[string]any{
			{"userNameQuery": map[string]string{"userName": username, "method": "TEXT_QUERY_METHOD_EQUALS"}},
		}})
	if err != nil {
		return "", err
	}
	return firstResult(body, "id"), nil
}

func okOrExists(status int) bool {
	return status == http.StatusOK || status == http.StatusCreated || status == http.StatusConflict
}

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
	// The machine-user PATs are only written by Zitadel's first-instance init,
	// so they must persist alongside the database (both under ZITADEL_DIR, which
	// Remove preserves) - not under the disposable runtime dir, which Remove
	// wipes and would desync the PATs from an existing instance on redeploy.
	machinekey := filepath.Join(env["ZITADEL_DIR"], "machinekey")
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
	// /debug/ready is served by the HTTP layer directly; the Management API
	// proxies through Zitadel's internal grpc-gateway to its own gRPC backend
	// over loopback, which can refuse the connection for a few seconds longer.
	// Gate provisioning on a real authenticated API call (as the Authentik
	// deployer does) so a transient gRPC "connection refused" is not fatal.
	rc.Log("Waiting for the Zitadel Management API to accept the admin token.")
	if err := api.waitAPIReady(ctx, 45, 2*time.Second); err != nil {
		return err
	}
	if err := provisionZitadel(ctx, rc, api); err != nil {
		return err
	}
	logZitadelAdminLogin(ctx, rc, api)

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
	machinekeyDir := filepath.Dir(path)
	postgresDir := filepath.Join(filepath.Dir(machinekeyDir), "postgres")
	return "", fmt.Errorf("Zitadel did not write the machine-user PAT to %s. It is written only during a first-instance init on an empty database; if the database already holds an instance without the PAT (e.g. an interrupted first init), stop the stack and remove %s and %s, then redeploy to re-initialize (lab data is disposable)", path, postgresDir, machinekeyDir)
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
	return a.doOrg(ctx, method, path, "", payload)
}

// doOrg is do scoped to an organization: when orgID is non-empty it sets the
// x-zitadel-orgid header so the call operates in that org instead of the
// token's own (default) org.
func (a *zitadelAPI) doOrg(ctx context.Context, method, path, orgID string, payload any) (int, []byte, error) {
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
	if orgID != "" {
		req.Header.Set("x-zitadel-orgid", orgID)
	}
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

// waitAPIReady polls an authenticated Management API endpoint until it answers
// 200. This exercises the same grpc-gateway -> gRPC path provisioning uses, so
// it clears the window where /debug/ready is up but the internal gRPC backend
// still refuses the loopback connection (gRPC code 14).
func (a *zitadelAPI) waitAPIReady(ctx context.Context, attempts int, interval time.Duration) error {
	var last string
	for i := 0; i < attempts; i++ {
		status, body, err := a.do(ctx, http.MethodGet, "/management/v1/orgs/me", nil)
		if err == nil && status == http.StatusOK {
			return nil
		}
		if err != nil {
			last = err.Error()
		} else {
			last = fmt.Sprintf("HTTP %d: %.200s", status, body)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
	return fmt.Errorf("Zitadel Management API did not become ready (last: %s); if it keeps failing with a gRPC \"dial tcp [::1]:8080: connect: connection refused\", the core container cannot reach its own gRPC backend over IPv6 loopback - enable IPv6 loopback in the container", last)
}

// provisionZitadel seeds the VCF-SSO objects. When ZITADEL_TENANTS is set, each
// name becomes an isolated organization with its own project, OIDC client,
// role, and lab user (credentials written to zitadel-oidc-<name>.txt); when it
// is empty a single set is seeded in the default org for backward
// compatibility. Every step tolerates a pre-existing object (HTTP 409) so a
// re-run is idempotent.
func provisionZitadel(ctx context.Context, rc *RunCtx, api *zitadelAPI) error {
	tenants := splitTenants(rc.Env["ZITADEL_TENANTS"])
	if len(tenants) == 0 {
		rc.Log("Provisioning the Zitadel bootstrap project and OIDC client in the default org.")
		return provisionZitadelOrg(ctx, rc, api, "", "")
	}
	for _, name := range tenants {
		orgID, err := ensureZitadelOrg(ctx, api, name)
		if err != nil {
			return err
		}
		rc.Log("Provisioning Zitadel tenant org %q (id %s).", name, orgID)
		if err := provisionZitadelOrg(ctx, rc, api, orgID, name); err != nil {
			return err
		}
	}
	return nil
}

// splitTenants parses the comma-separated ZITADEL_TENANTS list, trimming blanks.
func splitTenants(csv string) []string {
	var out []string
	for _, t := range strings.Split(csv, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

// provisionZitadelOrg seeds one org (orgID empty means the token's default org)
// with the bootstrap project, role, OIDC client, and lab user, writing the
// generated client credentials and the org login scope to a per-org file.
func provisionZitadelOrg(ctx context.Context, rc *RunCtx, api *zitadelAPI, orgID, label string) error {
	env := rc.Env

	projectID, err := ensureZitadelProject(ctx, api, orgID, env["ZITADEL_BOOTSTRAP_CLIENT_ID"])
	if err != nil {
		return err
	}

	role := env["ZITADEL_BOOTSTRAP_GROUP_NAME"]
	status, _, err := api.doOrg(ctx, http.MethodPost,
		"/management/v1/projects/"+projectID+"/roles", orgID,
		map[string]string{"roleKey": role, "displayName": role})
	if err != nil {
		return err
	}
	if !okOrExists(status) {
		return fmt.Errorf("create Zitadel project role %q: HTTP %d", role, status)
	}

	clientFile := zitadelClientFile(env, label)
	if _, err := os.Stat(clientFile); err == nil {
		rc.Log("Reusing existing Zitadel OIDC client (credentials in %s).", clientFile)
	} else {
		redirectURIs, _ := zitadelRedirectURIs(env)
		status, body, err := api.doOrg(ctx, http.MethodPost,
			"/management/v1/projects/"+projectID+"/apps/oidc", orgID,
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
		content := zitadelClientFileContent(api.base(), orgID, label, clientID, clientSecret)
		if err := os.WriteFile(clientFile, []byte(content), 0o600); err != nil {
			return err
		}
		rc.Log("Zitadel OIDC client created for %s; credentials written to %s (Zitadel generates the client id/secret, so use these for VCF SSO).",
			zitadelOrgLabel(label), clientFile)
	}

	userID, err := ensureZitadelUser(ctx, api, orgID, env)
	if err != nil {
		return err
	}
	if userID != "" {
		status, _, err = api.doOrg(ctx, http.MethodPost,
			"/management/v1/users/"+userID+"/grants", orgID,
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

// ensureZitadelOrg finds (or creates) an organization by name and returns its
// id. Org names are not unique in Zitadel (only domains are), so the lookup
// runs first to keep the operation idempotent.
func ensureZitadelOrg(ctx context.Context, api *zitadelAPI, name string) (string, error) {
	status, body, err := api.do(ctx, http.MethodPost, "/admin/v1/orgs/_search",
		map[string]any{"queries": []map[string]any{
			{"nameQuery": map[string]string{"name": name, "method": "TEXT_QUERY_METHOD_EQUALS"}},
		}})
	if err != nil {
		return "", err
	}
	if status == http.StatusOK {
		if id := firstResult(body, "id"); id != "" {
			return id, nil
		}
	}
	status, body, err = api.do(ctx, http.MethodPost, "/management/v1/orgs", map[string]string{"name": name})
	if err != nil {
		return "", err
	}
	if status != http.StatusOK && status != http.StatusCreated {
		return "", fmt.Errorf("create Zitadel org %q: HTTP %d: %.300s", name, status, body)
	}
	if id := firstResult(body, "id"); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("Zitadel org %q created but no id was returned: %.300s", name, body)
}

// zitadelClientFile is the per-org OIDC credential file. The default org (empty
// label) keeps the original name so existing deployments are unaffected.
func zitadelClientFile(env map[string]string, label string) string {
	dir := filepath.Join(env["ZITADEL_DIR"], "certs", env["ZITADEL_FQDN"])
	if label == "" {
		return filepath.Join(dir, "zitadel-oidc-client.txt")
	}
	return filepath.Join(dir, "zitadel-oidc-"+sanitizeFileLabel(label)+".txt")
}

// zitadelClientFileContent renders the VCF-SSO federation details, including the
// org login scope so the VCF OIDC request can pin sign-in to this tenant.
func zitadelClientFileContent(issuer, orgID, label, clientID, clientSecret string) string {
	var b strings.Builder
	if label != "" {
		fmt.Fprintf(&b, "tenant=%s\n", label)
	}
	if orgID != "" {
		fmt.Fprintf(&b, "org_id=%s\n", orgID)
		fmt.Fprintf(&b, "org_scope=urn:zitadel:iam:org:id:%s\n", orgID)
	}
	fmt.Fprintf(&b, "issuer=%s\n", issuer)
	fmt.Fprintf(&b, "client_id=%s\n", clientID)
	fmt.Fprintf(&b, "client_secret=%s\n", clientSecret)
	return b.String()
}

func zitadelOrgLabel(label string) string {
	if label == "" {
		return "the default org"
	}
	return "tenant " + label
}

// sanitizeFileLabel makes a filesystem-safe slug from an org name.
func sanitizeFileLabel(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}

func ensureZitadelProject(ctx context.Context, api *zitadelAPI, orgID, name string) (string, error) {
	status, body, err := api.doOrg(ctx, http.MethodPost, "/management/v1/projects", orgID, map[string]string{"name": name})
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
	status, body, err = api.doOrg(ctx, http.MethodPost, "/management/v1/projects/_search", orgID,
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

func ensureZitadelUser(ctx context.Context, api *zitadelAPI, orgID string, env map[string]string) (string, error) {
	username := env["ZITADEL_BOOTSTRAP_USERNAME"]
	status, body, err := api.doOrg(ctx, http.MethodPost, "/management/v1/users/human", orgID,
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
	status, body, err = api.doOrg(ctx, http.MethodPost, "/management/v1/users/_search", orgID,
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

// logZitadelAdminLogin logs the human admin's real login name and the Console
// URL. Zitadel appends the generated org domain to the configured username
// (e.g. provider-admin becomes provider-admin@zitadel.<fqdn>), so this saves
// operators from querying the API to discover it. Best-effort: never fatal.
func logZitadelAdminLogin(ctx context.Context, rc *RunCtx, api *zitadelAPI) {
	env := rc.Env
	loginName := ""
	_, body, err := api.do(ctx, http.MethodPost, "/management/v1/users/_search",
		map[string]any{"queries": []map[string]any{
			{"userNameQuery": map[string]string{"userName": env["ZITADEL_ADMIN_USERNAME"], "method": "TEXT_QUERY_METHOD_STARTS_WITH"}},
		}})
	if err == nil {
		loginName = firstResult(body, "preferredLoginName")
	}
	if loginName == "" {
		// Fall back to the configured username; the real login name carries the
		// generated org domain, visible in the Console user list.
		loginName = env["ZITADEL_ADMIN_USERNAME"] + " (Zitadel appends the org domain; see the Console user list for the full login name)"
	}
	rc.Log("Zitadel Console admin login name: %s (use the configured ZITADEL_ADMIN_PASSWORD) at https://%s:%s/ui/console.",
		loginName, env["ZITADEL_FQDN"], env["ZITADEL_PORT"])
}

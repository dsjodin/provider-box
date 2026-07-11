package deploy

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// CA deploys step-ca with its dedicated PostgreSQL backend, the port of
// bootstrap/ca.sh. The container self-initializes on badger at first start;
// the deployer then rewrites ca.json to the postgresql backend, enables CRL,
// restarts, and provisions the read-only role the certificates panel uses.
type CA struct{}

func (CA) Name() string   { return "ca" }
func (CA) Deps() []string { return nil }

func (c CA) Deploy(ctx context.Context, rc *RunCtx) error {
	env := rc.Env
	dataDir := env["CA_DATA_DIR"]
	passwordFile := caPasswordFile(env)
	pgpassFile := caPgpassFile(env)

	// Cross-field rules from require_ca_vars.
	if env["CA_POSTGRES_RO_USER"] == env["CA_POSTGRES_USER"] {
		return fmt.Errorf("CA_POSTGRES_RO_USER must differ from CA_POSTGRES_USER (the read-only role must not be the owner)")
	}
	if strings.HasPrefix(env["CA_POSTGRES_DATA_DIR"], dataDir+"/") {
		return fmt.Errorf("CA_POSTGRES_DATA_DIR must NOT be nested under CA_DATA_DIR (the CA_DATA_DIR chown would corrupt postgres data)")
	}
	if !strings.HasPrefix(passwordFile, dataDir+"/") {
		return fmt.Errorf("CA_PASSWORD_FILE must be located under CA_DATA_DIR so it is mounted into the container")
	}
	if !strings.HasPrefix(pgpassFile, dataDir+"/") {
		return fmt.Errorf("CA_PGPASSFILE must be located under CA_DATA_DIR so it is mounted into the container")
	}

	// Guard against a prior compose run with an empty CA_DATA_DIR having
	// auto-created root_ca.crt as a DIRECTORY; proceeding would loop on a
	// broken init and can destroy the CA.
	rootCert := filepath.Join(dataDir, "certs", "root_ca.crt")
	if fi, err := os.Stat(rootCert); err == nil && !fi.Mode().IsRegular() {
		return fmt.Errorf("step-ca root certificate %s is not a regular file (likely a bad bind mount from an empty CA_DATA_DIR); remove it and restore or re-initialize the CA", rootCert)
	}

	// Read the existing backend BEFORE compose runs. An existing badger CA is
	// refused: the postgres design rebuilds, it does not migrate in place.
	caJSON := filepath.Join(dataDir, "config", "ca.json")
	hadConfig := false
	if _, err := os.Stat(caJSON); err == nil {
		hadConfig = true
		if backend := caConfigDBType(caJSON); backend != "postgresql" {
			return fmt.Errorf("existing CA at %s uses the '%s' backend; Provider Box runs step-ca on PostgreSQL and does not migrate badger data in place. Remove %s to rebuild (lab certs are disposable), then redeploy every certificate-consuming service", dataDir, backend, dataDir)
		}
	}

	if err := EnsureDir(rc.Workdir("step-ca"), 0o755, -1, -1); err != nil {
		return err
	}
	if err := EnsureDir(dataDir, 0o755, -1, -1); err != nil {
		return err
	}
	if err := EnsureDir(filepath.Dir(passwordFile), 0o700, -1, -1); err != nil {
		return err
	}

	if _, err := os.Stat(passwordFile); err == nil {
		rc.Log("Using existing CA password file: %s", passwordFile)
	} else {
		value := env["CA_PASSWORD"]
		if value != "" {
			if strings.HasPrefix(value, "CHANGE_ME") || strings.HasPrefix(strings.ToLower(value), "change-me") {
				return fmt.Errorf("replace placeholder CA_PASSWORD before continuing")
			}
			rc.Log("Materializing CA_PASSWORD to managed file: %s", passwordFile)
		} else {
			raw := make([]byte, 32)
			if _, err := rand.Read(raw); err != nil {
				return err
			}
			value = base64.StdEncoding.EncodeToString(raw)
			rc.Log("CA password input not provided; generated one at: %s", passwordFile)
		}
		if err := os.WriteFile(passwordFile, []byte(value+"\n"), 0o600); err != nil {
			return err
		}
	}

	// libpq .pgpass so the postgres password stays out of ca.json's DSN. Only
	// the password field can contain the ':' and '\' metacharacters.
	esc := strings.ReplaceAll(env["CA_POSTGRES_PASSWORD"], `\`, `\\`)
	esc = strings.ReplaceAll(esc, ":", `\:`)
	pgpass := fmt.Sprintf("stepca-postgres:5432:%s:%s:%s\n", env["CA_POSTGRES_DB"], env["CA_POSTGRES_USER"], esc)
	if err := EnsureDir(filepath.Dir(pgpassFile), 0o700, -1, -1); err != nil {
		return err
	}
	if err := os.WriteFile(pgpassFile, []byte(pgpass), 0o600); err != nil {
		return err
	}

	// The postgres image runs as uid 70; its dir is a sibling of CA_DATA_DIR
	// so the recursive uid-1000 chown below never touches it.
	if err := EnsureDir(env["CA_POSTGRES_DATA_DIR"], 0o700, -1, -1); err != nil {
		return err
	}
	if err := chownR(env["CA_POSTGRES_DATA_DIR"], 70, 70); err != nil {
		return err
	}

	// The step-ca image runs as uid 1000; root-owned dirs keep init from
	// reading the password file.
	if err := chownR(dataDir, 1000, 1000); err != nil {
		return err
	}
	normalizeCAPasswordFiles(env, passwordFile)

	renderEnv := withDerived(env, map[string]string{
		"CA_PASSWORD_FILE_IN_CONTAINER": "/home/step/" + strings.TrimPrefix(passwordFile, dataDir+"/"),
		"CA_PGPASSFILE_IN_CONTAINER":    "/home/step/" + strings.TrimPrefix(pgpassFile, dataDir+"/"),
	})
	if err := Render("docker-compose.step-ca.yml.tpl", renderEnv, rc.Workdir("step-ca")+"/docker-compose.yml", 0o644); err != nil {
		return err
	}

	cmp := rc.Compose("step-ca")
	if err := cmp.Down(ctx); err != nil {
		return err
	}
	if err := cmp.Up(ctx); err != nil {
		return err
	}
	normalizeCAPasswordFiles(env, passwordFile)

	rc.Log("Waiting for stepca-postgres.")
	if err := waitCAPostgres(ctx, env); err != nil {
		return err
	}
	rc.Log("Waiting for step-ca to initialize.")
	if err := waitCAInit(ctx, env); err != nil {
		return err
	}

	if !hadConfig {
		// Fresh init just wrote a badger ca.json. Set the provisioner duration
		// (offline edit, DB-independent), then switch the backend to postgres
		// and enable CRL in the same restart.
		if err := configureProvisionerDuration(ctx, rc); err != nil {
			return err
		}
		if err := guardCAPostgresStoreEmpty(ctx, env); err != nil {
			return err
		}
		if err := patchCAJSONPostgresCRL(rc, caJSON, env); err != nil {
			return err
		}
		if err := cmp.docker(ctx, "compose", "restart", "step-ca"); err != nil {
			return err
		}
		if err := waitCAInit(ctx, env); err != nil {
			return err
		}
		moveBadgerDirAside(rc, dataDir)
	} else {
		if err := configureProvisionerDuration(ctx, rc); err != nil {
			return err
		}
		if err := cmp.docker(ctx, "compose", "restart", "step-ca"); err != nil {
			return err
		}
		if err := waitCAInit(ctx, env); err != nil {
			return err
		}
	}

	if err := ensureStepcaReadonlyRole(ctx, rc); err != nil {
		return err
	}

	// Issue the control plane's own leaf so it serves HTTPS after a restart
	// (main.go's TLS fallback tolerates the pre-CA window).
	if env["CONTROL_PLANE_FQDN"] != "" && env["CONTROL_PLANE_CERT_DIR"] != "" {
		if err := IssueCert(ctx, rc, env["CONTROL_PLANE_FQDN"], env["CONTROL_PLANE_CERT_DIR"], "control-plane"); err != nil {
			return err
		}
		rc.Log("Control plane certificate issued; restart the control-plane container to serve HTTPS.")
	}

	rc.Log("step-ca is ready at https://%s:%s (root certificate at /roots.pem).", env["CA_FQDN"], env["CA_PORT"])
	return nil
}

func (c CA) Remove(ctx context.Context, rc *RunCtx) error {
	cmp := rc.Compose("step-ca")
	if err := cmp.Down(ctx); err != nil {
		return err
	}
	if err := os.RemoveAll(rc.Workdir("step-ca")); err != nil {
		return err
	}
	rc.Log("Removed step-ca and stepca-postgres containers and runtime files. Persistent data in %s and %s was preserved.",
		rc.Env["CA_DATA_DIR"], rc.Env["CA_POSTGRES_DATA_DIR"])
	return nil
}

// withDerived returns a copy of env with extra keys merged (deployers never
// mutate the shared map).
func withDerived(env, extra map[string]string) map[string]string {
	out := make(map[string]string, len(env)+len(extra))
	for k, v := range env {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func chownR(root string, uid, gid int) error {
	return filepath.Walk(root, func(path string, _ os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		return os.Chown(path, uid, gid)
	})
}

func normalizeCAPasswordFiles(env map[string]string, passwordFile string) {
	for _, f := range []string{passwordFile, filepath.Join(env["CA_DATA_DIR"], "secrets", "password")} {
		if _, err := os.Stat(f); err == nil {
			_ = os.Chown(f, 1000, 1000)
			_ = os.Chmod(f, 0o600)
		}
	}
}

func caConfigDBType(caJSON string) string {
	b, err := os.ReadFile(caJSON)
	if err != nil {
		return "badger"
	}
	var cfg struct {
		DB struct {
			Type string `json:"type"`
		} `json:"db"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil || cfg.DB.Type == "" {
		return "badger"
	}
	return cfg.DB.Type
}

// caConnect opens a pgx connection to the loopback-published CA postgres as
// the owner role (the Go replacement for psql-in-container over trust auth).
func caConnect(ctx context.Context, env map[string]string) (*pgx.Conn, error) {
	u := url.URL{
		Scheme:   "postgresql",
		User:     url.UserPassword(env["CA_POSTGRES_USER"], env["CA_POSTGRES_PASSWORD"]),
		Host:     "127.0.0.1:" + env["CA_POSTGRES_PORT"],
		Path:     "/" + env["CA_POSTGRES_DB"],
		RawQuery: "sslmode=disable",
	}
	return pgx.Connect(ctx, u.String())
}

func waitCAPostgres(ctx context.Context, env map[string]string) error {
	var lastErr error
	for i := 0; i < 30; i++ {
		conn, err := caConnect(ctx, env)
		if err == nil {
			err = conn.Ping(ctx)
			conn.Close(ctx)
			if err == nil {
				return nil
			}
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	return fmt.Errorf("stepca-postgres did not become ready: %w", lastErr)
}

func waitCAInit(ctx context.Context, env map[string]string) error {
	caJSON := filepath.Join(env["CA_DATA_DIR"], "config", "ca.json")
	for i := 0; i < 30; i++ {
		if _, err := os.Stat(caJSON); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	if _, err := os.Stat(caJSON); err != nil {
		return fmt.Errorf("step-ca did not initialize (no %s); check the step-ca container logs", caJSON)
	}
	url := fmt.Sprintf("https://%s:%s/health", env["CA_FQDN"], env["CA_PORT"])
	root := filepath.Join(env["CA_DATA_DIR"], "certs", "root_ca.crt")
	return WaitHTTPSPinned(ctx, url, root, 30, 2*time.Second)
}

func configureProvisionerDuration(ctx context.Context, rc *RunCtx) error {
	env := rc.Env
	rc.Log("Configuring step-ca service certificate duration: %s", env["SERVICE_CERT_DURATION"])
	runner := Compose{Out: func(line string) { rc.Log("%s", line) }}
	err := runner.RunRM(ctx,
		"--user", "1000:1000",
		"-v", env["CA_DATA_DIR"]+":/home/step",
		env["CA_IMAGE"],
		"step", "ca", "provisioner", "update", env["CA_PROVISIONER_NAME"],
		"--x509-default-dur="+env["SERVICE_CERT_DURATION"],
		"--x509-max-dur="+env["SERVICE_CERT_DURATION"],
		"--ca-config", "/home/step/config/ca.json",
	)
	if err != nil {
		return fmt.Errorf("configure provisioner certificate duration: %w", err)
	}
	caJSON := filepath.Join(env["CA_DATA_DIR"], "config", "ca.json")
	_ = os.Chown(caJSON, 1000, 1000)
	return os.Chmod(caJSON, 0o600)
}

// guardCAPostgresStoreEmpty refuses to bind a freshly initialized CA (new
// root) onto a postgres store that already holds certificate records.
func guardCAPostgresStoreEmpty(ctx context.Context, env map[string]string) error {
	conn, err := caConnect(ctx, env)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT to_regclass('public.x509_certs') IS NOT NULL").Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	var rows int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM x509_certs").Scan(&rows); err != nil {
		return err
	}
	if rows != 0 {
		return fmt.Errorf("stepca-postgres already holds CA data (x509_certs has %d rows) but CA_DATA_DIR was freshly initialized; refusing to bind a new CA root to a mismatched store. Wipe CA_POSTGRES_DATA_DIR to rebuild, or restore the matching CA_DATA_DIR", rows)
	}
	return nil
}

// patchCAJSONPostgresCRL rewrites ca.json's db stanza to postgresql and
// enables CRL, preserving every other key (encoding/json replaces the jq
// pipeline).
func patchCAJSONPostgresCRL(rc *RunCtx, caJSON string, env map[string]string) error {
	b, err := os.ReadFile(caJSON)
	if err != nil {
		return err
	}
	var cfg map[string]any
	if err := json.Unmarshal(b, &cfg); err != nil {
		return fmt.Errorf("parse %s: %w", caJSON, err)
	}
	cfg["db"] = map[string]any{
		"type":       "postgresql",
		"dataSource": fmt.Sprintf("postgresql://%s@stepca-postgres:5432/%s?sslmode=disable", env["CA_POSTGRES_USER"], env["CA_POSTGRES_DB"]),
		"database":   env["CA_POSTGRES_DB"],
	}
	cfg["crl"] = map[string]any{"enabled": true, "generateOnRevoke": true, "cacheDuration": "24h"}
	out, err := json.MarshalIndent(cfg, "", "\t")
	if err != nil {
		return err
	}
	if err := os.WriteFile(caJSON, append(out, '\n'), 0o600); err != nil {
		return err
	}
	_ = os.Chown(caJSON, 1000, 1000)
	rc.Log("Rewrote %s: db -> postgresql, crl enabled.", caJSON)
	return nil
}

// moveBadgerDirAside keeps the abandoned badger dir for inspection.
func moveBadgerDirAside(rc *RunCtx, dataDir string) {
	badger := filepath.Join(dataDir, "db")
	if _, err := os.Stat(badger); err != nil {
		return
	}
	dest := badger + ".pre-postgres." + time.Now().Format("20060102150405")
	if err := os.Rename(badger, dest); err == nil {
		rc.Log("Moved the pre-migration badger directory aside: %s (retained, not deleted).", dest)
	}
}

// ensureStepcaReadonlyRole creates/refreshes the read-only role: SELECT on
// the three cert tables only. Role and db names are schema-validated
// identifiers; the password is passed as an escaped literal.
func ensureStepcaReadonlyRole(ctx context.Context, rc *RunCtx) error {
	env := rc.Env
	conn, err := caConnect(ctx, env)
	if err != nil {
		return err
	}
	defer conn.Close(ctx)

	roUser := env["CA_POSTGRES_RO_USER"]
	roPw := strings.ReplaceAll(env["CA_POSTGRES_RO_PASSWORD"], "'", "''")
	var exists bool
	if err := conn.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_roles WHERE rolname = $1)", roUser).Scan(&exists); err != nil {
		return err
	}
	verb := "CREATE"
	if exists {
		verb = "ALTER"
	}
	stmts := []string{
		fmt.Sprintf("%s ROLE %s LOGIN PASSWORD '%s'", verb, roUser, roPw),
		fmt.Sprintf("REVOKE ALL ON DATABASE %s FROM %s", env["CA_POSTGRES_DB"], roUser),
		fmt.Sprintf("GRANT CONNECT ON DATABASE %s TO %s", env["CA_POSTGRES_DB"], roUser),
		fmt.Sprintf("REVOKE ALL ON SCHEMA public FROM %s", roUser),
		fmt.Sprintf("GRANT USAGE ON SCHEMA public TO %s", roUser),
		fmt.Sprintf("GRANT SELECT ON x509_certs, x509_certs_data, revoked_x509_certs TO %s", roUser),
	}
	for _, stmt := range stmts {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			return fmt.Errorf("provision read-only role: %w", err)
		}
	}
	rc.Log("Ensured read-only postgres role '%s' (SELECT on cert tables only).", roUser)
	return nil
}

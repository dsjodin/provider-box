package deploy

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Netbox deploys NetBox with PostgreSQL, Redis, and the HTTPS terminator,
// the port of bootstrap/netbox.sh: pepper resolution, cert issuance, stack
// bring-up, API seeding (site/device/services/prefixes/IPs), and the
// dns-sync + dashboard token provisioning.
type Netbox struct{}

func (Netbox) Name() string   { return "netbox" }
func (Netbox) Deps() []string { return []string{"ca"} }

func (nb Netbox) Deploy(ctx context.Context, rc *RunCtx) error {
	env := rc.Env

	if len(env["NETBOX_SECRET_KEY"]) < 50 {
		return fmt.Errorf("NETBOX_SECRET_KEY must be at least 50 characters long")
	}
	if !strings.Contains(env["NETBOX_ALLOWED_HOSTS"], env["NETBOX_FQDN"]) {
		return fmt.Errorf("NETBOX_ALLOWED_HOSTS must include %s", env["NETBOX_FQDN"])
	}
	records, err := loadSeedRecords(rc)
	if err != nil {
		return err
	}
	if err := requireCAReady(ctx, env); err != nil {
		return err
	}

	// Directories: postgres runs as uid 70 and needs 0700.
	for _, dir := range []string{env["NETBOX_DIR"], env["NETBOX_MEDIA_DIR"], env["NETBOX_REDIS_DATA_DIR"]} {
		if err := EnsureDir(dir, 0o755, -1, -1); err != nil {
			return err
		}
	}
	if err := EnsureDir(env["NETBOX_POSTGRES_DATA_DIR"], 0o700, -1, -1); err != nil {
		return err
	}
	if err := chownR(env["NETBOX_POSTGRES_DATA_DIR"], 70, 70); err != nil {
		return err
	}

	pepper, err := resolveNetboxPepper(rc)
	if err != nil {
		return err
	}
	if err := IssueCert(ctx, rc, env["NETBOX_FQDN"], filepath.Join(env["NETBOX_DIR"], "certs"), "netbox"); err != nil {
		return err
	}

	renderEnv := withDerived(env, map[string]string{"NETBOX_API_TOKEN_PEPPER_1": pepper})
	if err := Render("docker-compose.netbox.yml.tpl", renderEnv, filepath.Join(env["NETBOX_DIR"], "docker-compose.yml"), 0o644); err != nil {
		return err
	}
	if err := Render("netbox-nginx.conf.tpl", renderEnv, filepath.Join(env["NETBOX_DIR"], "nginx.conf"), 0o644); err != nil {
		return err
	}

	cmp := Compose{Dir: env["NETBOX_DIR"], Out: func(line string) { rc.Log("%s", line) }}
	if err := cmp.Down(ctx); err != nil {
		return err
	}
	if err := cmp.Up(ctx); err != nil {
		return err
	}

	rc.Log("Waiting for NetBox to become ready at https://%s:%s/ (first start may take several minutes).", env["NETBOX_FQDN"], env["NETBOX_PORT"])
	caRoot := filepath.Join(env["CA_DATA_DIR"], "certs", "root_ca.crt")
	url := fmt.Sprintf("https://%s:%s/", env["NETBOX_FQDN"], env["NETBOX_PORT"])
	if err := WaitHTTPSPinned(ctx, url, caRoot, 120, 5*time.Second); err != nil {
		return err
	}

	api, err := newNetboxAPI(env)
	if err != nil {
		return err
	}
	api.auth, _, _, err = api.provisionToken(ctx, env["NETBOX_SUPERUSER_NAME"], env["NETBOX_SUPERUSER_PASSWORD"], "labprovider seeding", true)
	if err != nil {
		return fmt.Errorf("provision a NetBox API token for %s: %w", env["NETBOX_SUPERUSER_NAME"], err)
	}
	if err := seedNetbox(ctx, rc, api, records); err != nil {
		return err
	}
	if err := provisionNetboxDNSSyncToken(ctx, rc, api); err != nil {
		return err
	}
	if err := provisionNetboxDashboardToken(ctx, rc, api); err != nil {
		return err
	}
	// Retire the seeding token itself so re-runs do not accumulate live
	// superuser credentials (IMPROVEMENTS #2).
	api.retireTokensByDescription(ctx, rc, "labprovider seeding", "seeding")

	rc.Log("NetBox deployed: https://%s:%s (superuser %s). Media: %s",
		env["NETBOX_FQDN"], env["NETBOX_PORT"], env["NETBOX_SUPERUSER_NAME"], env["NETBOX_MEDIA_DIR"])
	return nil
}

func (nb Netbox) Remove(ctx context.Context, rc *RunCtx) error {
	env := rc.Env
	cmp := Compose{Dir: env["NETBOX_DIR"], Out: func(line string) { rc.Log("%s", line) }}
	if err := cmp.Down(ctx); err != nil {
		return err
	}
	os.Remove(filepath.Join(env["NETBOX_DIR"], "docker-compose.yml"))
	os.Remove(filepath.Join(env["NETBOX_DIR"], "nginx.conf"))
	os.RemoveAll(filepath.Join(env["NETBOX_DIR"], "certs"))
	rc.Log("Removed NetBox containers and runtime files. Persistent data in %s, %s, and %s was preserved (secrets included).",
		env["NETBOX_MEDIA_DIR"], env["NETBOX_POSTGRES_DATA_DIR"], env["NETBOX_REDIS_DATA_DIR"])
	return nil
}

// seedRecord is one external/custom DNS record from config/dns.seed.
type seedRecord struct {
	FQDN  string
	Value string // ip or ip/cidr
}

// loadSeedRecords parses the optional dns.seed file the wizard stores next to
// the managed config; records feed the NetBox IPAM import.
func loadSeedRecords(rc *RunCtx) ([]seedRecord, error) {
	path := seedFilePath(rc.Env)
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		rc.Log("No custom DNS records file found at %s; skipping import.", path)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var records []seedRecord
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("invalid record format in %s:%d (expected: <fqdn> <ip> or <fqdn> <ip/cidr>)", path, i+1)
		}
		records = append(records, seedRecord{FQDN: fields[0], Value: fields[1]})
	}
	return records, nil
}

// seedFilePath is the managed dns.seed location: alongside the managed config
// under /opt/labprovider/control-plane, overridable via DNS_SEED_FILE.
func seedFilePath(env map[string]string) string {
	if v := env["DNS_SEED_FILE"]; v != "" {
		return v
	}
	return "/opt/labprovider/control-plane/dns.seed"
}

func resolveNetboxPepper(rc *RunCtx) (string, error) {
	env := rc.Env
	secrets := filepath.Join(env["NETBOX_DIR"], "secrets")
	if err := EnsureDir(secrets, 0o700, -1, -1); err != nil {
		return "", err
	}
	pepperFile := filepath.Join(secrets, "api_token_pepper")
	if b, err := os.ReadFile(pepperFile); err == nil {
		rc.Log("Reusing existing NetBox API token pepper: %s", pepperFile)
		if len(b) < 50 {
			return "", fmt.Errorf("NetBox API token pepper in %s must be at least 50 characters long", pepperFile)
		}
		return string(b), nil
	}
	value := env["NETBOX_API_TOKEN_PEPPER"]
	if value != "" {
		if len(value) < 50 {
			return "", fmt.Errorf("NETBOX_API_TOKEN_PEPPER must be at least 50 characters long")
		}
		rc.Log("Materializing NETBOX_API_TOKEN_PEPPER to managed file: %s", pepperFile)
	} else {
		raw := make([]byte, 48)
		if _, err := rand.Read(raw); err != nil {
			return "", err
		}
		value = base64.StdEncoding.EncodeToString(raw)
		rc.Log("Generating a NetBox API token pepper at: %s", pepperFile)
	}
	if err := os.WriteFile(pepperFile, []byte(value), 0o600); err != nil {
		return "", err
	}
	return value, nil
}

// seedNetbox creates the labprovider inventory: site, manufacturer, device
// type, role, device, the canonical host IP, one service entry per built-in
// endpoint, and the dns.seed records (one IP object per unique address).
func seedNetbox(ctx context.Context, rc *RunCtx, api *netboxAPI, records []seedRecord) error {
	env := rc.Env

	siteID, err := api.ensureObject(ctx, "/api/dcim/sites/", "name=Provider+Box",
		map[string]any{"name": "labprovider", "slug": "labprovider", "status": "active"})
	if err != nil {
		return err
	}
	manufacturerID, err := api.ensureObject(ctx, "/api/dcim/manufacturers/", "name=Provider+Box",
		map[string]any{"name": "labprovider", "slug": "labprovider"})
	if err != nil {
		return err
	}
	deviceTypeID, err := api.ensureObject(ctx, "/api/dcim/device-types/", "model=Provider+Box",
		map[string]any{"manufacturer": manufacturerID, "model": "labprovider", "slug": "labprovider"})
	if err != nil {
		return err
	}
	roleID, err := api.ensureObject(ctx, "/api/dcim/device-roles/", "name=Provider+Services",
		map[string]any{"name": "Provider Services", "slug": "provider-services", "color": "607d8b"})
	if err != nil {
		return err
	}
	deviceID, err := api.getObjectID(ctx, "/api/dcim/devices/", "name=labprovider")
	if err != nil {
		return err
	}
	devicePayload := map[string]any{"site": siteID, "device_type": deviceTypeID, "role": roleID, "status": "active"}
	if deviceID == 0 {
		devicePayload["name"] = "labprovider"
		if deviceID, err = api.createObject(ctx, "/api/dcim/devices/", devicePayload); err != nil {
			return err
		}
	} else if err := api.patchObject(ctx, "/api/dcim/devices/", deviceID, devicePayload); err != nil {
		return err
	}

	// Canonical host IP: created explicitly from HOST_IP with
	// LABPROVIDER_FQDN as dns_name; built-in service FQDNs live in the
	// description. Always patched so config changes propagate.
	prefix, err := ensureSeedPrefix(ctx, api, env["HOST_IP"], "labprovider")
	if err != nil {
		return err
	}
	_ = prefix
	description := "labprovider services: " + strings.Join(builtinServiceFQDNs(env), ", ")
	hostAddr := env["HOST_IP"]
	hostPayload := map[string]any{"address": hostAddr, "dns_name": env["LABPROVIDER_FQDN"], "status": "active", "description": description}
	hostIPID, err := api.getObjectID(ctx, "/api/ipam/ip-addresses/", "address="+url.QueryEscape(hostAddr))
	if err != nil {
		return err
	}
	if hostIPID == 0 {
		if _, err := api.createObject(ctx, "/api/ipam/ip-addresses/", hostPayload); err != nil {
			return err
		}
	} else if err := api.patchObject(ctx, "/api/ipam/ip-addresses/", hostIPID, hostPayload); err != nil {
		return err
	}

	for _, svc := range builtinServiceEntries(env) {
		svcID, err := api.getObjectID(ctx, "/api/ipam/services/", fmt.Sprintf("device_id=%d&name=%s", deviceID, url.QueryEscape(svc.name)))
		if err != nil {
			return err
		}
		port, _ := strconv.Atoi(svc.port)
		payload := map[string]any{
			"parent_object_type": "dcim.device", "parent_object_id": deviceID,
			"name": svc.name, "protocol": svc.protocol, "ports": []int{port}, "description": svc.fqdn,
		}
		if svcID == 0 {
			if _, err := api.createObject(ctx, "/api/ipam/services/", payload); err != nil {
				return err
			}
		} else if err := api.patchObject(ctx, "/api/ipam/services/", svcID, payload); err != nil {
			return err
		}
	}

	for _, rec := range records {
		if err := ensureSeedIPAddress(ctx, api, rec); err != nil {
			return err
		}
	}
	return nil
}

// ensureSeedPrefix derives and creates the surrounding prefix object when the
// value carries CIDR information; plain IPs create nothing.
func ensureSeedPrefix(ctx context.Context, api *netboxAPI, value, source string) (string, error) {
	if !strings.Contains(value, "/") {
		return "", nil
	}
	_, network, err := deriveNetwork(value)
	if err != nil {
		return "", err
	}
	prefixID, err := api.getObjectID(ctx, "/api/ipam/prefixes/", "prefix="+url.QueryEscape(network))
	if err != nil {
		return "", err
	}
	payload := map[string]any{"prefix": network, "status": "active", "description": "Imported from " + source}
	if prefixID == 0 {
		if _, err := api.createObject(ctx, "/api/ipam/prefixes/", payload); err != nil {
			return "", err
		}
	} else if err := api.patchObject(ctx, "/api/ipam/prefixes/", prefixID, payload); err != nil {
		return "", err
	}
	return network, nil
}

// ensureSeedIPAddress imports one dns.seed record: prefix (when CIDR is
// present) plus one IP object per unique address. An existing object is left
// unchanged so re-runs never overwrite the canonical dns_name.
func ensureSeedIPAddress(ctx context.Context, api *netboxAPI, rec seedRecord) error {
	address := rec.Value
	if strings.Contains(rec.Value, "/") {
		if _, err := ensureSeedPrefix(ctx, api, rec.Value, "dns.seed"); err != nil {
			return err
		}
	} else {
		address = rec.Value + "/32"
	}
	ipID, err := api.getObjectID(ctx, "/api/ipam/ip-addresses/", "address="+url.QueryEscape(address))
	if err != nil {
		return err
	}
	if ipID != 0 {
		return nil
	}
	_, err = api.createObject(ctx, "/api/ipam/ip-addresses/", map[string]any{
		"address": address, "dns_name": rec.FQDN, "status": "active", "description": "Imported from dns.seed",
	})
	return err
}

func deriveNetwork(cidr string) (ip, network string, err error) {
	addr, ipnet, err := net.ParseCIDR(cidr)
	if err != nil {
		return "", "", fmt.Errorf("invalid CIDR %q: %w", cidr, err)
	}
	return addr.String(), ipnet.String(), nil
}

type builtinService struct {
	name, fqdn, protocol, port string
}

// builtinServiceEntries is the Go port of build_netbox_service_seed_block.
func builtinServiceEntries(env map[string]string) []builtinService {
	return []builtinService{
		{"dns-tcp", env["DNS_FQDN"], "tcp", "53"},
		{"dns-udp", env["DNS_FQDN"], "udp", "53"},
		{"ntp", env["DNS_FQDN"], "udp", "123"},
		{"syslog-tcp", env["SYSLOG_FQDN"], "tcp", env["SYSLOG_PORT"]},
		{"syslog-udp", env["SYSLOG_FQDN"], "udp", env["SYSLOG_PORT"]},
		{"step-ca", env["CA_FQDN"], "tcp", env["CA_PORT"]},
		{"depot-http", env["DEPOT_FQDN"], "tcp", env["DEPOT_HTTP_PORT"]},
		{"depot-https", env["DEPOT_FQDN"], "tcp", env["DEPOT_HTTPS_PORT"]},
		{"keycloak", env["KEYCLOAK_FQDN"], "tcp", env["KEYCLOAK_PORT"]},
		{"zitadel", env["ZITADEL_FQDN"], "tcp", env["ZITADEL_PORT"]},
		{"netbox", env["NETBOX_FQDN"], "tcp", env["NETBOX_PORT"]},
		{"s3", env["S3_FQDN"], "tcp", env["S3_PORT"]},
		{"sftp", env["SFTP_FQDN"], "tcp", env["SFTP_PORT"]},
		{"sftp-admin", env["SFTP_FQDN"], "tcp", env["SFTP_ADMIN_PORT"]},
	}
}

// builtinServiceFQDNs is the shared FQDN list (labprovider_builtin_fqdns):
// the canonical host description and dns-sync's built-in record synthesis
// both consume it. Unset services are skipped.
func builtinServiceFQDNs(env map[string]string) []string {
	var fqdns []string
	for _, key := range []string{"DNS_FQDN", "CA_FQDN", "DEPOT_FQDN", "KEYCLOAK_FQDN", "AUTHENTIK_FQDN", "ZITADEL_FQDN", "NETBOX_FQDN", "S3_FQDN", "SFTP_FQDN", "SYSLOG_FQDN", "CONTROL_PLANE_FQDN"} {
		if env[key] != "" {
			fqdns = append(fqdns, env[key])
		}
	}
	return fqdns
}

// provisionNetboxDNSSyncToken mints (or reuses) the composite Bearer token
// dns-sync consumes, retiring stale tokens first.
func provisionNetboxDNSSyncToken(ctx context.Context, rc *RunCtx, api *netboxAPI) error {
	env := rc.Env
	if env["DNS_SYNC_SECRETS_DIR"] == "" {
		rc.Log("NOTICE: DNS_SYNC_SECRETS_DIR is not set; skipping dns-sync NetBox token provisioning.")
		return nil
	}
	if err := EnsureDir(env["DNS_SYNC_SECRETS_DIR"], 0o700, -1, -1); err != nil {
		return err
	}
	tokenFile := filepath.Join(env["DNS_SYNC_SECRETS_DIR"], "netbox.token")
	if stored, err := os.ReadFile(tokenFile); err == nil && len(stored) > 0 {
		if api.statusWith(ctx, "Bearer "+string(stored), "/api/") == http.StatusOK {
			rc.Log("Reusing existing dns-sync NetBox token: %s", tokenFile)
			_ = os.Chmod(tokenFile, 0o600)
			_ = os.Chown(tokenFile, 1000, 1000)
			return nil
		}
		rc.Log("Stored dns-sync NetBox token was rejected; provisioning a replacement.")
	}
	api.retireTokensByDescription(ctx, rc, "labprovider dns-sync", "dns-sync")
	_, composite, _, err := api.provisionToken(ctx, env["NETBOX_SUPERUSER_NAME"], env["NETBOX_SUPERUSER_PASSWORD"], "labprovider dns-sync", true)
	if err != nil {
		return fmt.Errorf("provision a dns-sync NetBox token: %w", err)
	}
	if api.statusWith(ctx, "Bearer "+composite, "/api/") != http.StatusOK {
		return fmt.Errorf("NetBox rejected the freshly provisioned dns-sync token")
	}
	if err := os.WriteFile(tokenFile, []byte(composite), 0o600); err != nil {
		return err
	}
	_ = os.Chown(tokenFile, 1000, 1000)
	rc.Log("Provisioned a dns-sync NetBox token at: %s", tokenFile)
	return nil
}

// provisionNetboxDashboardToken creates the minimum-read path for the IPAM
// panel: a dashboard-readonly group with view-only permission on Prefix and
// IP address, a non-privileged service user, and a read-only composite token.
func provisionNetboxDashboardToken(ctx context.Context, rc *RunCtx, api *netboxAPI) error {
	env := rc.Env
	secretsDir := env["CONTROL_PLANE_SECRETS_DIR"]
	if secretsDir == "" {
		rc.Log("NOTICE: CONTROL_PLANE_SECRETS_DIR is not set; skipping dashboard NetBox read-only token provisioning.")
		return nil
	}
	if err := EnsureDir(secretsDir, 0o700, 1000, 1000); err != nil {
		return err
	}
	tokenFile := filepath.Join(secretsDir, "netbox-readonly.token")
	probe := "/api/ipam/prefixes/?limit=1"
	if stored, err := os.ReadFile(tokenFile); err == nil && len(stored) > 0 {
		if api.statusWith(ctx, "Bearer "+string(stored), probe) == http.StatusOK {
			rc.Log("Reusing existing dashboard NetBox token: %s", tokenFile)
			_ = os.Chmod(tokenFile, 0o600)
			_ = os.Chown(tokenFile, 1000, 1000)
			return nil
		}
		rc.Log("Stored dashboard NetBox token was rejected; provisioning a replacement.")
	}

	groupID, err := api.ensureObject(ctx, "/api/users/groups/", "name=dashboard-readonly",
		map[string]any{"name": "dashboard-readonly"})
	if err != nil {
		return fmt.Errorf("create or find the dashboard-readonly NetBox group: %w", err)
	}
	permPayload := map[string]any{
		"name": "dashboard-readonly", "enabled": true,
		"object_types": []string{"ipam.prefix", "ipam.ipaddress"},
		"actions":      []string{"view"},
		"groups":       []int{groupID},
	}
	permID, err := api.getObjectID(ctx, "/api/users/permissions/", "name=dashboard-readonly")
	if err != nil {
		return err
	}
	if permID == 0 {
		if _, err := api.createObject(ctx, "/api/users/permissions/", permPayload); err != nil {
			return err
		}
	} else if err := api.patchObject(ctx, "/api/users/permissions/", permID, permPayload); err != nil {
		return err
	}

	// Per-pass password used only to provision the token, never stored. The
	// "Aa1!" suffix satisfies NetBox 4.6's password class validators.
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return err
	}
	dashPass := base64.StdEncoding.EncodeToString(raw) + "Aa1!"
	userID, err := api.getObjectID(ctx, "/api/users/users/", "username=dashboard")
	if err != nil {
		return err
	}
	userPayload := map[string]any{"password": dashPass, "is_active": true, "is_staff": false, "is_superuser": false, "groups": []int{groupID}}
	if userID == 0 {
		userPayload["username"] = "dashboard"
		if _, err := api.createObject(ctx, "/api/users/users/", userPayload); err != nil {
			return err
		}
	} else if err := api.patchObject(ctx, "/api/users/users/", userID, userPayload); err != nil {
		return err
	}

	api.retireTokensByDescription(ctx, rc, "labprovider dashboard", "dashboard")
	_, composite, tokenID, err := api.provisionToken(ctx, "dashboard", dashPass, "labprovider dashboard", false)
	if err != nil {
		return fmt.Errorf("provision a dashboard NetBox token: %w", err)
	}
	// Enforce read-only at the token level regardless of the provision body.
	if tokenID != 0 {
		_ = api.patchObject(ctx, "/api/users/tokens/", tokenID, map[string]any{"write_enabled": false})
	}
	if api.statusWith(ctx, "Bearer "+composite, probe) != http.StatusOK {
		return fmt.Errorf("NetBox rejected the freshly provisioned dashboard token on IPAM read")
	}
	if err := os.WriteFile(tokenFile, []byte(composite), 0o600); err != nil {
		return err
	}
	_ = os.Chown(tokenFile, 1000, 1000)
	rc.Log("Provisioned a read-only dashboard NetBox token at: %s", tokenFile)
	return nil
}

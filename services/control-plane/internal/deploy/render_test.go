package deploy

import (
	"os"
	"path/filepath"
	"testing"
)

var testEnv = map[string]string{
	"S3_IMAGE":        "docker.io/chrislusf/seaweedfs:4.31",
	"S3_ACCESS_KEY":   "ak",
	"S3_SECRET_KEY":   "sk",
	"S3_PORT":         "8333",
	"S3_DATA_DIR":     "/opt/labprovider/seaweedfs",
	"WORKDIR":         "/opt/labprovider/runtime",
	"CHRONY_IMAGE":    "labprovider/chrony:0.1.0",
	"CHRONY_DIR":      "/opt/labprovider/chrony",
	"CHRONY_SERVER_1": "0.se.pool.ntp.org",
	"CHRONY_SERVER_2": "1.se.pool.ntp.org",
	"CHRONY_SERVER_3": "2.se.pool.ntp.org",
	"ALLOW_NET_1":     "10.0.0.0/8",
	"ALLOW_NET_2":     "172.16.0.0/12",
	"ALLOW_NET_3":     "192.168.0.0/16",
	"RSYSLOG_IMAGE":   "labprovider/rsyslog:0.1.0",
	"SYSLOG_PORT":     "514",
	"SYSLOG_LOG_DIR":  "/opt/labprovider/syslog/logs",

	"CA_IMAGE":                      "docker.io/smallstep/step-ca:0.30.2",
	"CA_POSTGRES_IMAGE":             "docker.io/library/postgres:17-alpine",
	"CA_POSTGRES_DB":                "stepca",
	"CA_POSTGRES_USER":              "stepca",
	"CA_POSTGRES_PASSWORD":          "pgpw",
	"CA_POSTGRES_PORT":              "5432",
	"CA_POSTGRES_DATA_DIR":          "/opt/labprovider/stepca-postgres",
	"CA_DATA_DIR":                   "/opt/labprovider/step-ca",
	"CA_FQDN":                       "ca.sddc.lab",
	"CA_PORT":                       "9000",
	"CA_NAME":                       "labprovider CA",
	"CA_PROVISIONER_NAME":           "admin",
	"CA_ENABLE_ACME":                "true",
	"CA_PASSWORD_FILE_IN_CONTAINER": "/home/step/secrets/password.txt",
	"CA_PGPASSFILE_IN_CONTAINER":    "/home/step/secrets/pgpass",

	"DNS_FQDN":              "dns.sddc.lab",
	"TECHNITIUM_IMAGE":      "docker.io/technitium/dns-server:15.3.0",
	"TECHNITIUM_HTTP_PORT":  "5380",
	"TECHNITIUM_HTTPS_PORT": "53443",
	"TECHNITIUM_DATA_DIR":   "/opt/labprovider/technitium/data",
	"TECHNITIUM_CERT_DIR":   "/opt/labprovider/technitium/certs",

	"NETBOX_FQDN":                 "netbox.sddc.lab",
	"NETBOX_PORT":                 "8444",
	"NETBOX_DIR":                  "/opt/labprovider/netbox",
	"NETBOX_MEDIA_DIR":            "/opt/labprovider/netbox/media",
	"NETBOX_POSTGRES_DATA_DIR":    "/opt/labprovider/netbox/postgres",
	"NETBOX_REDIS_DATA_DIR":       "/opt/labprovider/netbox/redis",
	"NETBOX_IMAGE":                "docker.io/netboxcommunity/netbox:v4.6.2",
	"NETBOX_POSTGRES_IMAGE":       "docker.io/library/postgres:17-alpine",
	"NETBOX_REDIS_IMAGE":          "docker.io/library/redis:7-alpine",
	"NETBOX_NGINX_IMAGE":          "docker.io/library/nginx:1.31-alpine",
	"NETBOX_POSTGRES_DB":          "netbox",
	"NETBOX_POSTGRES_USER":        "netbox",
	"NETBOX_POSTGRES_PASSWORD":    "nbpg",
	"NETBOX_REDIS_PASSWORD":       "nbredis",
	"NETBOX_SECRET_KEY":           "sk",
	"NETBOX_API_TOKEN_PEPPER_1":   "pepper",
	"NETBOX_ALLOWED_HOSTS":        "netbox.sddc.lab",
	"NETBOX_CSRF_TRUSTED_ORIGINS": "https://netbox.sddc.lab:8444",
	"NETBOX_SUPERUSER_NAME":       "admin",
	"NETBOX_SUPERUSER_EMAIL":      "admin@sddc.lab",
	"NETBOX_SUPERUSER_PASSWORD":   "nbsu",

	"DNS_SYNC_IMAGE":                     "labprovider/dns-sync:0.1.0",
	"DNS_SYNC_NETBOX_URL":                "https://netbox.sddc.lab:8444",
	"DNS_SYNC_TECHNITIUM_URL":            "https://dns.sddc.lab:53443",
	"DNS_SYNC_NETBOX_HOST":               "netbox.sddc.lab",
	"DNS_SYNC_TECHNITIUM_HOST":           "dns.sddc.lab",
	"DNS_SYNC_INTERVAL":                  "30s",
	"DNS_SYNC_BUILTIN_RECORDS":           "labprovider.sddc.lab=192.168.12.121",
	"DNS_SYNC_SECRETS_DIR":               "/opt/labprovider/dns-sync/secrets",
	"DNS_SYNC_TECHNITIUM_DASHBOARD_USER": "dashboard",

	"DEPOT_FQDN":       "vcfdepot.sddc.lab",
	"DEPOT_HTTP_PORT":  "80",
	"DEPOT_HTTPS_PORT": "443",
	"DEPOT_DATA_DIR":   "/opt/labprovider/depot/data",
	"DEPOT_CERT_DIR":   "/opt/labprovider/depot/certs",
	"DEPOT_AUTH_DIR":   "/opt/labprovider/depot/auth",
	"DEPOT_IMAGE":      "docker.io/library/nginx:1.31-alpine",

	"KEYCLOAK_IMAGE":          "quay.io/keycloak/keycloak:26.6.3",
	"KEYCLOAK_DIR":            "/opt/labprovider/keycloak",
	"KEYCLOAK_FQDN":           "auth.sddc.lab",
	"KEYCLOAK_PORT":           "8443",
	"KEYCLOAK_ADMIN_USER":     "admin",
	"KEYCLOAK_ADMIN_PASSWORD": "kcadmin",

	"AUTHENTIK_IMAGE":                                "ghcr.io/goauthentik/server:2026.5.3",
	"AUTHENTIK_POSTGRES_IMAGE":                       "docker.io/library/postgres:16-alpine",
	"AUTHENTIK_DIR":                                  "/opt/labprovider/authentik",
	"AUTHENTIK_FQDN":                                 "idp.sddc.lab",
	"AUTHENTIK_PORT":                                 "9443",
	"AUTHENTIK_SECRET_KEY":                           "aksecret",
	"AUTHENTIK_PG_DB":                                "authentik",
	"AUTHENTIK_PG_USER":                              "authentik",
	"AUTHENTIK_PG_PASSWORD":                          "akpg",
	"AUTHENTIK_ADMIN_PASSWORD":                       "akadmin",
	"AUTHENTIK_API_TOKEN":                            "aktoken",
	"AUTHENTIK_BOOTSTRAP_GROUP_NAME":                 "vcf-admins",
	"AUTHENTIK_BOOTSTRAP_USERNAME":                   "lab-admin",
	"AUTHENTIK_BOOTSTRAP_USER_PASSWORD":              "akuser",
	"AUTHENTIK_BOOTSTRAP_USER_EMAIL_DOMAIN":          "sddc.lab",
	"AUTHENTIK_BOOTSTRAP_CLIENT_ID":                  "vcf-sso",
	"AUTHENTIK_BOOTSTRAP_CLIENT_SECRET":              "akclient",
	"AUTHENTIK_BOOTSTRAP_CLIENT_REDIRECT_URIS_BLOCK": "        - matching_mode: strict\n          url: \"https://vc.sddc.lab/oauth2\"",

	"ZITADEL_IMAGE":          "ghcr.io/zitadel/zitadel:v4.16.1",
	"ZITADEL_LOGIN_IMAGE":    "ghcr.io/zitadel/zitadel-login:v4.16.1",
	"ZITADEL_NGINX_IMAGE":    "docker.io/library/nginx:1.31-alpine",
	"ZITADEL_POSTGRES_IMAGE": "docker.io/library/postgres:17-alpine",
	"ZITADEL_DIR":            "/opt/labprovider/zitadel",
	"ZITADEL_FQDN":           "zid.sddc.lab",
	"ZITADEL_PORT":           "7443",
	"ZITADEL_MASTERKEY":      "0123456789abcdef0123456789abcdef",
	"ZITADEL_ADMIN_USERNAME": "zitadel-admin",
	"ZITADEL_ADMIN_PASSWORD": "zidadmin",
	"ZITADEL_PG_DB":          "zitadel",
	"ZITADEL_PG_USER":        "zitadel",
	"ZITADEL_PG_PASSWORD":    "zidpg",

	"SFTPGO_IMAGE":        "docker.io/drakkan/sftpgo:v2.7.3",
	"SFTP_FQDN":           "sftp.sddc.lab",
	"SFTP_PORT":           "2022",
	"SFTP_ADMIN_PORT":     "8080",
	"SFTP_ADMIN_USER":     "admin",
	"SFTP_ADMIN_PASSWORD": "sftppw",
	"SFTP_DATA_DIR":       "/opt/labprovider/sftpgo/data",
	"SFTP_HOME_DIR":       "/opt/labprovider/sftpgo/home",
	"SFTP_CERT_DIR":       "/opt/labprovider/sftpgo/certs",

	"TRAEFIK_IMAGE":      "docker.io/library/traefik:v3.7.7",
	"TRAEFIK_FQDN":       "traefik.sddc.lab",
	"TRAEFIK_DIR":        "/opt/labprovider/traefik",
	"CONTROL_PLANE_FQDN": "dashboard.sddc.lab",
	"CONTROL_PLANE_PORT": "8445",
	"VMSCA_ENABLE":       "true",
	"VMSCA_FQDN":         "certsrv.sddc.lab",
	"VMSCA_PORT":         "8446",
	"HOST_IPV4":          "192.168.12.121",
}

// TestRenderGolden is the template parity harness: each converted template is
// rendered with a fixture env and compared to a checked-in golden file (the
// s3 golden was produced with envsubst from the original templates/*.tpl, so
// the conversion provably matches the bash render).
func TestRenderGolden(t *testing.T) {
	for _, name := range []string{
		"docker-compose.s3.yml.tpl",
		"chrony.conf.tpl",
		"docker-compose.chrony.yml.tpl",
		"rsyslog.conf.tpl",
		"docker-compose.rsyslog.yml.tpl",
		"docker-compose.step-ca.yml.tpl",
		"docker-compose.technitium.yml.tpl",
		"docker-compose.netbox.yml.tpl",
		"netbox-nginx.conf.tpl",
		"docker-compose.dns-sync.yml.tpl",
		"docker-compose.depot.yml.tpl",
		"depot-nginx.conf.tpl",
		"docker-compose.keycloak.yml.tpl",
		"docker-compose.authentik.yml.tpl",
		"authentik-blueprint.yaml.tpl",
		"docker-compose.zitadel.yml.tpl",
		"zitadel-nginx.conf.tpl",
		"docker-compose.sftpgo.yml.tpl",
		"traefik.yml.tpl",
		"traefik-dynamic.yml.tpl",
		"docker-compose.traefik.yml.tpl",
	} {
		t.Run(name, func(t *testing.T) {
			dest := filepath.Join(t.TempDir(), "out")
			if err := Render(name, testEnv, dest, 0o644); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(dest)
			if err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", name+".golden")
			if os.Getenv("UPDATE_GOLDEN") == "1" {
				if err := os.WriteFile(golden, got, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("render mismatch for %s:\n--- got ---\n%s\n--- want ---\n%s", name, got, want)
			}
		})
	}
}

// TestRenderUnsetVariableFails locks in the missingkey=error behavior that
// envsubst lacked: referencing an unset variable aborts the render.
func TestRenderUnsetVariableFails(t *testing.T) {
	err := Render("docker-compose.s3.yml.tpl", map[string]string{}, filepath.Join(t.TempDir(), "out"), 0o644)
	if err == nil {
		t.Fatal("render with unset variables succeeded; want error")
	}
}

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
	"S3_DATA_DIR":     "/opt/provider-box/seaweedfs",
	"WORKDIR":         "/opt/provider-box/runtime",
	"CHRONY_IMAGE":    "provider-box/chrony:0.1.0",
	"CHRONY_DIR":      "/opt/provider-box/chrony",
	"CHRONY_SERVER_1": "0.se.pool.ntp.org",
	"CHRONY_SERVER_2": "1.se.pool.ntp.org",
	"CHRONY_SERVER_3": "2.se.pool.ntp.org",
	"ALLOW_NET_1":     "10.0.0.0/8",
	"ALLOW_NET_2":     "172.16.0.0/12",
	"ALLOW_NET_3":     "192.168.0.0/16",
	"RSYSLOG_IMAGE":   "provider-box/rsyslog:0.1.0",
	"SYSLOG_PORT":     "514",
	"SYSLOG_LOG_DIR":  "/opt/provider-box/syslog/logs",

	"CA_IMAGE":                      "docker.io/smallstep/step-ca:0.30.2",
	"CA_POSTGRES_IMAGE":             "docker.io/library/postgres:17-alpine",
	"CA_POSTGRES_DB":                "stepca",
	"CA_POSTGRES_USER":              "stepca",
	"CA_POSTGRES_PASSWORD":          "pgpw",
	"CA_POSTGRES_PORT":              "5432",
	"CA_POSTGRES_DATA_DIR":          "/opt/provider-box/stepca-postgres",
	"CA_DATA_DIR":                   "/opt/provider-box/step-ca",
	"CA_FQDN":                       "ca.sddc.lab",
	"CA_PORT":                       "9000",
	"CA_NAME":                       "Provider Box CA",
	"CA_PROVISIONER_NAME":           "admin",
	"CA_ENABLE_ACME":                "true",
	"CA_PASSWORD_FILE_IN_CONTAINER": "/home/step/secrets/password.txt",
	"CA_PGPASSFILE_IN_CONTAINER":    "/home/step/secrets/pgpass",
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

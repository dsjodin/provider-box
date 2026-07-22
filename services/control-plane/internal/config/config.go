// Package config loads dashboard settings from the environment. All upstream
// tokens come from files (preferred) or env vars; nothing is hardcoded and
// tokens are never logged.
package config

import (
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Addr string // listen address, e.g. :8443
	FQDN string // dashboard FQDN, for display

	TLSCert string // path to the step-ca-issued cert; empty => HTTP fallback
	TLSKey  string

	// Certificates panel (step-ca PostgreSQL backend, read-only role).
	// DSN carries no password; the password comes from a file (or env) so it
	// stays out of the DSN string, matching the repo's file-based secrets.
	StepCADSN      string
	StepCAPassword string
	CertWarnDays   int

	// DNS panel (Technitium).
	TechnitiumURL      string
	TechnitiumToken    string
	TechnitiumCABundle string

	// IPAM panel (NetBox) - dedicated read-only token.
	NetboxURL      string
	NetboxToken    string
	NetboxCABundle string

	// Services + errors panels (Docker socket, mounted read-only).
	DockerHost       string
	ContainerFilters []string
	LogTail          int

	UpstreamTimeout time.Duration

	// Deploy engine paths. ConfigPath is the managed labprovider.env the
	// wizard edits; ExamplePath is the shipped example (copied into the image
	// at build time); StatePath is the advisory deploy-state file. The engine
	// is enabled when ExamplePath exists.
	ConfigPath  string
	ExamplePath string
	StatePath   string
}

// Load reads configuration from the environment.
func Load() Config {
	return Config{
		Addr: envOr("CONTROL_PLANE_ADDR", ":8443"),
		FQDN: envOr("CONTROL_PLANE_FQDN", "dashboard.sddc.lab"),

		TLSCert: os.Getenv("CONTROL_PLANE_TLS_CERT"),
		TLSKey:  os.Getenv("CONTROL_PLANE_TLS_KEY"),

		StepCADSN:      os.Getenv("CONTROL_PLANE_STEPCA_DSN"),
		StepCAPassword: readToken("CONTROL_PLANE_STEPCA_PG_PASSWORD_FILE", "CONTROL_PLANE_STEPCA_PG_PASSWORD"),
		CertWarnDays:   envInt("CONTROL_PLANE_CERT_WARN_DAYS", 30),

		TechnitiumURL:      os.Getenv("CONTROL_PLANE_TECHNITIUM_URL"),
		TechnitiumToken:    readToken("CONTROL_PLANE_TECHNITIUM_TOKEN_FILE", "CONTROL_PLANE_TECHNITIUM_TOKEN"),
		TechnitiumCABundle: os.Getenv("CONTROL_PLANE_TECHNITIUM_CA_BUNDLE"),

		NetboxURL:      os.Getenv("CONTROL_PLANE_NETBOX_URL"),
		NetboxToken:    readToken("CONTROL_PLANE_NETBOX_TOKEN_FILE", "CONTROL_PLANE_NETBOX_TOKEN"),
		NetboxCABundle: os.Getenv("CONTROL_PLANE_NETBOX_CA_BUNDLE"),

		DockerHost:       envOr("CONTROL_PLANE_DOCKER_HOST", "unix:///var/run/docker.sock"),
		ContainerFilters: splitCSV(envOr("CONTROL_PLANE_CONTAINER_FILTERS", "step-ca,technitium,netbox,dns-sync,authentik,keycloak,zitadel,depot,sftpgo,seaweedfs,control-plane")),
		LogTail:          envInt("CONTROL_PLANE_LOG_TAIL", 200),

		UpstreamTimeout: envDuration("CONTROL_PLANE_UPSTREAM_TIMEOUT", 5*time.Second),

		ConfigPath:  envOr("CONTROL_PLANE_CONFIG_PATH", "/opt/labprovider/control-plane/labprovider.env"),
		ExamplePath: envOr("CONTROL_PLANE_EXAMPLE_PATH", "/usr/local/share/labprovider/labprovider.env.example"),
		StatePath:   envOr("CONTROL_PLANE_STATE_PATH", "/opt/labprovider/control-plane/state.json"),
	}
}

// readToken prefers a file path (SOPS/age friendly) over an inline env var.
func readToken(fileKey, envKey string) string {
	if p := os.Getenv(fileKey); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return strings.TrimSpace(os.Getenv(envKey))
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

func envDuration(key string, def time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func splitCSV(v string) []string {
	var out []string
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

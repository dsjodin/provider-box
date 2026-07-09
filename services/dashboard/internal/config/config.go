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

	// Certificates panel (step-ca BadgerDB, read via snapshot).
	StepCADBPath   string
	StepCASnapshot string
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
}

// Load reads configuration from the environment.
func Load() Config {
	return Config{
		Addr: envOr("DASHBOARD_ADDR", ":8443"),
		FQDN: envOr("DASHBOARD_FQDN", "dashboard.sddc.lab"),

		TLSCert: os.Getenv("DASHBOARD_TLS_CERT"),
		TLSKey:  os.Getenv("DASHBOARD_TLS_KEY"),

		StepCADBPath:   os.Getenv("DASHBOARD_STEPCA_DB"),
		StepCASnapshot: os.Getenv("DASHBOARD_STEPCA_SNAPSHOT_DIR"),
		CertWarnDays:   envInt("DASHBOARD_CERT_WARN_DAYS", 30),

		TechnitiumURL:      os.Getenv("DASHBOARD_TECHNITIUM_URL"),
		TechnitiumToken:    readToken("DASHBOARD_TECHNITIUM_TOKEN_FILE", "DASHBOARD_TECHNITIUM_TOKEN"),
		TechnitiumCABundle: os.Getenv("DASHBOARD_TECHNITIUM_CA_BUNDLE"),

		NetboxURL:      os.Getenv("DASHBOARD_NETBOX_URL"),
		NetboxToken:    readToken("DASHBOARD_NETBOX_TOKEN_FILE", "DASHBOARD_NETBOX_TOKEN"),
		NetboxCABundle: os.Getenv("DASHBOARD_NETBOX_CA_BUNDLE"),

		DockerHost:       envOr("DASHBOARD_DOCKER_HOST", "unix:///var/run/docker.sock"),
		ContainerFilters: splitCSV(envOr("DASHBOARD_CONTAINER_FILTERS", "step-ca,technitium,netbox,dns-sync,authentik,keycloak,depot,sftpgo,seaweedfs,dashboard")),
		LogTail:          envInt("DASHBOARD_LOG_TAIL", 200),

		UpstreamTimeout: envDuration("DASHBOARD_UPSTREAM_TIMEOUT", 5*time.Second),
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

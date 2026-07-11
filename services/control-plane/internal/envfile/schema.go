package envfile

import "fmt"

// requirement binds one variable to its validator and the services that need
// it. The pseudo-service "common" is required by every deploy. This table is
// the Go port of the bash require_*_vars / validate_* functions; it grows one
// block per service as deployers are ported.
type requirement struct {
	Name       string
	RequiredBy []string
	Checks     []func(string) error
}

var schema = []requirement{
	// Common host identity (require_common_vars).
	{"HOST_IP", []string{"common"}, []func(string) error{checkCIDR}},
	{"SEARCH_DOMAIN", []string{"common"}, []func(string) error{checkFQDN}},
	{"WORKDIR", []string{"common"}, []func(string) error{checkAbsPath}},

	// Allowed client networks (require_allow_net_vars).
	{"ALLOW_NET_1", []string{"chrony"}, []func(string) error{checkCIDR}},
	{"ALLOW_NET_2", []string{"chrony"}, []func(string) error{checkCIDR}},
	{"ALLOW_NET_3", []string{"chrony"}, []func(string) error{checkCIDR}},

	// Chrony (require_ntp_vars plus the containerized image/dir).
	{"CHRONY_SERVER_1", []string{"chrony"}, []func(string) error{checkFQDN}},
	{"CHRONY_SERVER_2", []string{"chrony"}, []func(string) error{checkFQDN}},
	{"CHRONY_SERVER_3", []string{"chrony"}, []func(string) error{checkFQDN}},
	{"CHRONY_IMAGE", []string{"chrony"}, []func(string) error{checkImage}},
	{"CHRONY_DIR", []string{"chrony"}, []func(string) error{checkAbsPath}},

	// rsyslog (require_rsyslog_vars plus the containerized image).
	{"SYSLOG_PORT", []string{"rsyslog"}, []func(string) error{checkPort}},
	{"SYSLOG_LOG_DIR", []string{"rsyslog"}, []func(string) error{checkAbsPath}},
	{"RSYSLOG_IMAGE", []string{"rsyslog"}, []func(string) error{checkImage}},

	// step-ca (require_ca_vars). Cross-field rules (paths nested correctly,
	// RO role differs from owner) live in the CA deployer.
	{"CA_FQDN", []string{"ca"}, []func(string) error{checkFQDN}},
	{"CA_PORT", []string{"ca"}, []func(string) error{checkPort}},
	{"CA_DATA_DIR", []string{"ca"}, []func(string) error{checkAbsPath}},
	{"CA_NAME", []string{"ca"}, nil},
	{"CA_PROVISIONER_NAME", []string{"ca"}, nil},
	{"SERVICE_CERT_DURATION", []string{"ca"}, []func(string) error{checkNotPlaceholder, checkHourDuration}},
	{"CA_ENABLE_ACME", []string{"ca"}, []func(string) error{checkBool}},
	{"CA_IMAGE", []string{"ca"}, []func(string) error{checkImage}},
	{"CA_POSTGRES_IMAGE", []string{"ca"}, []func(string) error{checkImage}},
	{"CA_POSTGRES_DB", []string{"ca"}, []func(string) error{checkPgIdentifier}},
	{"CA_POSTGRES_USER", []string{"ca"}, []func(string) error{checkPgIdentifier}},
	{"CA_POSTGRES_PASSWORD", []string{"ca"}, []func(string) error{checkNotPlaceholder}},
	{"CA_POSTGRES_PORT", []string{"ca"}, []func(string) error{checkPort}},
	{"CA_POSTGRES_DATA_DIR", []string{"ca"}, []func(string) error{checkAbsPath}},
	{"CA_POSTGRES_RO_USER", []string{"ca"}, []func(string) error{checkPgIdentifier}},
	{"CA_POSTGRES_RO_PASSWORD", []string{"ca"}, []func(string) error{checkNotPlaceholder}},

	// SeaweedFS S3 (require_s3_vars).
	{"S3_FQDN", []string{"s3"}, []func(string) error{checkFQDN}},
	{"S3_PORT", []string{"s3"}, []func(string) error{checkPort}},
	{"S3_ACCESS_KEY", []string{"s3"}, []func(string) error{checkNotPlaceholder}},
	{"S3_SECRET_KEY", []string{"s3"}, []func(string) error{checkNotPlaceholder}},
	{"S3_DATA_DIR", []string{"s3"}, []func(string) error{checkAbsPath}},
	{"S3_IMAGE", []string{"s3"}, []func(string) error{checkImage}},
}

// Validate checks every variable required by the selected services (plus
// "common") and returns all findings at once, unlike bash's fail-fast, so the
// wizard can annotate the whole file in one pass.
func Validate(vars map[string]string, services []string) []Issue {
	want := map[string]bool{"common": true}
	for _, s := range services {
		want[s] = true
	}

	var issues []Issue
	for _, req := range schema {
		needed := false
		for _, svc := range req.RequiredBy {
			if want[svc] {
				needed = true
				break
			}
		}
		if !needed {
			continue
		}
		v, ok := vars[req.Name]
		if !ok || v == "" {
			issues = append(issues, Issue{req.Name, "missing required variable"})
			continue
		}
		for _, check := range req.Checks {
			if err := check(v); err != nil {
				issues = append(issues, Issue{req.Name, err.Error()})
			}
		}
	}
	return issues
}

// ValidateAll validates against every service in the schema; the wizard uses
// this so an uploaded config is checked completely, not per-selection.
func ValidateAll(vars map[string]string) []Issue {
	seen := map[string]bool{}
	var all []string
	for _, req := range schema {
		for _, svc := range req.RequiredBy {
			if svc != "common" && !seen[svc] {
				seen[svc] = true
				all = append(all, svc)
			}
		}
	}
	return Validate(vars, all)
}

// KnownService reports whether the schema knows the service name.
func KnownService(name string) error {
	for _, req := range schema {
		for _, svc := range req.RequiredBy {
			if svc == name {
				return nil
			}
		}
	}
	return fmt.Errorf("unknown service: %s", name)
}

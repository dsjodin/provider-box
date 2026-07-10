// Package technitium implements reconcile.Target against Technitium DNS Server.
//
// Endpoint shape verified against technitium/dns-server:13.4.2 - see
// services/dns-sync/TECHNITIUM_API.md. If you're updating this file because
// of a Technitium upgrade, re-run the probes captured there first.
package technitium

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/dsjodin/provider-box/services/dns-sync/internal/model"
)

const defaultTTL = 3600

// Target is the live Technitium reconcile.Target. Methods are safe for
// concurrent use; the underlying http.Client handles connection reuse.
type Target struct {
	BaseURL string
	Token   string
	TTL     int
	HTTP    *http.Client

	// DashboardReadonlyUser, when non-empty, is a non-admin Technitium username
	// granted View on each newly created zone (via zones/permissions/set) so the
	// read-only dashboard can list continuously-synced zones without a
	// --technitium re-run. Best-effort: grant failures are logged and never fail
	// reconcile. Empty disables the grant.
	DashboardReadonlyUser string

	// Logger receives best-effort, non-fatal messages (the dashboard zone grant).
	// Defaults to slog.Default() when nil.
	Logger *slog.Logger
}

func (t *Target) logger() *slog.Logger {
	if t.Logger != nil {
		return t.Logger
	}
	return slog.Default()
}

// New builds a Target. baseURL is the Technitium console root, e.g.
// "http://dns.sddc.lab:5380". caBundle is an optional PEM bundle path; pass ""
// to use the system trust store.
func New(baseURL, token, caBundle string) (*Target, error) {
	if baseURL == "" {
		return nil, errors.New("technitium base url is required")
	}
	if token == "" {
		return nil, errors.New("technitium token is required")
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if caBundle != "" {
		pem, err := os.ReadFile(caBundle)
		if err != nil {
			return nil, fmt.Errorf("read technitium CA bundle %s: %w", caBundle, err)
		}
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("no certificates parsed from %s", caBundle)
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	return &Target{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Token:   token,
		TTL:     defaultTTL,
		HTTP:    &http.Client{Transport: tr, Timeout: 30 * time.Second},
	}, nil
}

// envelope is the common Technitium response wrapper. The server always
// returns HTTP 200; the real status is in this body.
type envelope struct {
	Status       string          `json:"status"`
	ErrorMessage string          `json:"errorMessage,omitempty"`
	Response     json.RawMessage `json:"response,omitempty"`
}

func (t *Target) call(ctx context.Context, path string, params url.Values, out any) error {
	params.Set("token", t.Token)
	u := t.BaseURL + path + "?" + params.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	resp, err := t.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("technitium request: %w", err)
	}
	defer resp.Body.Close()

	var env envelope
	if err := json.NewDecoder(resp.Body).Decode(&env); err != nil {
		return fmt.Errorf("decode technitium response: %w", err)
	}
	if env.Status != "ok" {
		return &apiError{status: env.Status, message: env.ErrorMessage}
	}
	if out != nil && len(env.Response) > 0 {
		if err := json.Unmarshal(env.Response, out); err != nil {
			return fmt.Errorf("decode technitium response body: %w", err)
		}
	}
	return nil
}

type apiError struct {
	status  string
	message string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("technitium: status=%s message=%q", e.status, e.message)
}

// isZoneExistsError matches Technitium's non-idempotent zone-create reply so
// EnsureZone can treat "already exists" as success.
func isZoneExistsError(err error) bool {
	var a *apiError
	if !errors.As(err, &a) {
		return false
	}
	return a.status == "error" && strings.Contains(a.message, "Zone already exists")
}

// SetForwarder configures Technitium's upstream forwarder via /api/settings/set.
// Multiple values may be passed; they're joined with commas as the API expects.
// The endpoint is naturally idempotent - re-sending the same value is a no-op
// at the server - so no special "already set" handling is needed.
// See TECHNITIUM_API.md section 4.
func (t *Target) SetForwarder(ctx context.Context, upstreams ...string) error {
	if len(upstreams) == 0 {
		return errors.New("technitium: at least one forwarder is required")
	}
	for _, u := range upstreams {
		if strings.TrimSpace(u) == "" {
			return errors.New("technitium: empty forwarder value")
		}
	}
	params := url.Values{}
	params.Set("forwarders", strings.Join(upstreams, ","))
	if err := t.call(ctx, "/api/settings/set", params, nil); err != nil {
		return fmt.Errorf("set forwarders: %w", err)
	}
	return nil
}

// zoneEntry mirrors one element of /api/zones/list.
type zoneEntry struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Internal bool   `json:"internal"`
	Disabled bool   `json:"disabled"`
}

type zoneListResp struct {
	Zones []zoneEntry `json:"zones"`
}

type recordEntry struct {
	Name  string          `json:"name"`
	Type  string          `json:"type"`
	TTL   int             `json:"ttl"`
	RData json.RawMessage `json:"rData"`
}

type recordsResp struct {
	Records []recordEntry `json:"records"`
}

// List returns the records and zones Technitium currently has, filtered to
// the record types dns-sync manages and to non-internal zones.
func (t *Target) List(ctx context.Context) ([]model.Record, []string, error) {
	var zl zoneListResp
	if err := t.call(ctx, "/api/zones/list", url.Values{}, &zl); err != nil {
		return nil, nil, fmt.Errorf("list zones: %w", err)
	}

	zones := make([]string, 0, len(zl.Zones))
	records := []model.Record{}
	for _, z := range zl.Zones {
		if z.Internal {
			continue
		}
		zoneFQDN := model.NormalizeFQDN(z.Name)
		zones = append(zones, zoneFQDN)

		var rr recordsResp
		params := url.Values{}
		params.Set("zone", z.Name)
		params.Set("domain", z.Name)
		params.Set("listZone", "true")
		if err := t.call(ctx, "/api/zones/records/get", params, &rr); err != nil {
			return nil, nil, fmt.Errorf("get records for %s: %w", z.Name, err)
		}
		for _, r := range rr.Records {
			rec, ok := decodeRecord(zoneFQDN, r)
			if !ok {
				continue
			}
			records = append(records, rec)
		}
	}
	return records, zones, nil
}

func decodeRecord(zoneFQDN string, r recordEntry) (model.Record, bool) {
	switch r.Type {
	case "A", "AAAA":
		var d struct {
			IPAddress string `json:"ipAddress"`
		}
		if err := json.Unmarshal(r.RData, &d); err != nil || d.IPAddress == "" {
			return model.Record{}, false
		}
		return model.Record{
			Zone: zoneFQDN,
			Name: model.NormalizeFQDN(r.Name),
			Type: r.Type,
			Data: d.IPAddress,
			TTL:  r.TTL,
		}, true
	case "PTR":
		var d struct {
			PtrName string `json:"ptrName"`
		}
		if err := json.Unmarshal(r.RData, &d); err != nil || d.PtrName == "" {
			return model.Record{}, false
		}
		return model.Record{
			Zone: zoneFQDN,
			Name: model.NormalizeFQDN(r.Name),
			Type: r.Type,
			Data: model.NormalizeFQDN(d.PtrName),
			TTL:  r.TTL,
		}, true
	default:
		// NS, SOA, and anything else are not dns-sync-managed; ignore.
		return model.Record{}, false
	}
}

// Apply executes the ops. EnsureZone is performed first so subsequent Creates
// have their parent zone present; "Zone already exists" is treated as success.
// Creates and Deletes are idempotent on the Technitium side (verified, see
// TECHNITIUM_API.md), so a partial replay does not corrupt state.
func (t *Target) Apply(ctx context.Context, ops []model.Op) error {
	for _, op := range ops {
		if op.Kind != model.OpEnsureZone {
			continue
		}
		if err := t.ensureZone(ctx, op.Zone); err != nil {
			return err
		}
	}
	for _, op := range ops {
		if op.Kind != model.OpCreate {
			continue
		}
		if err := t.addRecord(ctx, op.Record); err != nil {
			return err
		}
	}
	for _, op := range ops {
		if op.Kind != model.OpDelete {
			continue
		}
		if err := t.deleteRecord(ctx, op.Record); err != nil {
			return err
		}
	}
	return nil
}

func (t *Target) ensureZone(ctx context.Context, zoneFQDN string) error {
	name := strings.TrimSuffix(zoneFQDN, ".")
	params := url.Values{}
	params.Set("zone", name)
	params.Set("type", "Primary")
	err := t.call(ctx, "/api/zones/create", params, nil)
	switch {
	case err == nil:
		// Zone newly created: best-effort grant so the read-only dashboard lists
		// it without a --technitium re-run. Only fires on fresh creation (an
		// existing zone was granted at its creation, or by --technitium's bulk
		// grant). Never fails zone creation.
		t.grantDashboardZoneView(ctx, name)
		return nil
	case isZoneExistsError(err):
		return nil
	default:
		return fmt.Errorf("create zone %s: %w", name, err)
	}
}

// grantDashboardZoneView best-effort grants the configured dashboard read-only
// user View permission on zone via zones/permissions/set, mirroring
// provision_technitium_dashboard_token in bootstrap/technitium.sh. Only
// userPermissions is sent, so Technitium leaves the zone's group permissions
// (Administrators/DNS Administrators) untouched; the admin creator is re-sent so
// it is not dropped from the user table. Non-fatal by contract: an empty
// DashboardReadonlyUser skips it entirely, a missing dashboard user is silently
// ignored by Technitium (unknown users are skipped when syncing permissions),
// and any transport or API error is logged and swallowed so reconcile never
// breaks on an optional dashboard grant.
func (t *Target) grantDashboardZoneView(ctx context.Context, zone string) {
	if t.DashboardReadonlyUser == "" {
		return
	}
	params := url.Values{}
	params.Set("zone", zone)
	params.Set("userPermissions", "admin|true|true|true|"+t.DashboardReadonlyUser+"|true|false|false")
	if err := t.call(ctx, "/api/zones/permissions/set", params, nil); err != nil {
		t.logger().Warn("dashboard zone grant failed; continuing", "zone", zone, "user", t.DashboardReadonlyUser, "err", err)
		return
	}
	t.logger().Debug("granted dashboard read access to zone", "zone", zone, "user", t.DashboardReadonlyUser)
}

func (t *Target) addRecord(ctx context.Context, r model.Record) error {
	params := url.Values{}
	params.Set("zone", strings.TrimSuffix(r.Zone, "."))
	params.Set("domain", strings.TrimSuffix(r.Name, "."))
	params.Set("type", r.Type)
	params.Set("ttl", strconv.Itoa(ttlOr(r.TTL, t.TTL)))
	switch r.Type {
	case "A", "AAAA":
		params.Set("ipAddress", r.Data)
	case "PTR":
		params.Set("ptrName", strings.TrimSuffix(r.Data, "."))
	default:
		return fmt.Errorf("technitium: unsupported record type %q", r.Type)
	}
	if err := t.call(ctx, "/api/zones/records/add", params, nil); err != nil {
		return fmt.Errorf("add %s %s: %w", r.Type, r.Name, err)
	}
	return nil
}

func (t *Target) deleteRecord(ctx context.Context, r model.Record) error {
	params := url.Values{}
	params.Set("zone", strings.TrimSuffix(r.Zone, "."))
	params.Set("domain", strings.TrimSuffix(r.Name, "."))
	params.Set("type", r.Type)
	switch r.Type {
	case "A", "AAAA":
		params.Set("ipAddress", r.Data)
	case "PTR":
		params.Set("ptrName", strings.TrimSuffix(r.Data, "."))
	default:
		return fmt.Errorf("technitium: unsupported record type %q", r.Type)
	}
	if err := t.call(ctx, "/api/zones/records/delete", params, nil); err != nil {
		return fmt.Errorf("delete %s %s: %w", r.Type, r.Name, err)
	}
	return nil
}

func ttlOr(v, def int) int {
	if v > 0 {
		return v
	}
	return def
}

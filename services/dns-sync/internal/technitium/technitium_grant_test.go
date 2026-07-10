package technitium

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
)

// newTestServer records every request path+query and lets each test decide the
// JSON body to return per path. Technitium always answers HTTP 200; the status
// field carries success/failure.
type recordingServer struct {
	mu    sync.Mutex
	calls []url.Values
	body  func(path string, q url.Values) string
	srv   *httptest.Server
}

func newRecordingServer(body func(path string, q url.Values) string) *recordingServer {
	rs := &recordingServer{body: body}
	rs.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		rs.mu.Lock()
		q.Set("__path", r.URL.Path)
		rs.calls = append(rs.calls, q)
		rs.mu.Unlock()
		_, _ = io.WriteString(w, rs.body(r.URL.Path, q))
	}))
	return rs
}

func (rs *recordingServer) find(path string) (url.Values, bool) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	for _, c := range rs.calls {
		if c.Get("__path") == path {
			return c, true
		}
	}
	return nil, false
}

func newTargetFor(rs *recordingServer, dashboardUser string) *Target {
	tt, _ := New(rs.srv.URL, "admin-token", "")
	tt.DashboardReadonlyUser = dashboardUser
	return tt
}

func TestEnsureZone_GrantsDashboardUserOnCreate(t *testing.T) {
	rs := newRecordingServer(func(path string, q url.Values) string {
		return `{"status":"ok","response":{}}`
	})
	defer rs.srv.Close()

	tt := newTargetFor(rs, "dashboard")
	if err := tt.ensureZone(context.Background(), "lab.io."); err != nil {
		t.Fatalf("ensureZone: %v", err)
	}

	grant, ok := rs.find("/api/zones/permissions/set")
	if !ok {
		t.Fatal("expected a zones/permissions/set call after zone creation")
	}
	if got := grant.Get("zone"); got != "lab.io" {
		t.Errorf("grant zone = %q, want lab.io", got)
	}
	// Same shape as provision_technitium_dashboard_token: admin creator preserved,
	// dashboard user added View-only, and no groupPermissions so group access is
	// left untouched.
	if got := grant.Get("userPermissions"); got != "admin|true|true|true|dashboard|true|false|false" {
		t.Errorf("userPermissions = %q", got)
	}
	if _, has := grant["groupPermissions"]; has {
		t.Error("groupPermissions must not be sent (would overwrite the zone's group access)")
	}
}

func TestEnsureZone_SkipsGrantWhenUnconfigured(t *testing.T) {
	rs := newRecordingServer(func(path string, q url.Values) string {
		return `{"status":"ok","response":{}}`
	})
	defer rs.srv.Close()

	tt := newTargetFor(rs, "") // dashboard user not configured
	if err := tt.ensureZone(context.Background(), "lab.io."); err != nil {
		t.Fatalf("ensureZone: %v", err)
	}
	if _, ok := rs.find("/api/zones/permissions/set"); ok {
		t.Error("no grant call expected when DashboardReadonlyUser is empty")
	}
}

func TestEnsureZone_GrantFailureIsNonFatal(t *testing.T) {
	rs := newRecordingServer(func(path string, q url.Values) string {
		if strings.HasSuffix(path, "/permissions/set") {
			return `{"status":"error","errorMessage":"boom"}`
		}
		return `{"status":"ok","response":{}}`
	})
	defer rs.srv.Close()

	tt := newTargetFor(rs, "dashboard")
	// A failing grant must not fail zone creation.
	if err := tt.ensureZone(context.Background(), "lab.io."); err != nil {
		t.Fatalf("ensureZone must ignore grant failure, got: %v", err)
	}
	if _, ok := rs.find("/api/zones/permissions/set"); !ok {
		t.Fatal("expected the grant to be attempted")
	}
}

func TestEnsureZone_NoGrantWhenZoneAlreadyExists(t *testing.T) {
	rs := newRecordingServer(func(path string, q url.Values) string {
		if strings.HasSuffix(path, "/zones/create") {
			return `{"status":"error","errorMessage":"Zone already exists: lab.io"}`
		}
		return `{"status":"ok","response":{}}`
	})
	defer rs.srv.Close()

	tt := newTargetFor(rs, "dashboard")
	if err := tt.ensureZone(context.Background(), "lab.io."); err != nil {
		t.Fatalf("ensureZone: %v", err)
	}
	if _, ok := rs.find("/api/zones/permissions/set"); ok {
		t.Error("grant must fire only on fresh creation, not when the zone already exists")
	}
}

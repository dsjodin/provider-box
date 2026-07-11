package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/dsjodin/provider-box/services/control-plane/internal/certs"
	"github.com/dsjodin/provider-box/services/control-plane/internal/dns"
	"github.com/dsjodin/provider-box/services/control-plane/internal/docker"
	"github.com/dsjodin/provider-box/services/control-plane/internal/ipam"
)

type stubCerts struct {
	out []certs.Cert
	err error
}

func (s stubCerts) List(context.Context) ([]certs.Cert, error) { return s.out, s.err }

type stubDNS struct {
	out dns.Overview
	err error
}

func (s stubDNS) Fetch(context.Context) (dns.Overview, error) { return s.out, s.err }

type stubIPAM struct {
	out ipam.Overview
	err error
}

func (s stubIPAM) Fetch(context.Context) (ipam.Overview, error) { return s.out, s.err }

type stubDocker struct {
	list    []docker.Container
	listErr error
	lines   []string
}

func (s stubDocker) List(context.Context, []string, time.Time) ([]docker.Container, error) {
	return s.list, s.listErr
}
func (s stubDocker) LogLines(context.Context, string, int) ([]string, error) {
	return s.lines, nil
}

func testServer(t *testing.T, opt Options) *Server {
	t.Helper()
	opt.Now = func() time.Time { return time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC) }
	opt.WarnDays = 30
	srv, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

// All sources up: every panel is ok and the errors panel picks up a JSON
// ERROR line tailed from a running container.
func TestCollect_AllUp(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	srv := testServer(t, Options{
		Certs: stubCerts{out: []certs.Cert{{CommonName: "ca.sddc.lab", NotAfter: now.Add(100 * 24 * time.Hour)}}},
		DNS:   stubDNS{out: dns.Overview{Zones: []dns.ZoneInfo{{Name: "sddc.lab", RecordCount: 3}}, TLSReachable: true}},
		IPAM:  stubIPAM{out: ipam.Overview{PrefixCount: 2, IPCount: 5, DNSNames: []string{"a.sddc.lab"}}},
		Docker: stubDocker{
			list:  []docker.Container{{ID: "x", Name: "dns-sync", State: "running"}},
			lines: []string{`{"level":"ERROR","msg":"reconcile failed"}`},
		},
	})

	page := srv.collect(context.Background())
	for name, st := range map[string]Status{
		"certs": page.Certs.Status, "dns": page.DNS.Status, "ipam": page.IPAM.Status,
		"services": page.Services.Status, "errors": page.Errors.Status,
	} {
		if !st.OK() {
			t.Errorf("%s panel not ok: %+v", name, st)
		}
	}
	if len(page.Errors.Entries) != 1 || page.Errors.Entries[0].Message != "reconcile failed" {
		t.Errorf("errors panel entries: %+v", page.Errors.Entries)
	}
	if page.Certs.Summary.ActiveOK != 1 {
		t.Errorf("certs summary: %+v", page.Certs.Summary)
	}
}

// One source down must not affect the others; its panel is unavailable.
func TestCollect_Isolation(t *testing.T) {
	srv := testServer(t, Options{
		Certs:  stubCerts{err: errors.New("connect stepca postgres failed")},
		DNS:    stubDNS{out: dns.Overview{TLSReachable: true}},
		IPAM:   stubIPAM{err: errors.New("netbox 500")},
		Docker: stubDocker{listErr: errors.New("dial /var/run/docker.sock: no such file")},
	})

	page := srv.collect(context.Background())

	if !page.Certs.Status.Unavailable() || !strings.Contains(page.Certs.Status.Error, "postgres") {
		t.Errorf("certs should be unavailable with error, got %+v", page.Certs.Status)
	}
	if !page.DNS.Status.OK() {
		t.Errorf("dns should stay ok despite other failures, got %+v", page.DNS.Status)
	}
	if !page.IPAM.Status.Unavailable() {
		t.Errorf("ipam should be unavailable, got %+v", page.IPAM.Status)
	}
	// Docker failure degrades both services and errors together.
	if !page.Services.Status.Unavailable() || !page.Errors.Status.Unavailable() {
		t.Errorf("docker panels should both be unavailable: svc=%+v err=%+v",
			page.Services.Status, page.Errors.Status)
	}
}

// Nil providers render as "not configured", never as errors.
func TestCollect_NotConfigured(t *testing.T) {
	srv := testServer(t, Options{})
	page := srv.collect(context.Background())
	for name, st := range map[string]Status{
		"certs": page.Certs.Status, "dns": page.DNS.Status, "ipam": page.IPAM.Status,
		"services": page.Services.Status, "errors": page.Errors.Status,
	} {
		if !st.Disabled() {
			t.Errorf("%s should be disabled, got %+v", name, st)
		}
	}
}

// The HTML page and JSON API render for a mixed up/down/disabled state.
func TestHandlers_Render(t *testing.T) {
	srv := testServer(t, Options{
		DNS:  stubDNS{out: dns.Overview{Zones: []dns.ZoneInfo{{Name: "sddc.lab"}}, TLSReachable: true}},
		IPAM: stubIPAM{err: errors.New("down")},
	})
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Provider Box") {
		t.Fatalf("index render: code=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "Source unavailable") {
		t.Errorf("expected unavailable IPAM panel in HTML")
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/state", nil))
	var page Page
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatalf("json api: %v", err)
	}
	if !page.DNS.Status.OK() || !page.IPAM.Status.Unavailable() {
		t.Errorf("json state mismatch: dns=%+v ipam=%+v", page.DNS.Status, page.IPAM.Status)
	}
}

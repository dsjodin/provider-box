package dns

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// Recorded Technitium response bodies (see services/dns-sync/TECHNITIUM_API.md).
const (
	zonesListBody = `{"response":{"zones":[
		{"name":"sddc.lab","type":"Primary","internal":false},
		{"name":"0.0.10.in-addr.arpa","type":"Primary","internal":false},
		{"name":"127.in-addr.arpa","type":"Primary","internal":true}
	]},"status":"ok"}`

	recordsSddcBody = `{"response":{"zone":{"name":"sddc.lab"},"records":[
		{"name":"sddc.lab","type":"SOA","ttl":900,"rData":{}},
		{"name":"sddc.lab","type":"NS","ttl":3600,"rData":{}},
		{"name":"a.sddc.lab","type":"A","ttl":3600,"rData":{"ipAddress":"10.0.0.10"}},
		{"name":"b.sddc.lab","type":"A","ttl":3600,"rData":{"ipAddress":"10.0.0.11"}}
	]},"status":"ok"}`

	recordsReverseBody = `{"response":{"records":[
		{"name":"0.0.10.in-addr.arpa","type":"SOA","rData":{}},
		{"name":"0.0.10.in-addr.arpa","type":"NS","rData":{}},
		{"name":"10.0.0.10.in-addr.arpa","type":"PTR","rData":{"ptrName":"a.sddc.lab"}}
	]},"status":"ok"}`

	settingsBody = `{"response":{"forwarders":["8.8.8.8"],"forwarderProtocol":"Udp",
		"webServiceEnableTls":true,"webServiceTlsPort":53443},"status":"ok"}`

	invalidTokenBody = `{"status":"invalid-token","errorMessage":"Invalid token or session expired."}`
)

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New(srv.URL, "tok", "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestFetch_OK(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/zones/list":
			_, _ = w.Write([]byte(zonesListBody))
		case "/api/zones/records/get":
			if r.URL.Query().Get("zone") == "sddc.lab" {
				_, _ = w.Write([]byte(recordsSddcBody))
			} else {
				_, _ = w.Write([]byte(recordsReverseBody))
			}
		case "/api/settings/get":
			_, _ = w.Write([]byte(settingsBody))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})

	ov, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ov.Zones) != 2 {
		t.Fatalf("internal zone not filtered: got %d zones", len(ov.Zones))
	}
	// Sorted by name: 0.0.10.in-addr.arpa before sddc.lab.
	if ov.Zones[0].Name != "0.0.10.in-addr.arpa" || ov.Zones[0].RecordCount != 1 {
		t.Errorf("reverse zone: %+v (want 1 managed PTR, NS/SOA excluded)", ov.Zones[0])
	}
	if ov.Zones[1].Name != "sddc.lab" || ov.Zones[1].RecordCount != 2 {
		t.Errorf("forward zone: %+v (want 2 A records, NS/SOA excluded)", ov.Zones[1])
	}
	if len(ov.Forwarders) != 1 || ov.Forwarders[0] != "8.8.8.8" {
		t.Errorf("forwarders: %v", ov.Forwarders)
	}
	if !ov.TLSEnabled || ov.TLSPort != 53443 || !ov.TLSReachable {
		t.Errorf("tls: enabled=%v port=%d reachable=%v", ov.TLSEnabled, ov.TLSPort, ov.TLSReachable)
	}
}

func TestFetch_InvalidToken(t *testing.T) {
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(invalidTokenBody))
	})
	if _, err := c.Fetch(context.Background()); err == nil {
		t.Fatal("expected error on invalid-token status")
	}
}

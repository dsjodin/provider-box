package docker

import (
	"context"
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestList_FilterAndParse(t *testing.T) {
	body := `[
		{"Id":"a1","Names":["/netbox-netbox-1"],"Image":"netboxcommunity/netbox:v4.6.2",
		 "State":"running","Status":"Up 2 hours (healthy)","Created":1000,
		 "Labels":{"com.docker.compose.project":"netbox"}},
		{"Id":"b2","Names":["/technitium"],"Image":"technitium/dns-server:15.3.0",
		 "State":"exited","Status":"Exited (0) 5 minutes ago","Created":900,"Labels":{}},
		{"Id":"c3","Names":["/some-unrelated-thing"],"Image":"redis:7","State":"running",
		 "Status":"Up 1 hour","Created":800,"Labels":{}}
	]`
	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/json" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(body))
	})

	now := time.Unix(1000+3600, 0)
	got, err := c.List(context.Background(), []string{"netbox", "technitium"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("filter kept %d containers, want 2", len(got))
	}
	// Sorted by name: netbox-netbox-1 before technitium.
	nb := got[0]
	if nb.Name != "netbox-netbox-1" || nb.Health != "healthy" || nb.Project != "netbox" {
		t.Errorf("netbox container: %+v", nb)
	}
	if nb.Uptime != "1h0m" {
		t.Errorf("uptime = %q, want 1h0m", nb.Uptime)
	}
	if got[1].State != "exited" || got[1].Uptime != "" {
		t.Errorf("exited container should have no uptime: %+v", got[1])
	}
}

func TestLogLines_Demux(t *testing.T) {
	frame := func(stream byte, payload string) []byte {
		h := make([]byte, 8)
		h[0] = stream
		binary.BigEndian.PutUint32(h[4:], uint32(len(payload)))
		return append(h, []byte(payload)...)
	}
	var stream []byte
	stream = append(stream, frame(1, "line one\n")...)
	stream = append(stream, frame(2, "line two\n")...)

	c := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/containers/x/logs" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write(stream)
	})

	lines, err := c.LogLines(context.Background(), "x", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 2 || lines[0] != "line one" || lines[1] != "line two" {
		t.Errorf("demuxed lines = %v", lines)
	}
}

func TestParseHealth(t *testing.T) {
	cases := map[string]string{
		"Up 2 hours (healthy)":            "healthy",
		"Up 3 minutes (unhealthy)":        "unhealthy",
		"Up 5 seconds (health: starting)": "starting",
		"Up 1 hour":                       "",
		"Exited (0) 5 minutes ago":        "",
	}
	for status, want := range cases {
		if got := parseHealth(status); got != want {
			t.Errorf("parseHealth(%q) = %q, want %q", status, got, want)
		}
	}
}

func newTestClient(t *testing.T, h http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := New("tcp://"+srv.Listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

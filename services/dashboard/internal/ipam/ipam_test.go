package ipam

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestFetch_CountsAndDNSNames(t *testing.T) {
	var base string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Token tok" {
			t.Errorf("auth header = %q, want legacy Token form", got)
		}
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/ipam/prefixes/"):
			_, _ = w.Write([]byte(`{"count":7,"next":null,"results":[{}]}`))
		case strings.Contains(r.URL.RawQuery, "page=2"):
			_, _ = w.Write([]byte(`{"count":3,"next":null,"results":[
				{"address":"10.0.0.12/24","dns_name":"a.sddc.lab"},
				{"address":"10.0.0.13/24","dns_name":""}
			]}`))
		case r.URL.Path == "/api/ipam/ip-addresses/":
			// First page of two; next points back to this server.
			_, _ = fmt.Fprintf(w, `{"count":3,"next":%q,"results":[
				{"address":"10.0.0.10/24","dns_name":"a.sddc.lab"},
				{"address":"10.0.0.11/24","dns_name":"b.sddc.lab"}
			]}`, base+"/api/ipam/ip-addresses/?page=2")
		default:
			t.Errorf("unexpected request %s?%s", r.URL.Path, r.URL.RawQuery)
		}
	}))
	defer srv.Close()
	base = srv.URL

	c, err := New(srv.URL, "tok", "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	ov, err := c.Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if ov.PrefixCount != 7 {
		t.Errorf("PrefixCount = %d, want 7", ov.PrefixCount)
	}
	if ov.IPCount != 3 {
		t.Errorf("IPCount = %d, want 3", ov.IPCount)
	}
	// Duplicate a.sddc.lab collapsed, blank excluded, sorted.
	want := []string{"a.sddc.lab", "b.sddc.lab"}
	if len(ov.DNSNames) != len(want) || ov.DNSNames[0] != want[0] || ov.DNSNames[1] != want[1] {
		t.Errorf("DNSNames = %v, want %v", ov.DNSNames, want)
	}
}

func TestAuthHeader_V2Bearer(t *testing.T) {
	c := &Client{Token: "nbt_abc.def"}
	if got := c.authHeader(); got != "Bearer nbt_abc.def" {
		t.Errorf("authHeader = %q, want Bearer for v2 composite token", got)
	}
}

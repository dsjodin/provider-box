package netbox

import (
	"reflect"
	"sort"
	"testing"
)

func TestBuildRecords_CanonicalPTRAndIPv4Only(t *testing.T) {
	ips := []ipAddress{
		// Two FQDNs on the same IP; lex-smallest must win the PTR.
		{Address: "10.0.0.10/24", DNSName: "zeta.lab.test"},
		{Address: "10.0.0.10/24", DNSName: "alpha.lab.test"},
		{Address: "10.0.0.11/24", DNSName: "bravo.lab.test"},
		// IPv6 must be skipped entirely - no AAAA, no PTR.
		{Address: "fd00::1/64", DNSName: "v6.lab.test"},
		// Empty DNS name must be skipped.
		{Address: "10.0.0.99/24", DNSName: ""},
	}

	got := buildRecords(ips)

	type rec struct{ Zone, Name, Type, Data string }
	flatten := func(rs []rec) []rec {
		sort.Slice(rs, func(i, j int) bool {
			if rs[i].Type != rs[j].Type {
				return rs[i].Type < rs[j].Type
			}
			if rs[i].Name != rs[j].Name {
				return rs[i].Name < rs[j].Name
			}
			return rs[i].Data < rs[j].Data
		})
		return rs
	}

	flat := make([]rec, 0, len(got))
	for _, r := range got {
		flat = append(flat, rec{Zone: r.Zone, Name: r.Name, Type: r.Type, Data: r.Data})
	}
	flat = flatten(flat)

	want := flatten([]rec{
		// Both A records emitted (NetBox can hold two IPs at the same address).
		{Zone: "lab.test.", Name: "alpha.lab.test.", Type: "A", Data: "10.0.0.10"},
		{Zone: "lab.test.", Name: "zeta.lab.test.", Type: "A", Data: "10.0.0.10"},
		{Zone: "lab.test.", Name: "bravo.lab.test.", Type: "A", Data: "10.0.0.11"},
		// Exactly one PTR per IP, canonical (lex-smallest) name wins.
		{Zone: "0.0.10.in-addr.arpa.", Name: "10.0.0.10.in-addr.arpa.", Type: "PTR", Data: "alpha.lab.test."},
		{Zone: "0.0.10.in-addr.arpa.", Name: "11.0.0.10.in-addr.arpa.", Type: "PTR", Data: "bravo.lab.test."},
	})

	if !reflect.DeepEqual(flat, want) {
		t.Errorf("records mismatch\n got: %#v\nwant: %#v", flat, want)
	}

	for _, r := range got {
		if r.Type == "AAAA" {
			t.Errorf("unexpected AAAA record emitted: %#v", r)
		}
	}
}

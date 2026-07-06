package seed

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func TestParse_Basic(t *testing.T) {
	input := `
# this is a comment
host1.lab.test 10.0.0.10
host2.lab.test 10.0.0.11/24

# blank above is fine
HOST3.LAB.TEST 10.0.0.12/24
`
	got, err := Parse(strings.NewReader(input))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []Entry{
		{FQDN: "host1.lab.test", Addr: netip.MustParseAddr("10.0.0.10")},
		{FQDN: "host2.lab.test", Addr: netip.MustParseAddr("10.0.0.11"), Prefix: netip.MustParsePrefix("10.0.0.0/24")},
		{FQDN: "host3.lab.test", Addr: netip.MustParseAddr("10.0.0.12"), Prefix: netip.MustParsePrefix("10.0.0.0/24")},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("entries mismatch\n got: %#v\nwant: %#v", got, want)
	}
}

func TestParse_Errors(t *testing.T) {
	cases := []string{
		"bare-fqdn-only",
		"host 999.999.999.999",
		"host 10.0.0.0/99",
		"host one two three",
	}
	for _, c := range cases {
		_, err := Parse(strings.NewReader(c))
		if err == nil {
			t.Errorf("expected error for %q, got nil", c)
		}
	}
}

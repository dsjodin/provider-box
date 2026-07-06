// Package seed parses the config/dns.seed format (compatible with 1.0's
// unbound.records) into typed entries. Reused by every command that consumes
// the seed - NetBox import, Technitium direct apply, validation.
package seed

import (
	"bufio"
	"fmt"
	"io"
	"net/netip"
	"strings"
)

// Entry is one seed line. Prefix is only set when the seed line carried a
// CIDR; consumers must treat an invalid Prefix as "no subnet known" and not
// guess - matches the AGENTS.md "no subnet assumptions" rule.
type Entry struct {
	FQDN   string
	Addr   netip.Addr
	Prefix netip.Prefix
}

func (e Entry) HasPrefix() bool { return e.Prefix.IsValid() }

// Parse reads seed lines from r. Format per line:
//
//	<fqdn> <ip>          - host only
//	<fqdn> <ip/cidr>     - host plus subnet
//
// Comments start with '#'; blank lines are ignored. Returns the first parse
// error rather than continuing on, so a malformed seed fails the bootstrap
// loudly instead of silently dropping records.
func Parse(r io.Reader) ([]Entry, error) {
	out := []Entry{}
	s := bufio.NewScanner(r)
	lineNo := 0
	for s.Scan() {
		lineNo++
		text := strings.TrimSpace(s.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		fields := strings.Fields(text)
		if len(fields) != 2 {
			return nil, fmt.Errorf("seed line %d: expected '<fqdn> <ip[/cidr]>', got %q", lineNo, text)
		}
		fqdn := strings.ToLower(fields[0])
		if fqdn == "" {
			return nil, fmt.Errorf("seed line %d: empty FQDN", lineNo)
		}
		e := Entry{FQDN: fqdn}
		if strings.Contains(fields[1], "/") {
			p, err := netip.ParsePrefix(fields[1])
			if err != nil {
				return nil, fmt.Errorf("seed line %d: invalid CIDR %q: %w", lineNo, fields[1], err)
			}
			e.Addr = p.Addr()
			e.Prefix = p.Masked()
		} else {
			a, err := netip.ParseAddr(fields[1])
			if err != nil {
				return nil, fmt.Errorf("seed line %d: invalid IP %q: %w", lineNo, fields[1], err)
			}
			e.Addr = a
		}
		out = append(out, e)
	}
	if err := s.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

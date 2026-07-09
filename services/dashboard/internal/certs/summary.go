package certs

import (
	"sort"
	"time"
)

// View is one certificate shaped for the dashboard panel.
type View struct {
	CommonName   string
	SANs         []string
	Provisioner  string
	NotBefore    time.Time
	NotAfter     time.Time
	DaysToExpiry int
	Warn         bool // active and within warnDays of expiry
	Expired      bool
	Revoked      bool
}

// Summary is the shaped certificates panel.
type Summary struct {
	Certs        []View
	ActiveOK     int // active, not within the warn window
	WarnCount    int // active, within warnDays of expiry
	ExpiredCount int
	RevokedCount int
}

// Summarize shapes raw certs for the panel: computes days-to-expiry against
// now, flags certs expiring within warnDays, and sorts soonest-expiry first.
// Revoked and expired certs are retained but flagged, not silently dropped.
func Summarize(raw []Cert, now time.Time, warnDays int) Summary {
	var s Summary
	for _, c := range raw {
		days := int(c.NotAfter.Sub(now).Hours() / 24)
		v := View{
			CommonName:   c.CommonName,
			SANs:         c.SANs,
			Provisioner:  c.Provisioner,
			NotBefore:    c.NotBefore,
			NotAfter:     c.NotAfter,
			DaysToExpiry: days,
			Revoked:      c.Revoked,
		}
		switch {
		case c.Revoked:
			s.RevokedCount++
		case !now.Before(c.NotAfter):
			v.Expired = true
			s.ExpiredCount++
		case days < warnDays:
			v.Warn = true
			s.WarnCount++
		default:
			s.ActiveOK++
		}
		s.Certs = append(s.Certs, v)
	}
	sort.SliceStable(s.Certs, func(i, j int) bool {
		return s.Certs[i].NotAfter.Before(s.Certs[j].NotAfter)
	})
	return s
}

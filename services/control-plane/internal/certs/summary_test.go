package certs

import (
	"testing"
	"time"
)

func TestSummarize(t *testing.T) {
	now := time.Date(2026, 7, 9, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	raw := []Cert{
		{CommonName: "healthy.sddc.lab", NotAfter: now.Add(100 * day)},
		{CommonName: "soon.sddc.lab", NotAfter: now.Add(10 * day)},
		{CommonName: "gone.sddc.lab", NotAfter: now.Add(-2 * day)},
		{CommonName: "revoked.sddc.lab", NotAfter: now.Add(50 * day), Revoked: true},
	}

	got := Summarize(raw, now, 30)

	if got.ActiveOK != 1 || got.WarnCount != 1 || got.ExpiredCount != 1 || got.RevokedCount != 1 {
		t.Fatalf("counts: active=%d warn=%d expired=%d revoked=%d",
			got.ActiveOK, got.WarnCount, got.ExpiredCount, got.RevokedCount)
	}

	// Soonest-expiry first: expired (-2d) leads, then soon (10d).
	if got.Certs[0].CommonName != "gone.sddc.lab" {
		t.Errorf("expected soonest-expiry first, got %q", got.Certs[0].CommonName)
	}

	byCN := map[string]View{}
	for _, v := range got.Certs {
		byCN[v.CommonName] = v
	}
	if v := byCN["soon.sddc.lab"]; !v.Warn || v.DaysToExpiry != 10 {
		t.Errorf("soon: warn=%v days=%d, want warn=true days=10", v.Warn, v.DaysToExpiry)
	}
	if v := byCN["gone.sddc.lab"]; !v.Expired {
		t.Errorf("gone should be flagged expired")
	}
	if v := byCN["revoked.sddc.lab"]; !v.Revoked || v.Warn || v.Expired {
		t.Errorf("revoked cert must be revoked-only, got %+v", v)
	}
	if v := byCN["healthy.sddc.lab"]; v.Warn || v.Expired || v.Revoked {
		t.Errorf("healthy cert should carry no flags, got %+v", v)
	}
}

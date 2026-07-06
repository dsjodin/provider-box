//go:build integration

// Integration test for TechnitiumTarget against a live Technitium server.
// Skipped unless TECHNITIUM_URL and TECHNITIUM_TOKEN are set.
//
// Run with:
//   TECHNITIUM_URL=http://127.0.0.1:5380 \
//   TECHNITIUM_TOKEN=... \
//   go test -tags integration ./internal/technitium
package technitium

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/dsjodin/provider-box/services/dns-sync/internal/model"
)

func testTarget(t *testing.T) *Target {
	t.Helper()
	urlStr := os.Getenv("TECHNITIUM_URL")
	token := os.Getenv("TECHNITIUM_TOKEN")
	if urlStr == "" || token == "" {
		t.Skip("TECHNITIUM_URL and TECHNITIUM_TOKEN must be set")
	}
	tgt, err := New(urlStr, token, "")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return tgt
}

func cleanupZones(t *testing.T, tgt *Target, names ...string) {
	t.Helper()
	for _, n := range names {
		params := url.Values{}
		params.Set("zone", n)
		_ = tgt.call(context.Background(), "/api/zones/delete", params, nil)
	}
}

func TestApply_CreatesIdempotentlyAndConverges(t *testing.T) {
	tgt := testTarget(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	const fwdZone = "itest.local."
	const revZone = "0.0.10.in-addr.arpa."
	cleanupZones(t, tgt, "itest.local", "0.0.10.in-addr.arpa")
	t.Cleanup(func() { cleanupZones(t, tgt, "itest.local", "0.0.10.in-addr.arpa") })

	ops := []model.Op{
		{Kind: model.OpEnsureZone, Zone: fwdZone},
		{Kind: model.OpEnsureZone, Zone: revZone},
		{Kind: model.OpCreate, Zone: fwdZone, Record: model.Record{
			Zone: fwdZone, Name: "host1.itest.local.", Type: "A", Data: "10.0.0.5",
		}},
		{Kind: model.OpCreate, Zone: revZone, Record: model.Record{
			Zone: revZone, Name: "5.0.0.10.in-addr.arpa.", Type: "PTR", Data: "host1.itest.local.",
		}},
	}

	if err := tgt.Apply(ctx, ops); err != nil {
		t.Fatalf("Apply (initial): %v", err)
	}
	// Second apply must be a no-op at the server level.
	if err := tgt.Apply(ctx, ops); err != nil {
		t.Fatalf("Apply (idempotent replay): %v", err)
	}

	got, _, err := tgt.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	var sawA, sawPTR bool
	for _, r := range got {
		if r.Type == "A" && r.Name == "host1.itest.local." && r.Data == "10.0.0.5" {
			sawA = true
		}
		if r.Type == "PTR" && r.Name == "5.0.0.10.in-addr.arpa." && r.Data == "host1.itest.local." {
			sawPTR = true
		}
		if r.Type == "AAAA" {
			t.Errorf("unexpected AAAA record present: %#v", r)
		}
	}
	if !sawA {
		t.Errorf("expected A host1.itest.local -> 10.0.0.5 in List")
	}
	if !sawPTR {
		t.Errorf("expected PTR 10.0.0.5 -> host1.itest.local in List")
	}

	// Converge to a new canonical PTR target. Technitium's PTR add APPENDS
	// (does not replace), so the diff that the real reconciler computes
	// includes both the Create for the new target and the Delete of the old.
	// This mirrors what reconcile.Diff produces - exercising that the
	// Create-then-Delete order in Apply leaves exactly one PTR.
	convergeOps := []model.Op{
		{Kind: model.OpCreate, Zone: revZone, Record: model.Record{
			Zone: revZone, Name: "5.0.0.10.in-addr.arpa.", Type: "PTR", Data: "host2.itest.local.",
		}},
		{Kind: model.OpDelete, Zone: revZone, Record: model.Record{
			Zone: revZone, Name: "5.0.0.10.in-addr.arpa.", Type: "PTR", Data: "host1.itest.local.",
		}},
	}
	if err := tgt.Apply(ctx, convergeOps); err != nil {
		t.Fatalf("Apply (PTR converge): %v", err)
	}
	got, _, err = tgt.List(ctx)
	if err != nil {
		t.Fatalf("List after converge: %v", err)
	}
	ptrCount := 0
	var ptrTarget string
	for _, r := range got {
		if r.Type == "PTR" && r.Name == "5.0.0.10.in-addr.arpa." {
			ptrCount++
			ptrTarget = r.Data
		}
	}
	if ptrCount != 1 {
		t.Errorf("expected exactly one PTR for 10.0.0.5, got %d", ptrCount)
	}
	if ptrTarget != "host2.itest.local." {
		t.Errorf("expected PTR target host2.itest.local., got %q", ptrTarget)
	}
}

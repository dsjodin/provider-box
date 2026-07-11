package certs

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"math/big"
	"net"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// TestDecodeIssued covers the blob-decode path that turns a step-ca
// x509_certs nvalue (raw DER) plus its x509_certs_data nvalue (JSON) into a
// Cert, independent of the database driver.
func TestDecodeIssued(t *testing.T) {
	der := makeCert(t, 4096, "active.sddc.lab", []string{"active.sddc.lab", "alias.sddc.lab"}, net.ParseIP("10.0.0.10"))
	dataJSON, _ := json.Marshal(map[string]any{
		"provisioner": map[string]string{"name": "admin"},
	})

	c, err := decodeIssued(der, dataJSON)
	if err != nil {
		t.Fatalf("decodeIssued: %v", err)
	}
	if c.CommonName != "active.sddc.lab" {
		t.Errorf("CommonName = %q, want active.sddc.lab", c.CommonName)
	}
	if c.Provisioner != "admin" {
		t.Errorf("Provisioner = %q, want admin", c.Provisioner)
	}
	if c.Serial != serialHex(big.NewInt(4096)) {
		t.Errorf("Serial = %q, want %q", c.Serial, serialHex(big.NewInt(4096)))
	}
	if c.Revoked {
		t.Error("Revoked should be false before revocation join")
	}
	wantSANs := map[string]bool{"active.sddc.lab": false, "alias.sddc.lab": false, "10.0.0.10": false}
	for _, s := range c.SANs {
		if _, ok := wantSANs[s]; ok {
			wantSANs[s] = true
		}
	}
	for s, seen := range wantSANs {
		if !seen {
			t.Errorf("SAN %q missing from %v", s, c.SANs)
		}
	}
}

// TestDecodeIssuedNoMetadata confirms a LEFT JOIN miss (nil metadata) yields an
// empty provisioner, not an error.
func TestDecodeIssuedNoMetadata(t *testing.T) {
	der := makeCert(t, 5, "nometa.sddc.lab", nil, nil)
	c, err := decodeIssued(der, nil)
	if err != nil {
		t.Fatalf("decodeIssued: %v", err)
	}
	if c.Provisioner != "" {
		t.Errorf("Provisioner = %q, want empty", c.Provisioner)
	}
}

func TestDecodeIssuedBadDER(t *testing.T) {
	if _, err := decodeIssued([]byte("not-a-cert"), nil); err == nil {
		t.Fatal("expected error for malformed DER")
	}
}

// TestDecodeRevokedSerial confirms the decimal serial in a revoked record is
// normalized to the same lowercase hex form decodeIssued produces, so the two
// join.
func TestDecodeRevokedSerial(t *testing.T) {
	revJSON, _ := json.Marshal(map[string]string{"Serial": "4097"})
	got, ok := decodeRevokedSerial(revJSON)
	if !ok {
		t.Fatal("decodeRevokedSerial returned ok=false")
	}
	if want := serialHex(big.NewInt(4097)); got != want {
		t.Errorf("serial = %q, want %q", got, want)
	}

	if _, ok := decodeRevokedSerial([]byte("{bad")); ok {
		t.Error("malformed JSON should return ok=false")
	}
	if _, ok := decodeRevokedSerial([]byte(`{"Serial":"xyz"}`)); ok {
		t.Error("non-decimal serial should return ok=false")
	}
}

// TestReaderList_Integration exercises the real SQL path against a live
// postgres shaped exactly like step-ca's backend (per-bucket nkey/nvalue BYTEA
// tables). It is skipped unless STEPCA_TEST_PG_DSN points at a throwaway
// database, because the sandbox has no postgres. Run on-host with e.g.
//
//	STEPCA_TEST_PG_DSN='postgres://postgres:pw@127.0.0.1:5432/postgres' go test ./internal/certs
func TestReaderList_Integration(t *testing.T) {
	dsn := os.Getenv("STEPCA_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set STEPCA_TEST_PG_DSN to run the live postgres reader test")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	seedFixtureTables(t, ctx, conn)

	r, err := NewReader(dsn, "")
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	got, err := r.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d certs, want 2: %+v", len(got), got)
	}
	byCN := map[string]Cert{}
	for _, c := range got {
		byCN[c.CommonName] = c
	}
	if a := byCN["active.sddc.lab"]; a.Revoked || a.Provisioner != "admin" {
		t.Errorf("active cert wrong: %+v", a)
	}
	if rc := byCN["revoked.sddc.lab"]; !rc.Revoked {
		t.Errorf("revoked cert not flagged: %+v", rc)
	}
}

func seedFixtureTables(t *testing.T, ctx context.Context, conn *pgx.Conn) {
	t.Helper()
	for _, tbl := range []string{tableIssued, tableIssuedData, tableRevoked} {
		if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+tbl+
			` (nkey BYTEA CHECK (octet_length(nkey) <= 255), nvalue BYTEA, PRIMARY KEY (nkey))`); err != nil {
			t.Fatalf("create %s: %v", tbl, err)
		}
		if _, err := conn.Exec(ctx, `TRUNCATE `+tbl); err != nil {
			t.Fatalf("truncate %s: %v", tbl, err)
		}
	}
	activeDER := makeCert(t, 4096, "active.sddc.lab", []string{"active.sddc.lab"}, nil)
	revokedDER := makeCert(t, 4097, "revoked.sddc.lab", []string{"revoked.sddc.lab"}, nil)
	dataJSON, _ := json.Marshal(map[string]any{"provisioner": map[string]string{"name": "admin"}})
	revJSON, _ := json.Marshal(map[string]string{"Serial": "4097"})

	exec := func(tbl, key string, val []byte) {
		if _, err := conn.Exec(ctx, `INSERT INTO `+tbl+` (nkey, nvalue) VALUES ($1, $2)`, []byte(key), val); err != nil {
			t.Fatalf("insert %s/%s: %v", tbl, key, err)
		}
	}
	exec(tableIssued, "4096", activeDER)
	exec(tableIssuedData, "4096", dataJSON)
	exec(tableIssued, "4097", revokedDER)
	exec(tableRevoked, "4097", revJSON)
}

func makeCert(t *testing.T, serial int64, cn string, dns []string, ip net.IP) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
		DNSNames:     dns,
	}
	if ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

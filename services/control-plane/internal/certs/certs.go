// Package certs reads step-ca's issued/revoked certificate state from its
// PostgreSQL backend. step-ca uses postgres as an opaque key-value store
// (smallstep/nosql): one table per bucket, each with two columns
// (nkey BYTEA, nvalue BYTEA). The value is never relational - it is the raw
// certificate or a JSON blob - so this reader SELECTs the blobs and decodes
// them here, exactly as the retired BadgerDB reader did. It only ever issues
// SELECTs and is expected to connect with a read-only role.
//
// Verified against step-ca v0.30.2 + smallstep/nosql v0.8.0:
//   - table "x509_certs"         nkey = decimal serial, nvalue = raw DER cert
//   - table "x509_certs_data"    nkey = decimal serial, nvalue = JSON CertificateData
//   - table "revoked_x509_certs" nkey = decimal serial, nvalue = JSON RevokedCertificateInfo
//
// Unlike the badger backend, the table IS the bucket, so nkey is the plain key
// (the decimal serial string), not a length-prefixed binary encoding.
package certs

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Cert is one issued certificate as read from step-ca. Revoked is set when the
// serial appears in the revoked table.
type Cert struct {
	Serial      string
	CommonName  string
	SANs        []string
	Provisioner string
	NotBefore   time.Time
	NotAfter    time.Time
	Revoked     bool
}

// Reader reads certificates from step-ca's PostgreSQL backend. It holds a
// parsed connection config and opens a short-lived connection per List call;
// the dashboard reads on page load, so a pool is unnecessary.
type Reader struct {
	cfg *pgx.ConnConfig
}

const (
	tableIssued     = "x509_certs"
	tableIssuedData = "x509_certs_data"
	tableRevoked    = "revoked_x509_certs"
)

// NewReader parses dsn (a libpq/pgx connection string, without a password) and
// applies password if non-empty. Keeping the password separate lets it come
// from a file rather than being baked into the DSN.
func NewReader(dsn, password string) (*Reader, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse stepca postgres dsn: %w", err)
	}
	if password != "" {
		cfg.Password = password
	}
	return &Reader{cfg: cfg}, nil
}

// List returns every issued certificate, with Revoked set from the revoked
// table. The caller derives active/expired state from NotAfter and Revoked.
func (r *Reader) List(ctx context.Context) ([]Cert, error) {
	conn, err := pgx.ConnectConfig(ctx, r.cfg)
	if err != nil {
		return nil, fmt.Errorf("connect stepca postgres: %w", err)
	}
	defer conn.Close(ctx)

	revoked, err := readRevoked(ctx, conn)
	if err != nil {
		return nil, err
	}
	issued, err := readIssued(ctx, conn)
	if err != nil {
		return nil, err
	}
	for i := range issued {
		if _, ok := revoked[issued[i].Serial]; ok {
			issued[i].Revoked = true
		}
	}
	return issued, nil
}

// readIssued joins each cert (raw DER) with its metadata blob (provisioner) by
// serial and decodes both in one pass.
func readIssued(ctx context.Context, conn *pgx.Conn) ([]Cert, error) {
	rows, err := conn.Query(ctx, `
		SELECT c.nvalue, d.nvalue
		FROM `+tableIssued+` AS c
		LEFT JOIN `+tableIssuedData+` AS d ON d.nkey = c.nkey`)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", tableIssued, err)
	}
	defer rows.Close()

	var out []Cert
	for rows.Next() {
		var der, dataJSON []byte
		if err := rows.Scan(&der, &dataJSON); err != nil {
			return nil, fmt.Errorf("scan %s row: %w", tableIssued, err)
		}
		c, err := decodeIssued(der, dataJSON)
		if err != nil {
			// A bad row must not abort the whole read; skip it.
			continue
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s: %w", tableIssued, err)
	}
	return out, nil
}

func readRevoked(ctx context.Context, conn *pgx.Conn) (map[string]struct{}, error) {
	rows, err := conn.Query(ctx, `SELECT nvalue FROM `+tableRevoked)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", tableRevoked, err)
	}
	defer rows.Close()

	out := map[string]struct{}{}
	for rows.Next() {
		var revJSON []byte
		if err := rows.Scan(&revJSON); err != nil {
			return nil, fmt.Errorf("scan %s row: %w", tableRevoked, err)
		}
		if serial, ok := decodeRevokedSerial(revJSON); ok {
			out[serial] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate %s: %w", tableRevoked, err)
	}
	return out, nil
}

// decodeIssued parses a raw DER cert and its optional JSON metadata blob into a
// Cert. der is required; dataJSON may be nil (LEFT JOIN miss).
func decodeIssued(der, dataJSON []byte) (Cert, error) {
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return Cert{}, err
	}
	return Cert{
		Serial:      serialHex(cert.SerialNumber),
		CommonName:  cert.Subject.CommonName,
		SANs:        collectSANs(cert),
		Provisioner: decodeProvisioner(dataJSON),
		NotBefore:   cert.NotBefore,
		NotAfter:    cert.NotAfter,
	}, nil
}

// certData mirrors db.CertificateData in step-ca. Only the provisioner name is
// consumed here.
type certData struct {
	Provisioner *struct {
		Name string `json:"name"`
	} `json:"provisioner,omitempty"`
}

func decodeProvisioner(dataJSON []byte) string {
	if len(dataJSON) == 0 {
		return ""
	}
	var cd certData
	if err := json.Unmarshal(dataJSON, &cd); err != nil || cd.Provisioner == nil {
		return ""
	}
	return cd.Provisioner.Name
}

// revokedRecord mirrors authority/db.RevokedCertificateInfo. The upstream
// struct has no JSON tags so fields serialize under their Go names.
type revokedRecord struct {
	Serial string `json:"Serial"`
}

// decodeRevokedSerial returns the revoked serial normalized to lowercase hex,
// matching decodeIssued's Serial form so the two can be joined.
func decodeRevokedSerial(revJSON []byte) (string, bool) {
	var rec revokedRecord
	if err := json.Unmarshal(revJSON, &rec); err != nil {
		return "", false
	}
	n, ok := new(big.Int).SetString(rec.Serial, 10)
	if !ok {
		return "", false
	}
	return serialHex(n), true
}

func serialHex(n *big.Int) string {
	return strings.ToLower(fmt.Sprintf("%x", n))
}

func collectSANs(c *x509.Certificate) []string {
	out := []string{}
	out = append(out, c.DNSNames...)
	for _, ip := range c.IPAddresses {
		out = append(out, ip.String())
	}
	out = append(out, c.EmailAddresses...)
	for _, u := range c.URIs {
		out = append(out, u.String())
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

package reconcile

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"time"

	badger "github.com/dgraph-io/badger/v4"

	"github.com/dsjodin/provider-box/services/stepca-api/internal/store"
)

// BadgerSource reads step-ca's BadgerDB directly. Couples to step-ca's storage
// layout; isolated here so a step-ca version bump only touches this file.
//
// Layout assumptions (verify per step-ca version):
//   - bucket "x509_certs"         keyed by serial, value = PEM cert chain
//   - bucket "revoked_x509_certs" keyed by serial, value = JSON revocation info
//
// step-ca's nosql/badger driver encodes keys as "<bucket>/<key>" (URL-encoded).
// Keep the prefix layout here; if it changes upstream, replace prefix() and the
// stripPrefix logic.
type BadgerSource struct {
	Path string
}

const (
	bucketIssued  = "x509_certs"
	bucketRevoked = "revoked_x509_certs"
)

func (b *BadgerSource) open() (*badger.DB, error) {
	opts := badger.DefaultOptions(b.Path).
		WithReadOnly(true).
		WithLogger(nil)
	return badger.Open(opts)
}

func (b *BadgerSource) Issued(ctx context.Context, yield func(store.Cert) error) error {
	db, err := b.open()
	if err != nil {
		return fmt.Errorf("open badger: %w", err)
	}
	defer db.Close()

	return db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		prefix := prefix(bucketIssued)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			item := it.Item()
			serial := stripPrefix(item.Key(), prefix)
			if serial == "" {
				continue
			}
			var pemBytes []byte
			if err := item.Value(func(v []byte) error {
				pemBytes = append(pemBytes[:0], v...)
				return nil
			}); err != nil {
				return err
			}
			cert, err := decodeCert(pemBytes)
			if err != nil {
				// One bad row should not abort the whole pass.
				continue
			}
			if err := yield(store.Cert{
				Serial:      serial,
				CommonName:  cert.Subject.CommonName,
				SANs:        collectSANs(cert),
				NotBefore:   cert.NotBefore,
				NotAfter:    cert.NotAfter,
				Provisioner: "",
				Status:      store.StatusActive,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// revokedRecord matches the fields step-ca stores in revoked_x509_certs. The
// upstream struct lives in step-ca's authority package; mirror only what we
// consume here so the dependency stays narrow.
type revokedRecord struct {
	Serial        string    `json:"serial"`
	ProvisionerID string    `json:"provisionerID"`
	ReasonCode    int       `json:"reasonCode"`
	Reason        string    `json:"reason"`
	RevokedAt     time.Time `json:"revokedAt"`
}

func (b *BadgerSource) Revoked(ctx context.Context, yield func(serial, reason string, at time.Time) error) error {
	db, err := b.open()
	if err != nil {
		return fmt.Errorf("open badger: %w", err)
	}
	defer db.Close()

	return db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		prefix := prefix(bucketRevoked)
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			item := it.Item()
			serial := stripPrefix(item.Key(), prefix)
			if serial == "" {
				continue
			}
			var raw []byte
			if err := item.Value(func(v []byte) error {
				raw = append(raw[:0], v...)
				return nil
			}); err != nil {
				return err
			}
			var rec revokedRecord
			if err := json.Unmarshal(raw, &rec); err != nil {
				continue
			}
			if rec.Serial == "" {
				rec.Serial = serial
			}
			if rec.RevokedAt.IsZero() {
				rec.RevokedAt = time.Now().UTC()
			}
			if err := yield(rec.Serial, rec.Reason, rec.RevokedAt); err != nil {
				return err
			}
		}
		return nil
	})
}

func prefix(bucket string) []byte {
	return []byte(bucket + "/")
}

func stripPrefix(key, prefix []byte) string {
	if len(key) <= len(prefix) {
		return ""
	}
	return string(key[len(prefix):])
}

func decodeCert(pemBytes []byte) (*x509.Certificate, error) {
	for {
		block, rest := pem.Decode(pemBytes)
		if block == nil {
			return nil, fmt.Errorf("no PEM block in cert payload")
		}
		if block.Type == "CERTIFICATE" {
			return x509.ParseCertificate(block.Bytes)
		}
		pemBytes = rest
	}
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

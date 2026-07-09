// Package certs reads step-ca's issued/revoked certificate state directly from
// its BadgerDB. This is the reusable cert-reading core migrated from the
// design-stage services/stepca-api (its reconcile/badger.go); the dashboard
// folds in that service's intent as a read-only "current certs" panel and
// drops the SQLite inventory + HTTP-API layers that were phase-2 collector
// concerns.
//
// To avoid lock contention with a running step-ca, the live data directory is
// never opened directly: each read copies the live dir to a unique temp
// directory under SnapshotRoot, opens the copy read-only, reads, then discards
// it. See STEPCA_STORAGE.md.
//
// Verified against step-ca v0.30.2 + smallstep/nosql v0.8.0:
//   - bucket "x509_certs"         keyed by decimal serial, value = raw DER cert
//   - bucket "x509_certs_data"    keyed by decimal serial, value = JSON CertificateData
//   - bucket "revoked_x509_certs" keyed by decimal serial, value = JSON RevokedCertificateInfo
//
// smallstep/nosql encodes badger keys as
//
//	[2-byte LE bucket len][bucket][2-byte LE key len][key]
//
// so iterating one bucket means prefix-seeking on its first two segments.
package certs

import (
	"context"
	"crypto/x509"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	badger "github.com/dgraph-io/badger/v4"
)

// Cert is one issued certificate as read from step-ca. Revoked is set when the
// serial appears in the revoked bucket.
type Cert struct {
	Serial      string
	CommonName  string
	SANs        []string
	Provisioner string
	NotBefore   time.Time
	NotAfter    time.Time
	Revoked     bool
}

// Reader reads certificates from a step-ca BadgerDB. Path is the live step-ca
// data directory; it is never opened directly. SnapshotRoot is the parent
// directory for per-read snapshots (empty means os.TempDir()).
type Reader struct {
	Path         string
	SnapshotRoot string
}

const (
	bucketIssued     = "x509_certs"
	bucketIssuedData = "x509_certs_data"
	bucketRevoked    = "revoked_x509_certs"
)

// List returns every issued certificate, with Revoked set from the revoked
// bucket. The caller derives active/expired state from NotAfter and Revoked.
func (r *Reader) List(ctx context.Context) ([]Cert, error) {
	var out []Cert
	err := r.withSnapshot(ctx, func(db *badger.DB) error {
		revoked, err := readRevoked(ctx, db)
		if err != nil {
			return err
		}
		issued, err := readIssued(ctx, db)
		if err != nil {
			return err
		}
		for i := range issued {
			if _, ok := revoked[issued[i].Serial]; ok {
				issued[i].Revoked = true
			}
		}
		out = issued
		return nil
	})
	return out, err
}

func readIssued(ctx context.Context, db *badger.DB) ([]Cert, error) {
	var out []Cert
	prefix := bucketPrefix(bucketIssued)
	err := db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			var raw []byte
			if err := it.Item().Value(func(v []byte) error {
				raw = append(raw[:0], v...)
				return nil
			}); err != nil {
				return err
			}
			cert, err := x509.ParseCertificate(raw)
			if err != nil {
				// A bad row must not abort the whole read; skip it.
				continue
			}
			out = append(out, Cert{
				Serial:      serialHex(cert.SerialNumber),
				CommonName:  cert.Subject.CommonName,
				SANs:        collectSANs(cert),
				Provisioner: lookupProvisioner(txn, cert.SerialNumber.String()),
				NotBefore:   cert.NotBefore,
				NotAfter:    cert.NotAfter,
			})
		}
		return nil
	})
	return out, err
}

// revokedRecord mirrors authority/db.RevokedCertificateInfo. The upstream
// struct has no JSON tags so fields serialize under their Go names.
type revokedRecord struct {
	Serial string `json:"Serial"`
}

func readRevoked(ctx context.Context, db *badger.DB) (map[string]struct{}, error) {
	out := map[string]struct{}{}
	prefix := bucketPrefix(bucketRevoked)
	err := db.View(func(txn *badger.Txn) error {
		it := txn.NewIterator(badger.DefaultIteratorOptions)
		defer it.Close()
		for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
			if err := ctx.Err(); err != nil {
				return err
			}
			var raw []byte
			if err := it.Item().Value(func(v []byte) error {
				raw = append(raw[:0], v...)
				return nil
			}); err != nil {
				return err
			}
			var rec revokedRecord
			if err := json.Unmarshal(raw, &rec); err != nil {
				continue
			}
			n, ok := new(big.Int).SetString(rec.Serial, 10)
			if !ok {
				continue
			}
			out[serialHex(n)] = struct{}{}
		}
		return nil
	})
	return out, err
}

// certData mirrors db.CertificateData in step-ca. Only the provisioner name is
// consumed here.
type certData struct {
	Provisioner *struct {
		Name string `json:"name"`
	} `json:"provisioner,omitempty"`
}

func lookupProvisioner(txn *badger.Txn, decimalSerial string) string {
	item, err := txn.Get(toBadgerKey(bucketIssuedData, decimalSerial))
	if err != nil {
		return ""
	}
	var raw []byte
	if err := item.Value(func(v []byte) error {
		raw = append(raw[:0], v...)
		return nil
	}); err != nil {
		return ""
	}
	var cd certData
	if err := json.Unmarshal(raw, &cd); err != nil || cd.Provisioner == nil {
		return ""
	}
	return cd.Provisioner.Name
}

// withSnapshot copies Path to a fresh temp dir, opens the copy read-only, runs
// fn, then removes the copy.
func (r *Reader) withSnapshot(ctx context.Context, fn func(*badger.DB) error) (retErr error) {
	if r.Path == "" {
		return errors.New("certs.Reader.Path is required")
	}
	root := r.SnapshotRoot
	if root == "" {
		root = os.TempDir()
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("ensure snapshot root %s: %w", root, err)
	}
	dir, err := os.MkdirTemp(root, "dashboard-stepca-*")
	if err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil && retErr == nil {
			retErr = fmt.Errorf("remove snapshot dir %s: %w", dir, rmErr)
		}
	}()

	if err := copyTree(ctx, r.Path, dir); err != nil {
		return fmt.Errorf("snapshot copy from %s: %w", r.Path, err)
	}

	opts := badger.DefaultOptions(dir).WithReadOnly(true).WithLogger(nil)
	db, err := badger.Open(opts)
	if err != nil {
		// An inconsistent copy (step-ca wrote mid-copy) shows up here; the
		// caller surfaces it as a panel error and the next page load retries.
		return fmt.Errorf("open snapshot badger at %s: %w", dir, err)
	}
	defer db.Close()
	return fn(db)
}

func copyTree(ctx context.Context, src, dst string) error {
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if cerr := ctx.Err(); cerr != nil {
			return cerr
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		return copyFile(p, target, info.Mode().Perm())
	})
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

// bucketPrefix returns the [LE-len][bucket] segment used to prefix-scan one
// table in the smallstep/nosql Badger encoding.
func bucketPrefix(bucket string) []byte {
	out := make([]byte, 2+len(bucket))
	binary.LittleEndian.PutUint16(out[:2], uint16(len(bucket)))
	copy(out[2:], bucket)
	return out
}

// toBadgerKey mirrors smallstep/nosql/badger/v2.toBadgerKey for point lookups.
func toBadgerKey(bucket, key string) []byte {
	out := make([]byte, 0, 4+len(bucket)+len(key))
	var lb, lk [2]byte
	binary.LittleEndian.PutUint16(lb[:], uint16(len(bucket)))
	binary.LittleEndian.PutUint16(lk[:], uint16(len(key)))
	out = append(out, lb[:]...)
	out = append(out, bucket...)
	out = append(out, lk[:]...)
	out = append(out, key...)
	return out
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

package reconcile

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

	"github.com/dsjodin/provider-box/services/stepca-api/internal/store"
)

// BadgerSource reads step-ca's BadgerDB. To avoid lock contention with a
// running step-ca, the live directory is never opened directly: each read
// copies the live dir to a unique temp directory under SnapshotRoot, opens
// the copy read-only, reads, then discards the copy. See STEPCA_STORAGE.md.
//
// Verified against step-ca v0.30.2 + smallstep/nosql v0.8.0:
//   - bucket "x509_certs"         keyed by decimal serial, value = raw DER cert
//   - bucket "x509_certs_data"    keyed by decimal serial, value = JSON CertificateData
//   - bucket "revoked_x509_certs" keyed by decimal serial, value = JSON RevokedCertificateInfo
//
// smallstep/nosql encodes badger keys as
//   [2-byte LE bucket len][bucket][2-byte LE key len][key]
// so iterating one bucket means prefix-seeking on its first two segments.
type BadgerSource struct {
	// Path is the live step-ca data directory. Read-only access only; the
	// reconciler never opens this directory directly.
	Path string

	// SnapshotRoot is the parent directory under which per-read snapshots are
	// created. Empty means os.TempDir(). On a filesystem that supports cheap
	// snapshots (ZFS, btrfs, LVM thin) this could be replaced by a snapshot
	// mount; the on-disk shape Badger sees is unchanged.
	SnapshotRoot string
}

const (
	bucketIssued     = "x509_certs"
	bucketIssuedData = "x509_certs_data"
	bucketRevoked    = "revoked_x509_certs"
)

// withSnapshot copies Path to a fresh temp dir, opens the copy read-only,
// runs fn, then removes the copy. Errors from the copy or open are returned
// to the caller; the reconcile loop logs and retries on the next interval.
func (b *BadgerSource) withSnapshot(ctx context.Context, fn func(*badger.DB) error) (retErr error) {
	if b.Path == "" {
		return errors.New("BadgerSource.Path is required")
	}
	root := b.SnapshotRoot
	if root == "" {
		root = os.TempDir()
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return fmt.Errorf("ensure snapshot root %s: %w", root, err)
	}

	dir, err := os.MkdirTemp(root, "stepca-snapshot-*")
	if err != nil {
		return fmt.Errorf("create snapshot dir: %w", err)
	}
	defer func() {
		if rmErr := os.RemoveAll(dir); rmErr != nil && retErr == nil {
			retErr = fmt.Errorf("remove snapshot dir %s: %w", dir, rmErr)
		}
	}()

	if err := copyTree(ctx, b.Path, dir); err != nil {
		return fmt.Errorf("snapshot copy from %s: %w", b.Path, err)
	}

	opts := badger.DefaultOptions(dir).WithReadOnly(true).WithLogger(nil)
	db, err := badger.Open(opts)
	if err != nil {
		// Inconsistent copy (e.g. step-ca wrote mid-copy and the MANIFEST
		// disagrees with vlog) shows up as an Open error here. Returning lets
		// the next reconcile pass try again on a fresh copy.
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

// toBadgerKey mirrors smallstep/nosql/badger/v2.toBadgerKey so we can build
// the exact same key shape for point lookups (e.g. x509_certs_data by serial).
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

// certData mirrors db.CertificateData in step-ca. Only the field stepca-api
// consumes is listed.
type certData struct {
	Provisioner *struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Type string `json:"type"`
	} `json:"provisioner,omitempty"`
}

func (b *BadgerSource) Issued(ctx context.Context, yield func(store.Cert) error) error {
	return b.withSnapshot(ctx, func(db *badger.DB) error {
		prefix := bucketPrefix(bucketIssued)
		return db.View(func(txn *badger.Txn) error {
			it := txn.NewIterator(badger.DefaultIteratorOptions)
			defer it.Close()
			for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
				if err := ctx.Err(); err != nil {
					return err
				}
				item := it.Item()
				var raw []byte
				if err := item.Value(func(v []byte) error {
					raw = append(raw[:0], v...)
					return nil
				}); err != nil {
					return err
				}
				cert, err := x509.ParseCertificate(raw)
				if err != nil {
					// Bad row shouldn't abort the whole pass; reconcile picks
					// it up on the next tick if it heals.
					continue
				}
				if err := yield(store.Cert{
					Serial:      serialHex(cert.SerialNumber),
					CommonName:  cert.Subject.CommonName,
					SANs:        collectSANs(cert),
					NotBefore:   cert.NotBefore,
					NotAfter:    cert.NotAfter,
					Provisioner: lookupProvisioner(txn, cert.SerialNumber.String()),
					Status:      store.StatusActive,
				}); err != nil {
					return err
				}
			}
			return nil
		})
	})
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
	if err := json.Unmarshal(raw, &cd); err != nil {
		return ""
	}
	if cd.Provisioner == nil {
		return ""
	}
	return cd.Provisioner.Name
}

// revokedRecord mirrors authority/db.RevokedCertificateInfo. The upstream
// struct has no JSON tags so fields serialize under their Go names; mirror
// only the fields stepca-api consumes.
type revokedRecord struct {
	Serial    string    `json:"Serial"`
	Reason    string    `json:"Reason"`
	RevokedAt time.Time `json:"RevokedAt"`
}

func (b *BadgerSource) Revoked(ctx context.Context, yield func(serial, reason string, at time.Time) error) error {
	return b.withSnapshot(ctx, func(db *badger.DB) error {
		prefix := bucketPrefix(bucketRevoked)
		return db.View(func(txn *badger.Txn) error {
			it := txn.NewIterator(badger.DefaultIteratorOptions)
			defer it.Close()
			for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
				if err := ctx.Err(); err != nil {
					return err
				}
				item := it.Item()
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
				n, ok := new(big.Int).SetString(rec.Serial, 10)
				if !ok {
					continue
				}
				at := rec.RevokedAt
				if at.IsZero() {
					at = time.Now().UTC()
				}
				if err := yield(serialHex(n), rec.Reason, at); err != nil {
					return err
				}
			}
			return nil
		})
	})
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

package store

import (
	"context"
	_ "embed"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schema string

const (
	StatusActive  = "active"
	StatusRevoked = "revoked"
	StatusExpired = "expired"
)

const timeFmt = time.RFC3339

type Cert struct {
	Serial       string
	CommonName   string
	SANs         []string
	Provisioner  string
	NotBefore    time.Time
	NotAfter     time.Time
	Status       string
	RevokedAt    *time.Time
	RevokeReason string
	Source       string
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Upsert inserts or updates a certificate row keyed by serial. Revocation state
// from the input is preserved only when it is set; an active record never
// overwrites an existing revoked record.
func (s *Store) Upsert(ctx context.Context, c Cert) error {
	if c.Serial == "" {
		return errors.New("serial is required")
	}
	sans, err := json.Marshal(c.SANs)
	if err != nil {
		return err
	}
	status := c.Status
	if status == "" {
		status = StatusActive
	}
	var revokedAt any
	if c.RevokedAt != nil {
		revokedAt = c.RevokedAt.UTC().Format(timeFmt)
	}

	_, err = s.db.ExecContext(ctx, `
		INSERT INTO certificates
		    (serial, common_name, sans, provisioner, not_before, not_after,
		     status, revoked_at, revoke_reason, source, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, datetime('now'))
		ON CONFLICT(serial) DO UPDATE SET
		    common_name   = excluded.common_name,
		    sans          = excluded.sans,
		    provisioner   = excluded.provisioner,
		    not_before    = excluded.not_before,
		    not_after     = excluded.not_after,
		    status        = CASE
		        WHEN certificates.status = 'revoked' THEN 'revoked'
		        ELSE excluded.status
		    END,
		    revoked_at    = COALESCE(certificates.revoked_at, excluded.revoked_at),
		    revoke_reason = COALESCE(NULLIF(certificates.revoke_reason, ''), excluded.revoke_reason),
		    source        = excluded.source,
		    updated_at    = datetime('now')
	`,
		c.Serial, c.CommonName, string(sans), c.Provisioner,
		c.NotBefore.UTC().Format(timeFmt), c.NotAfter.UTC().Format(timeFmt),
		status, revokedAt, c.RevokeReason, c.Source,
	)
	return err
}

func (s *Store) MarkRevoked(ctx context.Context, serial, reason string, at time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE certificates
		   SET status = 'revoked',
		       revoked_at = ?,
		       revoke_reason = ?,
		       updated_at = datetime('now')
		 WHERE serial = ?
	`, at.UTC().Format(timeFmt), reason, serial)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

var ErrNotFound = errors.New("certificate not found")

func (s *Store) Get(ctx context.Context, serial string) (*Cert, error) {
	row := s.db.QueryRowContext(ctx, selectStmt+` WHERE serial = ?`, serial)
	c, err := scanCert(row)
	if err == sql.ErrNoRows {
		return nil, ErrNotFound
	}
	return c, err
}

type ListFilter struct {
	Status         string
	CommonName     string
	ExpiringBefore *time.Time
	Limit          int
	Offset         int
}

func (s *Store) List(ctx context.Context, f ListFilter) ([]Cert, error) {
	q := selectStmt + ` WHERE 1=1`
	args := []any{}
	if f.Status != "" {
		// match against the derived status (active / revoked / expired)
		q += ` AND derived_status = ?`
		args = append(args, f.Status)
	}
	if f.CommonName != "" {
		q += ` AND common_name LIKE ?`
		args = append(args, "%"+f.CommonName+"%")
	}
	if f.ExpiringBefore != nil {
		q += ` AND not_after < ?`
		args = append(args, f.ExpiringBefore.UTC().Format(timeFmt))
	}
	q += ` ORDER BY not_after DESC`
	if f.Limit > 0 {
		q += ` LIMIT ?`
		args = append(args, f.Limit)
		if f.Offset > 0 {
			q += ` OFFSET ?`
			args = append(args, f.Offset)
		}
	}

	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Cert{}
	for rows.Next() {
		c, err := scanCert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *c)
	}
	return out, rows.Err()
}

// AllSerials returns every serial currently in the store. The reconcile job
// uses this to detect rows that the authoritative source no longer reports as
// active so it can flip their derived state if needed.
func (s *Store) AllSerials(ctx context.Context) (map[string]struct{}, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT serial FROM certificates`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]struct{}{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out[s] = struct{}{}
	}
	return out, rows.Err()
}

// status is derived at read time per design: 'expired' is computed from
// not_after < now so query results never depend on a background sweeper.
const selectStmt = `
SELECT serial, common_name, sans, provisioner, not_before, not_after,
       CASE
           WHEN status = 'revoked'                       THEN 'revoked'
           WHEN datetime(not_after) < datetime('now')    THEN 'expired'
           ELSE 'active'
       END AS derived_status,
       revoked_at, revoke_reason, source
  FROM certificates`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanCert(r rowScanner) (*Cert, error) {
	var (
		c            Cert
		sans         sql.NullString
		provisioner  sql.NullString
		notBefore    string
		notAfter     string
		revokedAt    sql.NullString
		revokeReason sql.NullString
		source       sql.NullString
	)
	if err := r.Scan(
		&c.Serial, &c.CommonName, &sans, &provisioner,
		&notBefore, &notAfter, &c.Status,
		&revokedAt, &revokeReason, &source,
	); err != nil {
		return nil, err
	}
	if sans.Valid && sans.String != "" {
		_ = json.Unmarshal([]byte(sans.String), &c.SANs)
	}
	c.Provisioner = provisioner.String
	c.RevokeReason = revokeReason.String
	c.Source = source.String
	var err error
	if c.NotBefore, err = time.Parse(timeFmt, notBefore); err != nil {
		return nil, fmt.Errorf("parse not_before: %w", err)
	}
	if c.NotAfter, err = time.Parse(timeFmt, notAfter); err != nil {
		return nil, fmt.Errorf("parse not_after: %w", err)
	}
	if revokedAt.Valid {
		t, err := time.Parse(timeFmt, revokedAt.String)
		if err != nil {
			return nil, fmt.Errorf("parse revoked_at: %w", err)
		}
		c.RevokedAt = &t
	}
	return &c, nil
}

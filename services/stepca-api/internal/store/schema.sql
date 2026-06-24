CREATE TABLE IF NOT EXISTS certificates (
    serial        TEXT PRIMARY KEY,
    common_name   TEXT NOT NULL,
    sans          TEXT,
    provisioner   TEXT,
    not_before    TEXT NOT NULL,
    not_after     TEXT NOT NULL,
    status        TEXT NOT NULL DEFAULT 'active',
    revoked_at    TEXT,
    revoke_reason TEXT,
    source        TEXT,
    created_at    TEXT NOT NULL DEFAULT (datetime('now')),
    updated_at    TEXT NOT NULL DEFAULT (datetime('now'))
);

CREATE INDEX IF NOT EXISTS idx_certs_status    ON certificates(status);
CREATE INDEX IF NOT EXISTS idx_certs_not_after ON certificates(not_after);
CREATE INDEX IF NOT EXISTS idx_certs_cn        ON certificates(common_name);

-- 0005_media_upload_leases: durable, install-scoped staging uploads and ephemeral media leases.
--
-- Bytes never enter SQLite. uploads persist only the lifecycle and integrity identity of a staged
-- object; the file store is keyed by opaque ids beneath the configured gateway media root. A lease
-- is the only completion-visible capability and is intentionally neither sha-derived nor reusable
-- across installs.

CREATE TABLE media_uploads (
  id             TEXT PRIMARY KEY,
  install_id     TEXT NOT NULL,
  expected_sha256 TEXT NOT NULL,
  mime_type      TEXT NOT NULL,
  total_bytes    INTEGER NOT NULL CHECK (total_bytes > 0),
  received_bytes INTEGER NOT NULL DEFAULT 0 CHECK (received_bytes >= 0 AND received_bytes <= total_bytes),
  state          TEXT NOT NULL CHECK (state IN ('open','completed','aborted','expired')),
  expires_at     DATETIME NOT NULL,
  created_at     DATETIME NOT NULL,
  updated_at     DATETIME NOT NULL,
  completed_at   DATETIME
);
CREATE INDEX idx_media_uploads_expiry ON media_uploads(state, expires_at);
CREATE INDEX idx_media_uploads_install ON media_uploads(install_id, created_at);

CREATE TABLE media_leases (
  id              TEXT PRIMARY KEY,
  install_id      TEXT NOT NULL,
  upload_id       TEXT NOT NULL UNIQUE,
  sha256          TEXT NOT NULL,
  mime_type       TEXT NOT NULL,
  size_bytes      INTEGER NOT NULL CHECK (size_bytes > 0),
  fetch_token_hash TEXT NOT NULL UNIQUE,
  state           TEXT NOT NULL CHECK (state IN ('active','expired','deleted')),
  expires_at      DATETIME NOT NULL,
  created_at      DATETIME NOT NULL,
  deleted_at      DATETIME
);
CREATE INDEX idx_media_leases_expiry ON media_leases(state, expires_at);
CREATE INDEX idx_media_leases_install ON media_leases(install_id, created_at);

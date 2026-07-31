CREATE TABLE global_spend_daily (
  period_day TEXT PRIMARY KEY,
  spend_pusd INTEGER NOT NULL DEFAULT 0 CHECK (spend_pusd >= 0),
  requests   INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0)
);

CREATE TABLE global_spend_monthly (
  period_month TEXT PRIMARY KEY,
  spend_pusd   INTEGER NOT NULL DEFAULT 0 CHECK (spend_pusd >= 0),
  requests     INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0)
);

CREATE TABLE install_category_daily (
  install_id TEXT    NOT NULL,
  category   TEXT    NOT NULL,
  period_day TEXT    NOT NULL,
  units      INTEGER NOT NULL DEFAULT 0 CHECK (units >= 0),
  PRIMARY KEY (install_id, category, period_day)
);

CREATE TABLE install_fp_rate (
  fp_sha256  TEXT NOT NULL,
  window_day TEXT NOT NULL,
  count      INTEGER NOT NULL DEFAULT 0,
  last_at    DATETIME,
  PRIMARY KEY (fp_sha256, window_day)
);

CREATE TABLE install_global_rate (
  window_day TEXT PRIMARY KEY,
  count      INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE install_ip_rate (
  ip_key      TEXT NOT NULL,
  window_hour TEXT NOT NULL,
  count       INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (ip_key, window_hour)
);

CREATE TABLE install_spend_daily (
  install_id TEXT NOT NULL,
  period_day TEXT NOT NULL,
  spend_pusd INTEGER NOT NULL DEFAULT 0 CHECK (spend_pusd >= 0),
  requests   INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0),
  PRIMARY KEY (install_id, period_day)
);

CREATE TABLE install_voices (
  id          TEXT    PRIMARY KEY,
  install_id  TEXT    NOT NULL,
  name        TEXT    NOT NULL,
  upstream_id TEXT    NOT NULL,
  created_at  INTEGER NOT NULL
);

CREATE TABLE installs (
  id             TEXT PRIMARY KEY,
  public_key     BLOB NOT NULL,
  key_thumbprint TEXT NOT NULL UNIQUE,
  fingerprint    TEXT,
  client         TEXT,
  status         TEXT NOT NULL DEFAULT 'active',
  created_at     DATETIME NOT NULL,
  last_seen_at   DATETIME
);

CREATE TABLE media_leases (
  id               TEXT PRIMARY KEY,
  install_id       TEXT NOT NULL,
  upload_id        TEXT NOT NULL UNIQUE,
  sha256           TEXT NOT NULL,
  mime_type        TEXT NOT NULL,
  size_bytes       INTEGER NOT NULL CHECK (size_bytes > 0),
  fetch_token_hash TEXT NOT NULL UNIQUE,
  state            TEXT NOT NULL CHECK (state IN ('active','expired','deleted')),
  expires_at       DATETIME NOT NULL,
  created_at       DATETIME NOT NULL,
  deleted_at       DATETIME
);

CREATE TABLE media_uploads (
  id              TEXT PRIMARY KEY,
  install_id      TEXT NOT NULL,
  expected_sha256 TEXT NOT NULL,
  mime_type       TEXT NOT NULL,
  total_bytes     INTEGER NOT NULL CHECK (total_bytes > 0),
  received_bytes  INTEGER NOT NULL DEFAULT 0 CHECK (received_bytes >= 0 AND received_bytes <= total_bytes),
  state           TEXT NOT NULL CHECK (state IN ('open','completed','aborted','expired')),
  expires_at      DATETIME NOT NULL,
  created_at      DATETIME NOT NULL,
  updated_at      DATETIME NOT NULL,
  completed_at    DATETIME
);

CREATE TABLE provider_spend_daily (
  provider   TEXT NOT NULL CHECK (provider IN ('qwen')),
  period_day TEXT NOT NULL,
  spend_pusd INTEGER NOT NULL DEFAULT 0 CHECK (spend_pusd >= 0),
  requests   INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0),
  PRIMARY KEY (provider, period_day)
);

CREATE TABLE quota_monthly (
  install_id   TEXT NOT NULL,
  period_month TEXT NOT NULL,
  requests     INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0),
  PRIMARY KEY (install_id, period_month)
);

CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at DATETIME NOT NULL
);

CREATE TABLE spend_ledger (
  request_id       TEXT PRIMARY KEY,
  install_id       TEXT NOT NULL,
  provider         TEXT NOT NULL CHECK (provider IN ('qwen')),
  model            TEXT NOT NULL,
  rate_card_id     TEXT NOT NULL,
  period_month     TEXT NOT NULL,
  period_day       TEXT NOT NULL,
  reserved_pusd    INTEGER NOT NULL CHECK (reserved_pusd > 0),
  charged_pusd     INTEGER CHECK (charged_pusd >= 0),
  state            TEXT NOT NULL CHECK (state IN ('open', 'settled', 'rolled_back', 'orphaned')),
  sublimit_applied INTEGER NOT NULL DEFAULT 0 CHECK (sublimit_applied IN (0, 1)),
  category         TEXT NOT NULL DEFAULT '',
  category_units   INTEGER NOT NULL DEFAULT 0,
  created_at       DATETIME NOT NULL,
  terminal_at      DATETIME,
  CHECK (
    (state = 'open' AND charged_pusd IS NULL AND terminal_at IS NULL)
    OR (state = 'settled' AND charged_pusd IS NOT NULL AND terminal_at IS NOT NULL)
    OR (state = 'rolled_back' AND charged_pusd = 0 AND terminal_at IS NOT NULL)
    OR (state = 'orphaned' AND charged_pusd = reserved_pusd AND terminal_at IS NOT NULL)
  )
);

CREATE UNIQUE INDEX idx_install_voice_name ON install_voices (install_id, name);

CREATE INDEX idx_install_voice_owner ON install_voices (install_id);

CREATE INDEX idx_media_leases_expiry ON media_leases(state, expires_at);

CREATE INDEX idx_media_leases_install ON media_leases(install_id, created_at);

CREATE INDEX idx_media_uploads_expiry ON media_uploads(state, expires_at);

CREATE INDEX idx_media_uploads_install ON media_uploads(install_id, created_at);

CREATE INDEX idx_spend_ledger_open ON spend_ledger(state, created_at);

CREATE INDEX idx_spend_ledger_period_provider ON spend_ledger(period_day, provider);

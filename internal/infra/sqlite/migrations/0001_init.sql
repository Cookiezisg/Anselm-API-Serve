-- 0001_init: initial schema (8 tables + idx_ledger_open).
-- Forward-only DDL. IF NOT EXISTS makes fresh-database initialization idempotent;
-- later migrations (0002+) are real ALTERs and use no such guard.

-- installs: issued install identities. fingerprint is plaintext, risk-observation
-- only — never a merge/dedup key (dedup goes through hash; see install_fp_rate).
CREATE TABLE IF NOT EXISTS installs (
  id            TEXT PRIMARY KEY,
  public_key    BLOB NOT NULL,
  key_thumbprint TEXT NOT NULL UNIQUE,
  fingerprint   TEXT,
  client        TEXT,
  status        TEXT NOT NULL DEFAULT 'active',
  created_at    DATETIME NOT NULL,
  last_seen_at  DATETIME
);

-- usage: dual-purpose by period shape — 'YYYY-MM' holds monthly request count,
-- 'YYYY-MM-DD' holds daily reserved tokens (+ optional daily sublimit count).
CREATE TABLE IF NOT EXISTS usage (
  install_id    TEXT NOT NULL,
  period        TEXT NOT NULL,
  count         INTEGER NOT NULL DEFAULT 0,
  tokens        INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (install_id, period)
);

-- budget: one row per day — the global daily token budget guardrail.
CREATE TABLE IF NOT EXISTS budget (
  period        TEXT PRIMARY KEY,
  tokens_used   INTEGER NOT NULL DEFAULT 0,
  requests      INTEGER NOT NULL DEFAULT 0
);

-- ledger: pessimistic reservation ledger; settled IS NULL = in-flight.
CREATE TABLE IF NOT EXISTS ledger (
  request_id    TEXT PRIMARY KEY,
  install_id    TEXT NOT NULL,
  period_day    TEXT NOT NULL,
  reserved      INTEGER NOT NULL,
  settled       INTEGER,
  created_at    DATETIME NOT NULL
);
-- Serves open-reservation count (settled IS NULL) and orphan scan (... AND created_at < ?).
CREATE INDEX IF NOT EXISTS idx_ledger_open ON ledger(settled, created_at);

-- install_ip_rate: persisted per-IP hourly install rate bucket (survives restarts).
CREATE TABLE IF NOT EXISTS install_ip_rate (
  ip_key        TEXT NOT NULL,
  window_hour   TEXT NOT NULL,
  count         INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (ip_key, window_hour)
);

-- install_global_rate: global daily install coarse valve (Sybil gate, disabled by
-- default). Log-natured bucket keyed by reset-tz calendar day; no deleted_at.
CREATE TABLE IF NOT EXISTS install_global_rate (
  window_day    TEXT PRIMARY KEY,
  count         INTEGER NOT NULL DEFAULT 0
);

-- install_fp_rate: per-fingerprint daily cap + cooldown (Sybil gate, disabled by
-- default). Privacy red-line — stores ONLY SHA-256(fp), never plaintext.
CREATE TABLE IF NOT EXISTS install_fp_rate (
  fp_sha256     TEXT NOT NULL,
  window_day    TEXT NOT NULL,
  count         INTEGER NOT NULL DEFAULT 0,
  last_at       DATETIME,
  PRIMARY KEY (fp_sha256, window_day)
);

-- settings: runtime-config DB overlay (hot-reload). Key-value of ONLY the
-- dashboard-overridden runtime-tunable items; secrets never land here.
CREATE TABLE IF NOT EXISTS settings (
  key           TEXT PRIMARY KEY,
  value         TEXT NOT NULL,
  updated_at    DATETIME NOT NULL
);

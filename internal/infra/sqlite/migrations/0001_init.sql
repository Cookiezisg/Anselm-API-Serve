-- 0001_init: the complete gateway schema.
--
-- This file is a SQUASH of what were seven incremental migrations. That was only
-- possible because the gateway had not launched: there was no production data to
-- carry forward, so the schema could be restated instead of patched. A deployed
-- database must NEVER be treated this way.
--
-- What the squash removed, and why each was dead rather than merely old:
--   * the v1 token ledger (usage / budget / ledger) — superseded by the pUSD
--     accounting below; zero Go references, only its own migration tests.
--   * `gemini` and `deepseek` from the provider CHECK — one never shipped, the
--     other was retired from routing. A closed enum listing identities nothing
--     can write is not a constraint, it is a suggestion that reads like one.
--   * the `_next` table names left behind by SQLite's rebuild-and-rename dance,
--     and the ALTER-appended columns that landed at the end of a line instead of
--     where they belong.
--
-- 本文件是七个增量迁移的**压平**。它之所以做得成,是因为网关尚未上线:没有生产数据要带走,
-- 故 schema 可以被**重述**、而不是继续打补丁。**已部署的数据库绝不可以这样处理。**
--
-- 压平删掉的东西,以及每一样为什么是「死的」而不只是「旧的」:
--   * v1 token 账本(usage / budget / ledger)——已被下面的 pUSD 账本取代;Go 代码零引用,
--     只有它自己的迁移测试还在用。
--   * provider CHECK 里的 `gemini` 与 `deepseek`——一个从未上线,一个已撤出路由。一个列着
--     「没有东西写得进去的身份」的封闭枚举不是约束,是一条长得像约束的建议。
--   * SQLite「重建再改名」留下的 `_next` 表名,以及被 ALTER 追加到行尾、而不是待在该待的
--     位置上的那两列。

-- ── identity ────────────────────────────────────────────────────────────────
-- An install is the isolation unit: one device, one Ed25519 public key. There is
-- no user, no account and no password anywhere in this schema.
-- install 是隔离单元:一台设备、一把 Ed25519 公钥。整个 schema 里没有用户、没有账号、
-- 没有密码。
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

-- Issuance rate buckets. All three are dormant by default (their limits are 0)
-- and exist so the gates can be armed without a schema change.
-- 领号频控桶。三者默认全部休眠(限额为 0),存在的意义是让那几道闸随时能上膛而不必改 schema。
CREATE TABLE install_ip_rate (
  ip_key      TEXT NOT NULL,
  window_hour TEXT NOT NULL,
  count       INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (ip_key, window_hour)
);

CREATE TABLE install_global_rate (
  window_day TEXT PRIMARY KEY,
  count      INTEGER NOT NULL DEFAULT 0
);

-- fp_sha256 is a HASH, never a raw fingerprint: the gate needs to recognise a
-- repeat, not to know what the device is.
-- fp_sha256 是**哈希**、绝不是原始指纹:这道闸需要认出「又是它」,不需要知道它是什么。
CREATE TABLE install_fp_rate (
  fp_sha256  TEXT NOT NULL,
  window_day TEXT NOT NULL,
  count      INTEGER NOT NULL DEFAULT 0,
  last_at    DATETIME,
  PRIMARY KEY (fp_sha256, window_day)
);

-- ── config overlay ──────────────────────────────────────────────────────────
-- Runtime-hot settings only. Secrets are env-only and never reach this table.
-- 只放运行时可热改项。机密只走 env,永远到不了这张表。
CREATE TABLE settings (
  key        TEXT PRIMARY KEY,
  value      TEXT NOT NULL,
  updated_at DATETIME NOT NULL
);

-- ── accounting (pUSD) ───────────────────────────────────────────────────────
-- Balances are non-negative integer pico-US dollars (1 USD = 10^12 pUSD). Raw
-- provider tokens are converted through a frozen rate card BEFORE anything is
-- written here, so no two token vectors are ever added together.
-- 余额是非负整数 pico 美元(1 USD = 10^12 pUSD)。provider 的原始 token 在写入这里**之前**
-- 就已经过冻结费率卡换算,故任何两个 token 向量都不会被相加。

-- Gate 1: the per-install monthly request entitlement.
CREATE TABLE quota_monthly (
  install_id   TEXT NOT NULL,
  period_month TEXT NOT NULL,
  requests     INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0),
  PRIMARY KEY (install_id, period_month)
);

-- Statistics, not gates. These three record where the money went; only
-- quota_monthly and global_spend_monthly can deny a request.
-- 统计,不是闸。这三张记录钱去了哪里;能**拒绝**请求的只有 quota_monthly 与
-- global_spend_monthly。
CREATE TABLE install_spend_daily (
  install_id TEXT NOT NULL,
  period_day TEXT NOT NULL,
  spend_pusd INTEGER NOT NULL DEFAULT 0 CHECK (spend_pusd >= 0),
  requests   INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0),
  PRIMARY KEY (install_id, period_day)
);

CREATE TABLE global_spend_daily (
  period_day TEXT PRIMARY KEY,
  spend_pusd INTEGER NOT NULL DEFAULT 0 CHECK (spend_pusd >= 0),
  requests   INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0)
);

CREATE TABLE provider_spend_daily (
  provider   TEXT NOT NULL CHECK (provider IN ('qwen')),
  period_day TEXT NOT NULL,
  spend_pusd INTEGER NOT NULL DEFAULT 0 CHECK (spend_pusd >= 0),
  requests   INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0),
  PRIMARY KEY (provider, period_day)
);

-- Gate 2: the operator's shared monthly wallet. Together with quota_monthly
-- these are the only two rows a reservation can be denied by.
-- 闸 2:operator 的共享月钱包。它与 quota_monthly 是预留唯一可能被拒的两行。
CREATE TABLE global_spend_monthly (
  period_month TEXT PRIMARY KEY,
  spend_pusd   INTEGER NOT NULL DEFAULT 0 CHECK (spend_pusd >= 0),
  requests     INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0)
);

-- Gate 3: per-install per-day units for the category capabilities (image /
-- speech / video / voice). One mechanism serves all four; the unit differs per
-- category and is whatever a person actually rations (clips, not seconds).
-- 闸 3:品类能力(图 / 语音 / 视频 / 音色)的逐 install 每日单位。一套机制服务四个品类;
-- 单位随品类不同,取的是**人心里配给的那个东西**(是「条」,不是「秒」)。
CREATE TABLE install_category_daily (
  install_id TEXT    NOT NULL,
  category   TEXT    NOT NULL,
  period_day TEXT    NOT NULL,
  units      INTEGER NOT NULL DEFAULT 0 CHECK (units >= 0),
  PRIMARY KEY (install_id, category, period_day)
);

-- The reservation ledger. Its state machine is enforced in the schema rather
-- than only in Go: a terminal row must carry the evidence its state implies, so
-- a half-written settlement cannot be committed at all.
-- 预留账本。它的状态机由 **schema** 强制、不只靠 Go:终态行必须带着它那个状态所蕴含的证据,
-- 故一次写了一半的结算根本提交不上去。
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

CREATE INDEX idx_spend_ledger_open ON spend_ledger(state, created_at);
CREATE INDEX idx_spend_ledger_period_provider ON spend_ledger(period_day, provider);

-- ── durable media staging ───────────────────────────────────────────────────
-- Raw media never enters chat JSON. A proof-bound resumable upload lands here;
-- only an opaque, short-lived lease ever crosses into model traffic.
-- 原始媒体从不进入 chat JSON。proof 绑定的可续传上传落在这里;进入模型流量的只有一个
-- 不透明的、短命的 lease。
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

CREATE INDEX idx_media_uploads_expiry ON media_uploads(state, expires_at);
CREATE INDEX idx_media_uploads_install ON media_uploads(install_id, created_at);

-- fetch_token_hash is a HASH: the plaintext fetch token exists only in the URL
-- handed to the upstream, never at rest.
-- fetch_token_hash 是**哈希**:明文取件 token 只存在于交给上游的那个 URL 里,不落盘。
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

CREATE INDEX idx_media_leases_expiry ON media_leases(state, expires_at);
CREATE INDEX idx_media_leases_install ON media_leases(install_id, created_at);

-- ── cloned voices ───────────────────────────────────────────────────────────
-- The gateway's own row id is the handle a client sees; upstream_id never
-- crosses that line. If it did, any install could synthesize with any other
-- install's cloned voice simply by naming it.
-- 客户端看到的句柄是网关**自己的行 id**;upstream_id 绝不越过这条线。越过了,任何 install
-- 只要**指名**就能用别人的克隆音色合成。
CREATE TABLE install_voices (
  id          TEXT    PRIMARY KEY,
  install_id  TEXT    NOT NULL,
  name        TEXT    NOT NULL,
  upstream_id TEXT    NOT NULL,
  created_at  INTEGER NOT NULL
);

CREATE UNIQUE INDEX idx_install_voice_name ON install_voices (install_id, name);
CREATE INDEX idx_install_voice_owner ON install_voices (install_id);

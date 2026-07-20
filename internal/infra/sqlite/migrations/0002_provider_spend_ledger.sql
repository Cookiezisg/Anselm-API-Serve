-- 0002_provider_spend_ledger: provider-aware fixed-point cost accounting.
--
-- Shared quota balances are pico-US dollars (pUSD), never raw provider tokens.
-- The v1 tables remain intact for audit but are no longer written by the quota
-- store. Historical DeepSeek token balances are converted conservatively at
-- 280000 pUSD/token (the highest historical V4 Flash token dimension: output).

-- Refuse corrupt/overflowing v1 balances instead of letting SQLite promote an
-- overflowing multiplication to REAL. MaxInt64 / 280000 = 32940614417338.
-- TOTAL() is used only by this guard: unlike SUM() it cannot integer-overflow,
-- and every integer through this threshold is represented exactly as a double.
-- Once the guard passes, the integer SUM()*280000 floors below are safe.
CREATE TABLE accounting_v2_migration_guard (
  ok INTEGER NOT NULL CHECK (ok = 1)
);
INSERT INTO accounting_v2_migration_guard(ok)
SELECT CASE WHEN EXISTS (
  SELECT 1 FROM usage
   WHERE typeof(count) <> 'integer' OR typeof(tokens) <> 'integer'
      OR count < 0 OR tokens < 0 OR tokens > 32940614417338
  UNION ALL
  SELECT 1 FROM budget
   WHERE typeof(tokens_used) <> 'integer' OR typeof(requests) <> 'integer'
      OR tokens_used < 0 OR tokens_used > 32940614417338 OR requests < 0
  UNION ALL
  SELECT 1 FROM ledger
   WHERE typeof(reserved) <> 'integer'
      OR (settled IS NOT NULL AND typeof(settled) <> 'integer')
      OR reserved <= 0 OR reserved > 32940614417338
      OR settled < 0 OR settled > 32940614417338
  UNION ALL
  SELECT 1 FROM ledger
   WHERE settled IS NULL OR settled > 0
   GROUP BY install_id, period_day
  HAVING TOTAL(CASE WHEN settled IS NULL THEN reserved ELSE settled END) > 32940614417338
  UNION ALL
  SELECT 1 FROM ledger
   WHERE settled IS NULL OR settled > 0
   GROUP BY period_day
  HAVING TOTAL(CASE WHEN settled IS NULL THEN reserved ELSE settled END) > 32940614417338
  UNION ALL
  SELECT 1 FROM settings
   WHERE key IN ('GLOBAL_DAILY_BUDGET_TOKENS', 'INSTALL_DAILY_TOKEN_CAP')
     AND (value = '' OR value GLOB '*[^0-9]*'
          OR CAST(value AS INTEGER) <= 0
          OR CAST(value AS INTEGER) > 329406144173384846)
) THEN 0 ELSE 1 END;
DROP TABLE accounting_v2_migration_guard;

CREATE TABLE quota_monthly (
  install_id   TEXT NOT NULL,
  period_month TEXT NOT NULL,
  requests     INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0),
  PRIMARY KEY (install_id, period_month)
);

CREATE TABLE install_spend_daily (
  install_id  TEXT NOT NULL,
  period_day  TEXT NOT NULL,
  spend_pusd  INTEGER NOT NULL DEFAULT 0 CHECK (spend_pusd >= 0),
  requests    INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0),
  PRIMARY KEY (install_id, period_day)
);

CREATE TABLE provider_spend_daily (
  provider    TEXT NOT NULL CHECK (provider IN ('deepseek', 'gemini')),
  period_day  TEXT NOT NULL,
  spend_pusd  INTEGER NOT NULL DEFAULT 0 CHECK (spend_pusd >= 0),
  requests    INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0),
  PRIMARY KEY (provider, period_day)
);

CREATE TABLE global_spend_daily (
  period_day  TEXT PRIMARY KEY,
  spend_pusd  INTEGER NOT NULL DEFAULT 0 CHECK (spend_pusd >= 0),
  requests    INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0)
);

CREATE TABLE spend_ledger (
  request_id       TEXT PRIMARY KEY,
  install_id       TEXT NOT NULL,
  provider         TEXT NOT NULL CHECK (provider IN ('deepseek', 'gemini')),
  model            TEXT NOT NULL,
  rate_card_id     TEXT NOT NULL,
  period_month     TEXT NOT NULL,
  period_day       TEXT NOT NULL,
  reserved_pusd    INTEGER NOT NULL CHECK (reserved_pusd > 0),
  charged_pusd     INTEGER CHECK (charged_pusd >= 0),
  state            TEXT NOT NULL CHECK (state IN ('open', 'settled', 'rolled_back', 'orphaned')),
  sublimit_applied INTEGER NOT NULL DEFAULT 0 CHECK (sublimit_applied IN (0, 1)),
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

-- Preserve the monthly request entitlement and the optional daily request
-- sublimit count exactly. Only the old token columns require conversion.
INSERT INTO quota_monthly(install_id, period_month, requests)
SELECT install_id, period, count
  FROM usage
 WHERE length(period) = 7;

INSERT INTO install_spend_daily(install_id, period_day, spend_pusd, requests)
SELECT install_id, period, tokens * 280000, count
  FROM usage
 WHERE length(period) = 10;

-- Every v1 completion was DeepSeek-backed. The same conservative conversion is
-- used for provider and global aggregates so their balances stay consistent.
INSERT INTO provider_spend_daily(provider, period_day, spend_pusd, requests)
SELECT 'deepseek', period, tokens_used * 280000, requests
  FROM budget;

INSERT INTO global_spend_daily(period_day, spend_pusd, requests)
SELECT period, tokens_used * 280000, requests
  FROM budget;

-- The old orphan reconciler encoded an unknown outcome as settled=reserved but
-- refunded that reservation from usage/budget. That row is indistinguishable
-- from an ordinary full-cost settlement, so copying only the v1 balances could
-- understate a provider charge. Conservatively floor each wallet at the legacy
-- ledger's chargeable aggregate: open -> reserved, positive settled -> settled,
-- settled=0 (a proven rollback) -> excluded. MAX, rather than addition, keeps
-- ordinary reservations already present in usage/budget from being double-counted.
-- Every legacy call was DeepSeek-backed.
INSERT INTO install_spend_daily(install_id, period_day, spend_pusd, requests)
SELECT install_id,
       period_day,
       SUM(CASE WHEN settled IS NULL THEN reserved ELSE settled END) * 280000,
       0
  FROM ledger
 WHERE settled IS NULL OR settled > 0
 GROUP BY install_id, period_day
ON CONFLICT(install_id, period_day) DO UPDATE SET
  spend_pusd = MAX(install_spend_daily.spend_pusd, excluded.spend_pusd);

INSERT INTO provider_spend_daily(provider, period_day, spend_pusd, requests)
SELECT 'deepseek',
       period_day,
       SUM(CASE WHEN settled IS NULL THEN reserved ELSE settled END) * 280000,
       COUNT(*)
  FROM ledger
 WHERE settled IS NULL OR settled > 0
 GROUP BY period_day
ON CONFLICT(provider, period_day) DO UPDATE SET
  spend_pusd = MAX(provider_spend_daily.spend_pusd, excluded.spend_pusd),
  requests = MAX(provider_spend_daily.requests, excluded.requests);

INSERT INTO global_spend_daily(period_day, spend_pusd, requests)
SELECT period_day,
       SUM(CASE WHEN settled IS NULL THEN reserved ELSE settled END) * 280000,
       COUNT(*)
  FROM ledger
 WHERE settled IS NULL OR settled > 0
 GROUP BY period_day
ON CONFLICT(period_day) DO UPDATE SET
  spend_pusd = MAX(global_spend_daily.spend_pusd, excluded.spend_pusd),
  requests = MAX(global_spend_daily.requests, excluded.requests);

INSERT INTO spend_ledger(
  request_id, install_id, provider, model, rate_card_id,
  period_month, period_day, reserved_pusd, charged_pusd, state,
  sublimit_applied, created_at, terminal_at
)
SELECT
  request_id,
  install_id,
  'deepseek',
  'legacy-deepseek',
  'legacy-v1-max-280000-pusd',
  substr(period_day, 1, 7),
  period_day,
  reserved * 280000,
  CASE WHEN settled IS NULL THEN NULL ELSE settled * 280000 END,
  CASE WHEN settled IS NULL THEN 'open'
       WHEN settled = 0 THEN 'rolled_back'
       ELSE 'settled' END,
  0,
  created_at,
  CASE WHEN settled IS NULL THEN NULL ELSE created_at END
FROM ledger;

-- Runtime overlay keys changed units as well. Config exposes whole micro-USD
-- for operator ergonomics, while balances stay exact pUSD internally:
-- ceil(tokens * 280000 pUSD / 1000000 pUSD-per-microUSD)
-- = ceil(tokens * 28 / 100). Existing v2 overrides win on a reworked database.
INSERT OR IGNORE INTO settings(key, value, updated_at)
SELECT 'GLOBAL_DAILY_SPEND_MICRO_USD',
       CAST((CAST(value AS INTEGER) * 28 + 99) / 100 AS TEXT),
       updated_at
  FROM settings
 WHERE key = 'GLOBAL_DAILY_BUDGET_TOKENS';

INSERT OR IGNORE INTO settings(key, value, updated_at)
SELECT 'INSTALL_DAILY_SPEND_MICRO_USD',
       CAST((CAST(value AS INTEGER) * 28 + 99) / 100 AS TEXT),
       updated_at
  FROM settings
 WHERE key = 'INSTALL_DAILY_TOKEN_CAP';

-- MODEL_ALLOWLIST was the old public/provider model surface. The v2 router owns
-- two server-side aliases instead, so retaining this overlay would make the new
-- closed Specs registry reject startup. The env/default path supplies aliases.
DELETE FROM settings
 WHERE key IN ('MODEL_ALLOWLIST', 'GLOBAL_DAILY_BUDGET_TOKENS', 'INSTALL_DAILY_TOKEN_CAP');

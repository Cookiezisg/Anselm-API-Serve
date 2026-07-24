-- 0003_add_qwen_provider: admit Qwen provider-aware accounting.
--
-- This repository has not shipped a production database, so its initial
-- provider-ledger schema is intentionally clean: runtime accounting identities
-- are only DeepSeek and Qwen. A deployed schema must never be edited this way;
-- that is not this project's state.

CREATE TABLE provider_spend_daily_next (
  provider    TEXT NOT NULL CHECK (provider IN ('deepseek', 'gemini', 'qwen')),
  period_day  TEXT NOT NULL,
  spend_pusd  INTEGER NOT NULL DEFAULT 0 CHECK (spend_pusd >= 0),
  requests    INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0),
  PRIMARY KEY (provider, period_day)
);
INSERT INTO provider_spend_daily_next(provider, period_day, spend_pusd, requests)
SELECT provider, period_day, spend_pusd, requests FROM provider_spend_daily;
DROP TABLE provider_spend_daily;
ALTER TABLE provider_spend_daily_next RENAME TO provider_spend_daily;

CREATE TABLE spend_ledger_next (
  request_id       TEXT PRIMARY KEY,
  install_id       TEXT NOT NULL,
  provider         TEXT NOT NULL CHECK (provider IN ('deepseek', 'gemini', 'qwen')),
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
INSERT INTO spend_ledger_next(
  request_id, install_id, provider, model, rate_card_id, period_month,
  period_day, reserved_pusd, charged_pusd, state, sublimit_applied, created_at, terminal_at
)
SELECT request_id, install_id, provider, model, rate_card_id, period_month,
       period_day, reserved_pusd, charged_pusd, state, sublimit_applied, created_at, terminal_at
  FROM spend_ledger;
DROP TABLE spend_ledger;
ALTER TABLE spend_ledger_next RENAME TO spend_ledger;
CREATE INDEX idx_spend_ledger_open ON spend_ledger(state, created_at);
CREATE INDEX idx_spend_ledger_period_provider ON spend_ledger(period_day, provider);

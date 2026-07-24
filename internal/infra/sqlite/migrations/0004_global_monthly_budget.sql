-- 0004_global_monthly_budget: replace daily spend guardrails with one operator
-- monthly spend wallet.
--
-- Historical daily spend tables remain as accounting/dashboard statistics. The
-- new global_spend_monthly table is the only spend wallet that denies traffic.

CREATE TABLE global_spend_monthly (
  period_month TEXT PRIMARY KEY,
  spend_pusd   INTEGER NOT NULL DEFAULT 0 CHECK (spend_pusd >= 0),
  requests     INTEGER NOT NULL DEFAULT 0 CHECK (requests >= 0)
);

INSERT INTO global_spend_monthly(period_month, spend_pusd, requests)
SELECT substr(period_day, 1, 7), SUM(spend_pusd), SUM(requests)
  FROM global_spend_daily
 GROUP BY substr(period_day, 1, 7);

DELETE FROM settings
 WHERE key IN (
   'GLOBAL_DAILY_SPEND_MICRO_USD',
   'INSTALL_DAILY_SPEND_MICRO_USD',
   'DEEPSEEK_DAILY_SPEND_MICRO_USD',
   'QWEN_DAILY_SPEND_MICRO_USD'
 );

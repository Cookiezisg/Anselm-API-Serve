-- 0006_install_category_daily: per-install per-category daily unit ledger (WRK-082 批B, 代拍 B4).
--
-- One mechanism for every generation category: image reservations consume `units` = image count
-- (gated by IMAGE_DAILY_LIMIT); the later speech batch consumes `units` = characters against its
-- own limit. The gate itself is a conditional UPDATE inside the same BEGIN IMMEDIATE reserve
-- transaction as the monthly/wallet gates, so a partial reserve can never be observed.
--
-- spend_ledger gains the category snapshot columns for the same forgery-resistance discipline as
-- sublimit_applied: settle/rollback pin every reserved field, and rollback reverses exactly the
-- recorded units (never a re-read of live config).

CREATE TABLE install_category_daily (
  install_id TEXT    NOT NULL,
  category   TEXT    NOT NULL,
  period_day TEXT    NOT NULL,
  units      INTEGER NOT NULL DEFAULT 0 CHECK (units >= 0),
  PRIMARY KEY (install_id, category, period_day)
);

ALTER TABLE spend_ledger ADD COLUMN category TEXT NOT NULL DEFAULT '';
ALTER TABLE spend_ledger ADD COLUMN category_units INTEGER NOT NULL DEFAULT 0;

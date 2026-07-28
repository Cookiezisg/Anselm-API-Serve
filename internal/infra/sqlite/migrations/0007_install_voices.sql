-- 0007_install_voices: per-install cloned-voice INVENTORY (WRK-082 H9).
--
-- Why a table at all, when the desktop already keeps its own voices row: because the voice lives
-- upstream in OUR provider account, not the user's. Every install's clone is registered under one
-- DashScope credential, so this gateway is the only place that can answer two questions nobody
-- else can — "how many has this install got" (their inventory) and "how many exist in total"
-- (ours, against the provider's per-account ceiling). The desktop row is a client-side pointer;
-- this row is the thing that actually consumes a shared resource.
--
-- INVENTORY, NOT QUOTA — and the schema says so. A daily table is keyed by period_day and resets
-- itself by simply never matching tomorrow; this one has no period column at all, because nothing
-- frees a slot with the passage of time. A voice occupies its slot until someone deletes it, and
-- creation costs money once ($0.2/voice on the qwen-tts family — the only one reachable with a
-- base64 sample, see H9 第0步). Anything that reads like a refill here would be a lie.
--
-- The UNIQUE index is the anti-orphan rule: one (install, name) pair maps to exactly one upstream
-- registration. Enrolling twice under one name would strand the first one in our account, where
-- nothing can address it again and it would keep consuming the shared ceiling forever.
--
-- 0007_install_voices:逐 install 的克隆音色**库存**(H9)。
--
-- 桌面端已有自己的 voices 行,为什么这里还要一张表:因为音色住在**我们的** provider 账号里、不是用户的。
-- 每个 install 的克隆都登记在**同一把** DashScope 凭证之下,故本网关是唯一能回答那两个别处答不了的问题
-- 的地方——「这个 install 有几个」(他的库存)与「总共存在几个」(我们的,对着供应商的账号级上限)。
-- 桌面那一行是客户端侧的**指针**;这一行才是真正消耗共享资源的那个东西。
--
-- **是库存、不是配额——schema 本身就这么说。** 日表按 period_day 作键、靠「明天根本匹配不上」自我重置;
-- 这张表**根本没有周期列**,因为**时间的流逝不会腾出任何位置**。一个音色占着它的位直到有人删掉,而创建
-- 花一次钱($0.2/音色,qwen-tts 那一支——唯一收 base64 样本的那家,见 H9 第0步)。这里任何读起来像
-- 「会续」的东西都是撒谎。
--
-- UNIQUE 索引是那条**防孤儿**规则:一对 (install, name) 恰好映射到一个上游登记。同名登记两次,会让
-- 第一个搁浅在我们账号里——再没有东西够得着它,而它会永远占着那份共享上限。

CREATE TABLE install_voices (
  id          TEXT    PRIMARY KEY,
  install_id  TEXT    NOT NULL,
  name        TEXT    NOT NULL,
  upstream_id TEXT    NOT NULL,
  created_at  INTEGER NOT NULL
);

CREATE UNIQUE INDEX idx_install_voice_name ON install_voices (install_id, name);
CREATE INDEX idx_install_voice_owner ON install_voices (install_id);

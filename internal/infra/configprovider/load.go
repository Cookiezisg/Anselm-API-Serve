// Package configprovider is the ONLY stateful config holder: it reads every env
// key into an immutable config.Config (LoadBase), assembles the DB overlay over
// env defaults at boot (LoadWithOverlay), and serves the live snapshot lock-free
// behind an atomic.Pointer (Load) with a serialized write path (ApplyOverrides:
// validate → persist all-or-nothing → atomic swap). The pure rules (bounds, tier
// table, cross-field semantics, PERF-2 budget) live in domain/config; this layer
// owns only the I/O (env + DB) and the atomic state.
//
// 加载顺序:env 默认/机密/硬约束(LoadBase)← DB overlay 覆盖运行时可改项
// (LoadWithOverlay)。读路径 Load() 无锁取当前 atomic 快照(每请求快照一次,热更
// 新永不在单请求内半旧半新);写路径 ApplyOverrides 在写锁下:domain 校验 → 全或无
// 持久化 → 原子 swap(持久化失败不 swap)。机密(DEEPSEEK_API_KEY / DASHBOARD_* /
// INSTALL_POW_SECRET)只读 env,绝不入 overlay、绝不在 Dump/Snapshot 出真值。
package configprovider

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/config"
)

// Named fail-fast errors for the required-secret / required-list keys, so callers
// (and tests) can assert the precise cause without string matching.
var (
	ErrDeepSeekKeyRequired   = errors.New("DEEPSEEK_API_KEY is required")
	ErrModelAllowlistMissing = errors.New("MODEL_ALLOWLIST is required")
)

// LoadBase reads EVERY env key (via the injected getenv so tests stay hermetic),
// applies the spec defaults, parses + bounds each value with the SAME ceilings the
// override path uses (domain/config Bound*/Max*), runs the cross-field semantic
// checks and the PERF-2 memory-budget self-check, and fails fast (named errors) on
// any bad value. A bad RESET_TZ PANICS (never a silent UTC fallback — that would
// shift every period boundary by 8h, 蓝图 §7.1 红线). Secrets are read here and
// only here (env-only); they never leave via Dump/Snapshot in the clear.
func LoadBase(getenv func(string) string) (config.Config, error) {
	g := &envReader{getenv: getenv}
	c := config.Config{}

	// DeepSeek key(s) — the one required secret; comma-separated, first is primary.
	rawKeys := getenv("DEEPSEEK_API_KEY")
	if strings.TrimSpace(rawKeys) == "" {
		return config.Config{}, ErrDeepSeekKeyRequired
	}
	for _, k := range strings.Split(rawKeys, ",") {
		if k = strings.TrimSpace(k); k != "" {
			c.DeepSeekAPIKeys = append(c.DeepSeekAPIKeys, k)
		}
	}
	if len(c.DeepSeekAPIKeys) == 0 {
		return config.Config{}, ErrDeepSeekKeyRequired
	}

	c.DeepSeekBaseURL = strings.TrimRight(g.str("DEEPSEEK_BASE_URL", "https://api.deepseek.com"), "/")

	allow := getenv("MODEL_ALLOWLIST")
	if strings.TrimSpace(allow) == "" {
		return config.Config{}, ErrModelAllowlistMissing
	}
	for _, m := range strings.Split(allow, ",") {
		if m = strings.TrimSpace(m); m != "" {
			c.ModelAllowlist = append(c.ModelAllowlist, m)
		}
	}
	if len(c.ModelAllowlist) == 0 {
		return config.Config{}, ErrModelAllowlistMissing
	}
	c.DefaultModel = c.ModelAllowlist[0]

	// --- runtime-hot numeric knobs (env default ← bounded, shared ceilings) ---
	c.MonthlyQuota = g.boundedInt64("MONTHLY_QUOTA", 5000, 1, config.MaxMonthlyQuota)

	c.GlobalDailyBudget = g.int64("GLOBAL_DAILY_BUDGET_TOKENS", 0)
	if c.GlobalDailyBudget <= 0 {
		g.fail("GLOBAL_DAILY_BUDGET_TOKENS must be > 0 (the only wallet guardrail)")
	}
	g.bound64("GLOBAL_DAILY_BUDGET_TOKENS", c.GlobalDailyBudget, 1, config.MaxGlobalDailyBudget)

	c.InstallDailyTokenCap = g.int64("INSTALL_DAILY_TOKEN_CAP", 0)
	if c.InstallDailyTokenCap <= 0 {
		g.fail("INSTALL_DAILY_TOKEN_CAP must be > 0")
	}
	g.bound64("INSTALL_DAILY_TOKEN_CAP", c.InstallDailyTokenCap, 1, config.MaxInstallDailyTokenCap)

	c.MaxTokensCap = g.boundedInt64("MAX_TOKENS_CAP", 4096, 1, config.MaxTokensCap)
	c.InputTokenCap = g.boundedInt64("INPUT_TOKEN_CAP", 16384, 1, config.MaxInputTokenCap)
	c.MaxMessages = g.boundedInt("MAX_MESSAGES", 256, 1, config.MaxMessages)
	c.MaxMessageChars = g.boundedInt("MAX_MESSAGE_CHARS", 131072, 1, config.MaxMessageChars)
	c.NGlobalConcurrency = g.boundedInt("N_GLOBAL_CONCURRENCY", 8, 1, config.MaxNGlobalConcurrency)
	c.RatePerMin = g.boundedInt("RATE_PER_MIN", 20, 0, config.MaxRatePerMin)
	c.DailySublimit = g.boundedInt64("DAILY_SUBLIMIT", 0, 0, config.MaxDailySublimit)
	c.InstallPerIPHour = g.boundedInt("INSTALL_PER_IP_HOUR", 10, 1, config.MaxInstallPerIPHour)

	// M2 Sybil 领号闸:全部默认 0=禁用(默认配置下 /v1/install 行为逐字不变)。
	c.InstallGlobalDailyCap = g.boundedInt64("INSTALL_GLOBAL_DAILY_CAP", 0, 0, config.MaxInstallGlobalDailyCap)
	c.InstallPerFPDaily = g.boundedInt64("INSTALL_PER_FP_DAILY", 0, 0, config.MaxInstallPerFPDaily)
	c.InstallPerFPCooldownSec = g.boundedInt("INSTALL_PER_FP_COOLDOWN_SEC", 0, 0, config.MaxInstallPerFPCooldownSec)

	// M2 领号 PoW(默认 off=dormant);非法 mode fail-fast;secret 是 env-only 机密。
	c.InstallPowMode = g.str("INSTALL_POW_MODE", config.PowModeOff)
	if err := config.ValidatePowMode(c.InstallPowMode); err != nil {
		g.failErr(err)
	}
	c.InstallPowDifficulty = g.boundedInt("INSTALL_POW_DIFFICULTY", 20, 1, config.MaxInstallPowDifficulty)
	loadPowSecret(getenv, &c)

	// M2 per-token 异常自动降速(默认 0=禁用)。FACTOR/COOLDOWN 仅 ANOMALY_RPM>0 时读。
	c.TokenAnomalyRPM = g.boundedInt("TOKEN_ANOMALY_RPM", 0, 0, config.MaxTokenAnomalyRPM)
	c.TokenThrottleFactor = g.boundedInt("TOKEN_THROTTLE_FACTOR", 4, 1, config.MaxTokenThrottleFactor)
	c.TokenThrottleCooldownSec = g.boundedInt("TOKEN_THROTTLE_COOLDOWN_SEC", 300, 1, config.MaxTokenThrottleCooldownSec)

	hdrSec := g.boundedInt("UPSTREAM_HEADER_TIMEOUT_SEC", 60, 1, config.MaxUpstreamHeaderTimeoutSec)
	c.UpstreamHeaderTimeout = time.Duration(hdrSec) * time.Second
	queueMS := g.boundedInt("QUEUE_WAIT_MS", 1500, 0, config.MaxQueueWaitMS)
	c.QueueWait = time.Duration(queueMS) * time.Millisecond

	// --- startup-hard PERF-2 memory-budget inputs (env-only, restart to change) ---
	c.GoMemLimitMiB = g.int("GOMEMLIMIT_MIB", 768)
	if c.GoMemLimitMiB < 0 {
		g.fail("GOMEMLIMIT_MIB must be >= 0 (0=disable)")
	}
	c.SQLiteCacheKiB = g.int("SQLITE_CACHE_KIB", 32768)
	if c.SQLiteCacheKiB <= 0 {
		g.fail("SQLITE_CACHE_KIB must be > 0")
	}
	mmapMB := g.int("SQLITE_MMAP_MB", 256)
	if mmapMB < 0 {
		g.fail("SQLITE_MMAP_MB must be >= 0 (0=disable mmap)")
	}
	c.SQLiteMmapBytes = int64(mmapMB) * 1024 * 1024
	c.SQLiteAutockptPage = g.int("SQLITE_WAL_AUTOCHECKPOINT", 4000)
	if c.SQLiteAutockptPage < 0 {
		g.fail("SQLITE_WAL_AUTOCHECKPOINT must be >= 0")
	}
	c.ReadPoolMaxConns = g.int("READ_POOL_MAX_CONNS", 4)
	if c.ReadPoolMaxConns <= 0 {
		g.fail("READ_POOL_MAX_CONNS must be > 0")
	}
	c.MemBudgetMiB = g.int("MEM_BUDGET_MIB", 2048)
	if c.MemBudgetMiB <= 0 {
		g.fail("MEM_BUDGET_MIB must be > 0")
	}
	c.MemSafetyMarginMiB = g.int("MEM_SAFETY_MARGIN_MIB", 400)
	if c.MemSafetyMarginMiB < 0 {
		g.fail("MEM_SAFETY_MARGIN_MIB must be >= 0")
	}
	if g.err != nil {
		return config.Config{}, g.err
	}
	// PERF-2 self-check (must run on the assembled budget inputs). Advisory WARN to
	// stderr only when GOMEMLIMIT=0 (heap unbounded); a real soft-limit overflow
	// fails fast — these inputs are startup-hard exactly so this can't be bypassed.
	advisory, err := c.ValidateMemoryBudget()
	if err != nil {
		return config.Config{}, err
	}
	if advisory {
		worst := c.WorstCaseMemoryMiB()
		ceiling := int64(c.MemBudgetMiB) - int64(c.MemSafetyMarginMiB)
		fmt.Fprintf(os.Stderr,
			"WARN config: worst-case memory %dMiB exceeds budget %dMiB - safety %dMiB = %dMiB, "+
				"but GOMEMLIMIT_MIB=0 (heap unbounded) so this is advisory; size the box accordingly\n",
			worst, c.MemBudgetMiB, c.MemSafetyMarginMiB, ceiling)
	}

	// REL-6 磁盘满防护阈值。
	c.DiskMinMB = g.boundedInt("DISK_MIN_MB", 500, 0, config.MaxDiskMinMB)
	c.DiskMinPercent = g.int("DISK_MIN_PERCENT", 5)
	if c.DiskMinPercent < 0 || c.DiskMinPercent > 100 {
		g.fail("DISK_MIN_PERCENT must be 0..100 (0=disable percent floor)")
	}

	// RESET_TZ — fail-fast PANIC on an unresolvable zone (never silent UTC).
	c.ResetTZ = g.str("RESET_TZ", "Asia/Shanghai")
	loc, lerr := time.LoadLocation(c.ResetTZ)
	if lerr != nil {
		panic(fmt.Sprintf("config: LoadLocation(%q) failed: %v", c.ResetTZ, lerr))
	}
	c.Location = loc

	// Cross-field semantics (SEC-2) on the assembled config.
	if g.err == nil {
		if err := c.ValidateSemantics(); err != nil {
			g.failErr(err)
		}
	}

	// Three physically-isolated listeners (ADR-004): the three addrs must differ.
	c.ListenAddr = g.str("LISTEN_ADDR", "127.0.0.1:8080")
	c.AdminAddr = g.str("ADMIN_ADDR", "127.0.0.1:9090")
	c.DashboardAddr = g.str("DASHBOARD_ADDR", "127.0.0.1:8081")
	if c.DashboardAddr == c.ListenAddr {
		g.fail(fmt.Sprintf("DASHBOARD_ADDR %q must not equal LISTEN_ADDR %q (the three listeners must be physically isolated)", c.DashboardAddr, c.ListenAddr))
	}
	if c.DashboardAddr == c.AdminAddr {
		g.fail(fmt.Sprintf("DASHBOARD_ADDR %q must not equal ADMIN_ADDR %q (the three listeners must be physically isolated)", c.DashboardAddr, c.AdminAddr))
	}
	c.LogLevel = g.str("LOG_LEVEL", "info")
	c.DBPath = g.str("GATEWAY_DB_PATH", "anselm-gateway.db")

	// Dashboard auth secret (env-only): both set or both empty.
	c.DashboardUser = g.str("DASHBOARD_USER", "")
	c.DashboardPassword = g.str("DASHBOARD_PASSWORD", "")
	if (c.DashboardUser == "") != (c.DashboardPassword == "") {
		g.fail("DASHBOARD_USER and DASHBOARD_PASSWORD must be set together (or both empty)")
	}
	c.DashboardDevInsecureCookie = g.boolean("DASHBOARD_DEV_INSECURE_COOKIE", false)

	// Operator unmetered allowlist (env-only, GW-INV-14 discipline): comma-separated
	// hex SHA-256(token). Whitelisted tokens bypass ALL risk controls (god-mode).
	// Empty = dormant. A malformed entry fails fast — a typo must not silently leave
	// the operator's device un-whitelisted (and still risk-controlled).
	c.WhitelistTokenSHA256 = loadWhitelist(getenv, g)

	if g.err != nil {
		return config.Config{}, g.err
	}
	return c, nil
}

// loadWhitelist parses WHITELIST_TOKEN_SHA256 into a hex-hash set. Each entry is
// trimmed, lower-cased, and validated as a 64-char hex SHA-256; a malformed entry
// records a fail-fast error via g (never silently dropped). Empty/unset → nil (the
// dormant zero-value; IsUnmetered/HasUnmetered both read it as disabled).
func loadWhitelist(getenv func(string) string, g *envReader) map[string]struct{} {
	raw := strings.TrimSpace(getenv("WHITELIST_TOKEN_SHA256"))
	if raw == "" {
		return nil
	}
	set := make(map[string]struct{})
	for idx, h := range strings.Split(raw, ",") {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if !isHex64(h) {
			// NEVER echo the raw entry: the most likely operator mistake is pasting the
			// TOKEN itself instead of its SHA-256, and that plaintext token would
			// otherwise land in stderr/journald in the clear. Report only the position
			// + length (low-cardinality, non-secret) — enough to fix the typo.
			g.fail(fmt.Sprintf("WHITELIST_TOKEN_SHA256: entry #%d is not a 64-char hex SHA-256 (got %d chars); set SHA-256(token), not the token itself", idx+1, len(h)))
			return nil
		}
		set[h] = struct{}{}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// isHex64 reports whether s is exactly 64 lowercase hex digits (a SHA-256 hex).
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// loadPowSecret resolves the env-only INSTALL_POW_SECRET into the in-memory key +
// a SECRET-SAFE source marker (never the value). It NEVER mints a secret: present
// → "configured", absent → "disabled" (nil key). The "mode≠off requires a secret"
// invariant is enforced separately in ValidateSemantics + the override apply.
//
// 机密纯 env 解析:配了→configured;没配→disabled(nil),绝不自动生成。
func loadPowSecret(getenv func(string) string, c *config.Config) {
	if raw := strings.TrimSpace(getenv("INSTALL_POW_SECRET")); raw != "" {
		c.InstallPowSecret = []byte(raw)
		c.InstallPowSecretSource = "configured"
		return
	}
	c.InstallPowSecret = nil
	c.InstallPowSecretSource = "disabled"
}

// envReader accumulates the FIRST parse/bound error (so LoadBase reports one clear
// cause) while keeping each call site a single expression. getenv is injected so
// tests need no real process env.
type envReader struct {
	getenv func(string) string
	err    error
}

func (r *envReader) fail(msg string) {
	if r.err == nil {
		r.err = errors.New(msg)
	}
}

func (r *envReader) failErr(err error) {
	if r.err == nil {
		r.err = err
	}
}

func (r *envReader) str(key, def string) string {
	if v := strings.TrimSpace(r.getenv(key)); v != "" {
		return v
	}
	return def
}

func (r *envReader) int(key string, def int) int {
	v := strings.TrimSpace(r.getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		r.fail(fmt.Sprintf("%s: invalid int %q", key, v))
		return def
	}
	return n
}

func (r *envReader) int64(key string, def int64) int64 {
	v := strings.TrimSpace(r.getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		r.fail(fmt.Sprintf("%s: invalid int %q", key, v))
		return def
	}
	return n
}

func (r *envReader) boolean(key string, def bool) bool {
	v := strings.TrimSpace(r.getenv(key))
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		r.fail(fmt.Sprintf("%s: invalid bool %q", key, v))
		return def
	}
	return b
}

func (r *envReader) bound(key string, v, min, max int) {
	if err := config.BoundInt(key, v, min, max); err != nil {
		r.failErr(err)
	}
}

func (r *envReader) bound64(key string, v, min, max int64) {
	if err := config.BoundInt64(key, v, min, max); err != nil {
		r.failErr(err)
	}
}

func (r *envReader) boundedInt(key string, def, min, max int) int {
	n := r.int(key, def)
	r.bound(key, n, min, max)
	return n
}

func (r *envReader) boundedInt64(key string, def, min, max int64) int64 {
	n := r.int64(key, def)
	r.bound64(key, n, min, max)
	return n
}

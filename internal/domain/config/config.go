// Package config is the PURE config domain: the immutable Config value, its
// per-field bounds, the tier descriptor table, cross-field semantic validation,
// the PERF-2 worst-case-RSS budget guard, and an all-or-nothing pure
// ApplyOverrides for the runtime-hot subset. It has ZERO I/O — no os, no
// database/sql, no net/http. Env reading and DB overlay persistence are infra's
// job (infra/configprovider, infra/store/settingsstore); this package only owns
// the types + the rules, so both the env-load path and the hot-override path
// share one source of truth for bounds and semantics and can never diverge.
//
// 配置三类:① runtime-hot(后台可改、存 DB、原子热生效);② secret-env-only
// (机密:env only、绝不入库、dump 只掩码);③ startup-hard(内存预算项 / 监听 /
// tz / DB 路径:env only,改需重启)。本包是纯类型 + 规则层,不读 env、不碰 DB。
package config

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
)

// errMsg is a tiny constructor for static-message errors (keeps spec.go's
// rejection paths terse without an errors import there).
func errMsg(msg string) error { return errors.New(msg) }

// Hot-editable numeric ceilings (per-spec maxima). Each runtime-tunable number
// has both a floor AND a sane upper bound so a dashboard edit — or a hand-edited
// settings row — can never set a value that exhausts memory (a huge
// MAX_MESSAGE_CHARS × MAX_MESSAGES buffers gigabytes per request) or silently
// nullifies a wallet/OWASP-API4 guardrail (an astronomically large budget/cap is
// a non-guardrail). The same ceilings gate BOTH the env-load path (infra) and the
// override path (ApplyOverrides) so the two can never disagree. Exported so infra
// can reuse them when parsing env.
//
// 每个运行时可改数值项都设合理上界(不止下界):防 OOM 与「天文数字=护栏形同虚设」。
// env 与 overlay 两路共用同一套天花板,绝不分歧。
const (
	MaxMonthlyQuota       int64 = 1_000_000_000
	MaxPublicModelIDBytes int   = 128
	// Spend knobs are entered as integer micro-USD and converted exactly to the
	// pico-USD accounting unit. $9M/month is already far beyond this gateway's
	// intended envelope while leaving multiplication headroom in int64.
	MaxMonthlySpendMicroUSD int64 = 9_000_000_000_000
	MaxTokensCap            int64 = 1_000_000
	MaxInputTokenCap        int64 = 10_000_000
	MaxMessages             int   = 100_000
	MaxMessageChars         int   = 16 * 1024 * 1024
	MaxMediaParts           int   = 64
	MaxMediaDecodedBytes    int64 = 8 * 1024 * 1024
	// MaxBodyBytesCeiling bounds MAX_BODY_BYTES: bodies are fully buffered (~3.5×
	// peak per in-flight request incl. decode + upstream re-marshal), so the
	// ceiling is what the reference 2G box tolerates at N_GLOBAL — never higher.
	MaxBodyBytesCeiling         int64 = 8 * 1024 * 1024
	MinBodyBytes                int64 = 4 * 1024
	MaxNGlobalConcurrency       int   = 100_000
	MaxRatePerMin               int   = 10_000_000
	MaxDailySublimit            int64 = 1_000_000_000
	MaxInstallPerIPHour         int   = 1_000_000
	MaxInstallGlobalDailyCap    int64 = 100_000_000
	MaxInstallPerFPDaily        int64 = 1_000_000
	MaxInstallPerFPCooldownSec  int   = 86_400
	MaxInstallPowDifficulty     int   = 32
	MaxTokenAnomalyRPM          int   = 10_000_000
	MaxTokenThrottleFactor      int   = 1000
	MaxTokenThrottleCooldownSec int   = 86_400
	MaxQueueWaitMS              int   = 60_000
	MaxUpstreamHeaderTimeoutSec int   = 600
	MaxDiskMinMB                int   = 1024 * 1024 * 1024
)

// PoW mode enum (reviewer B3 — three-state, not bool). off is the default and
// keeps the whole PoW path dormant (byte-for-byte unchanged /v1/install).
//
// 领号 PoW 三态:off(dormant)/ shadow(验但失败仍放行,灰度观测)/ enforce。
const (
	PowModeOff     = "off"
	PowModeShadow  = "shadow"
	PowModeEnforce = "enforce"
)

// Config is the immutable, validated runtime configuration. Client-facing model
// identity, provider targets, and fixed-point spend wallets are explicit facts;
// there are no token-budget or model-allowlist compatibility mirrors.
//
// 全配置启动期一次性读入并校验,运行期只读。客户端逻辑模型、上游目标与定点金额
// 钱包各自显式表达,不存在旧白名单/令牌预算镜像。
type Config struct {
	DeepSeekAPIKeys []string // DEEPSEEK_API_KEY(逗号分隔多 key,第一个为主)
	DeepSeekBaseURL string   // DEEPSEEK_BASE_URL
	KimiAPIKeys     []string // KIMI_API_KEY(可选;未配置则多模态能力明确不可用)
	KimiBaseURL     string   // KIMI_BASE_URL(OpenAI compatibility base)

	// PublicModelID is the single client-facing logical model. Actual upstream
	// models are construction/config facts selected solely by content capability;
	// a client model string never selects a provider or a price tier.
	PublicModelID           string // PUBLIC_MODEL_ID(default anselm-auto)
	TextUpstreamModel       string // TEXT_UPSTREAM_MODEL(exact priced DeepSeek id)
	MultimodalUpstreamModel string // MULTIMODAL_UPSTREAM_MODEL(exact priced Kimi id)

	MonthlyQuota           int64 // MONTHLY_QUOTA(次数,用户可见额度)
	GlobalMonthlySpendPUSD int64 // GLOBAL_MONTHLY_SPEND_MICRO_USD converted to pUSD
	MaxTokensCap           int64 // MAX_TOKENS_CAP(caller max_tokens 保险丝;兼缺省请求预留额输出分量)
	// InputTokenCap is a compatibility-only legacy setting. Byte estimates are
	// not tokenizer truth and no longer reject requests; the field remains so
	// existing env/dashboard overlays keep loading during a rolling upgrade.
	InputTokenCap        int64 // INPUT_TOKEN_CAP(已弃用、无执行效果；上下文由上游模型判定)
	MaxMessages          int   // MAX_MESSAGES(messages 数组元素数上限,OWASP API4)
	MaxMessageChars      int   // MAX_MESSAGE_CHARS(单条 message content 字符数上限)
	MaxMediaParts        int   // MAX_MEDIA_PARTS(整请求媒体 part 数上限)
	MaxMediaDecodedBytes int64 // MAX_MEDIA_DECODED_BYTES(base64 解码后总字节)
	MaxBodyBytes         int64 // MAX_BODY_BYTES(请求体字节上限;内存保护,重启生效)
	NGlobalConcurrency   int   // N_GLOBAL_CONCURRENCY(全局在飞并发)
	RatePerMin           int   // RATE_PER_MIN(每 install 分钟令牌桶)
	DailySublimit        int64 // DAILY_SUBLIMIT(每 install 日次数子限额;0=禁用)
	InstallPerIPHour     int   // INSTALL_PER_IP_HOUR(/install 单 IP 时频控)

	// M2 防 Sybil 领号闸(全部默认 0=禁用,默认配置下 /v1/install 行为逐字不变):
	InstallGlobalDailyCap   int64 // INSTALL_GLOBAL_DAILY_CAP(全局每日领号粗阀;0=禁用,须远高于真实日活)
	InstallPerFPDaily       int64 // INSTALL_PER_FP_DAILY(同 fingerprint 当日领号上限;0=禁用)
	InstallPerFPCooldownSec int   // INSTALL_PER_FP_COOLDOWN_SEC(同 fingerprint 相邻领号最小间隔秒;0=禁用)

	// M2 批次3 领号 PoW 钩子(默认 off=dormant):
	InstallPowMode         string // INSTALL_POW_MODE(off|shadow|enforce,默认 off)
	InstallPowDifficulty   int    // INSTALL_POW_DIFFICULTY(前导零 bit 数,默认 20,界 [1,32])
	InstallPowSecret       []byte // INSTALL_POW_SECRET(env-only 机密;mode≠off 时必须显式配)
	InstallPowSecretSource string // configured/disabled 供 SECRET-SAFE 快照,绝不报真值

	// M2 批次2 per-token 异常自动降速(默认 0=禁用):
	TokenAnomalyRPM          int // TOKEN_ANOMALY_RPM(per-install 异常 RPM 触发点;0=禁用整套自动降速)
	TokenThrottleFactor      int // TOKEN_THROTTLE_FACTOR(降速倍数=RATE_PER_MIN/此值;1=逃生口)
	TokenThrottleCooldownSec int // TOKEN_THROTTLE_COOLDOWN_SEC(单次降速持续秒,到期自动回归)

	UpstreamHeaderTimeout time.Duration // UPSTREAM_HEADER_TIMEOUT_SEC(connect→header;不盖流式 body)
	QueueWait             time.Duration // QUEUE_WAIT_MS(N_global 满时有界等待窗口,REL-7)

	// PERF-2 内存预算自检项(startup-hard:改需重启,否则绕过最坏 RSS 防线)。
	// cache_size 是 per-connection:写池 1 份 + 读池 READ_POOL_MAX_CONNS 份,故乘子
	// 是 (1+READ_POOL) 而非常被误算的 ×2。
	GoMemLimitMiB      int   // GOMEMLIMIT_MIB(debug.SetMemoryLimit 软上限)
	SQLiteCacheKiB     int   // SQLITE_CACHE_KIB(每连接 page cache 上限 KiB)
	SQLiteMmapBytes    int64 // SQLITE_MMAP_BYTES(mmap_size 字节)
	SQLiteAutockptPage int   // SQLITE_WAL_AUTOCHECKPOINT(WAL 自动 checkpoint 触发页数)
	ReadPoolMaxConns   int   // READ_POOL_MAX_CONNS(只读池并发上限;每连接一份 cache)
	MemBudgetMiB       int   // MEM_BUDGET_MIB(总内存预算)
	MemSafetyMarginMiB int   // MEM_SAFETY_MARGIN_MIB(为 OS/runtime 突发预留的余量下限)

	// REL-6 磁盘满 / WAL 膨胀防护:数据盘剩余低于阈值进只读降级。
	DiskMinMB      int // DISK_MIN_MB(数据盘剩余绝对下限 MiB)
	DiskMinPercent int // DISK_MIN_PERCENT(剩余百分比下限;0=禁用百分比判定)

	ResetTZ  string         // RESET_TZ
	Location *time.Location // LoadLocation(ResetTZ) 结果

	ListenAddr    string // LISTEN_ADDR
	AdminAddr     string // ADMIN_ADDR(/metrics 独立 admin 端口;必须 loopback)
	DashboardAddr string // DASHBOARD_ADDR(管理后台独立 loopback 监听)
	// DashboardAuthMode is an env-only, restart-required trust-boundary choice:
	// disabled starts no dashboard listener; builtin keeps the Go session/CSRF
	// login; external delegates authentication to a loopback-adjacent IAP such as
	// Cloudflare Access or Tailscale. It is intentionally not dashboard-editable.
	DashboardAuthMode string // DASHBOARD_AUTH_MODE(disabled|builtin|external)
	LogLevel          string // LOG_LEVEL(debug|info|warn|error)
	DBPath            string // GATEWAY_DB_PATH(SQLite 落盘位置)

	// 管理后台鉴权(机密类:env only,绝不入 settings 表,dump 只掩码)。
	DashboardUser     string // DASHBOARD_USER
	DashboardPassword string // DASHBOARD_PASSWORD

	// DEV-ONLY:无 Caddy/TLS 的本机 plain-HTTP 登录时关 cookie Secure 标志。
	// 生产恒 false(关了 cookie 在明文 HTTP 上可被嗅探/注入)。
	DashboardDevInsecureCookie bool // DASHBOARD_DEV_INSECURE_COOKIE
}

const (
	DashboardAuthModeDisabled = "disabled"
	DashboardAuthModeBuiltin  = "builtin"
	DashboardAuthModeExternal = "external"
)

// ValidateDashboardAuthMode closes the dashboard trust-boundary enum. A typo
// must never silently turn off a login wall or assume a nonexistent upstream IAP.
func ValidateDashboardAuthMode(mode string) error {
	switch mode {
	case DashboardAuthModeDisabled, DashboardAuthModeBuiltin, DashboardAuthModeExternal:
		return nil
	default:
		return fmt.Errorf("DASHBOARD_AUTH_MODE %q invalid: must be one of disabled|builtin|external", mode)
	}
}

// BoundInt64 / BoundInt enforce an inclusive [min,max] range on a parsed value,
// naming the offending key + the bound it crossed. Exported so the infra env-load
// path and the override path share one set of ceilings — a number can never be
// set past its OOM/guardrail bound on either route.
//
// 数值上下界统一校验,越界指名报错;env 与 overlay 两路共用同一套天花板。
func BoundInt64(name string, v, min, max int64) error {
	if v < min {
		return fmt.Errorf("%s must be >= %d", name, min)
	}
	if v > max {
		return fmt.Errorf("%s must be <= %d", name, max)
	}
	return nil
}

// BoundInt is the int sibling of BoundInt64.
func BoundInt(name string, v, min, max int) error {
	if v < min {
		return fmt.Errorf("%s must be >= %d", name, min)
	}
	if v > max {
		return fmt.Errorf("%s must be <= %d", name, max)
	}
	return nil
}

// ValidatePowMode fail-fasts on an INSTALL_POW_MODE outside the closed enum so a
// typo (e.g. "enabled") can never silently land in an unintended state. Exported
// for reuse on the env-load path.
//
// 非法 mode 立即 fail-fast(指名合法集),绝不静默落入意外态。
func ValidatePowMode(m string) error {
	switch m {
	case PowModeOff, PowModeShadow, PowModeEnforce:
		return nil
	default:
		return fmt.Errorf("INSTALL_POW_MODE %q invalid: must be one of off|shadow|enforce", m)
	}
}

// validatePowSecretPresent enforces the strong-consistency invariant (reviewer
// B3): an EFFECTIVE INSTALL_POW_MODE of shadow/enforce requires a non-empty
// env-only INSTALL_POW_SECRET, so a "flip mode without a secret" can never
// activate PoW signing with an empty HMAC key (enforce 锁死 / shadow 观测失真).
// The secret is env-only (absent from the override registry); a hot-edit only
// changes mode and inherits the secret from the base config, so the correct ops
// flow is: env-config the secret + restart, THEN flip the mode. mode=off needs no
// secret (dormant) so it never trips this. Run on BOTH the semantic path and the
// per-key override apply.
//
// 强一致铁律:生效 mode∈{shadow,enforce} 必须有 secret,否则 fail-fast。热改只改
// mode、secret 从 base 继承,故正确流程 = 先 env 配 secret + 重启 → 再热改 mode。
func validatePowSecretPresent(mode string, secret []byte) error {
	if mode != PowModeShadow && mode != PowModeEnforce {
		return nil
	}
	if len(secret) == 0 {
		return fmt.Errorf(
			"CONFIG_POW_SECRET_REQUIRED: INSTALL_POW_MODE %s requires INSTALL_POW_SECRET (env-only); "+
				"set it and restart before enabling PoW", mode)
	}
	return nil
}

// ValidateSemantics enforces SEC-2 cross-field consistency on top of the
// per-field existence/positivity checks. These are config relationships whose
// violation silently defeats a guardrail (not a single bad value), so each is
// named explicitly on failure. Run on the env-load path, the startup overlay
// assembly, AND every hot override batch — the single source of cross-field truth.
//
// Cross-field rules: the operator monthly budget is positive; every active
// upstream model has a compiled immutable rate card; one request's provable
// worst-case quote fits the monthly operator budget; media cannot exceed the
// body memory envelope; effective PoW modes have a secret. Kimi-dependent checks
// are conditional on its optional credential: an intentionally text-only
// deployment must not fail startup because of an inactive media model.
func (c *Config) ValidateSemantics() error {
	if c.GlobalMonthlySpendPUSD <= 0 {
		return fmt.Errorf("SEC-2 config: GLOBAL_MONTHLY_SPEND_MICRO_USD must be > 0")
	}
	if !validPublicModelID(c.PublicModelID) {
		return fmt.Errorf("SEC-2 config: PUBLIC_MODEL_ID must be 1..%d ASCII bytes using letters, digits, '.', '_', '-', ':', or '/'", MaxPublicModelIDBytes)
	}
	textOutput := min(c.MaxTokensCap, billing.DeepSeekOutputLimit)
	// With estimator admission removed, the provable worst case is the complete
	// text-model input limit (an over-limit byte quote is clamped to this bound
	// before Reserve). Validate that one such request fits the monthly wallet.
	textPrompt := billing.DeepSeekInputLimit
	textPlan, err := billing.NewPlan(billing.ProviderDeepSeek, c.TextUpstreamModel,
		billing.InputStandard, textPrompt, textOutput)
	if err != nil {
		return fmt.Errorf("SEC-2 config: TEXT_UPSTREAM_MODEL has no exact rate card: %w", err)
	}
	if textPlan.ReservedPUSD > c.GlobalMonthlySpendPUSD {
		return fmt.Errorf("SEC-2 config: text request worst-case quote exceeds GLOBAL_MONTHLY_SPEND_MICRO_USD")
	}
	if len(c.KimiAPIKeys) > 0 {
		// Kimi compatibility does not promise a smaller thinking-token sub-cap.
		// Reserve K2.6's full input/output hard limits, then refund only from
		// authoritative usage.
		mediaPlan, err := billing.NewPlan(billing.ProviderKimi, c.MultimodalUpstreamModel,
			billing.InputStandard, billing.KimiInputLimit, billing.KimiOutputLimit)
		if err != nil {
			return fmt.Errorf("SEC-2 config: MULTIMODAL_UPSTREAM_MODEL has no exact rate card: %w", err)
		}
		if mediaPlan.ReservedPUSD > c.GlobalMonthlySpendPUSD {
			return fmt.Errorf("SEC-2 config: multimodal hard-limit quote exceeds GLOBAL_MONTHLY_SPEND_MICRO_USD")
		}
	}
	if c.MaxMediaParts < 1 || c.MaxMediaParts > MaxMediaParts {
		return fmt.Errorf("SEC-2 config: MAX_MEDIA_PARTS out of range")
	}
	if c.MaxMediaDecodedBytes < 1 || c.MaxMediaDecodedBytes > c.MaxBodyBytes {
		return fmt.Errorf("SEC-2 config: MAX_MEDIA_DECODED_BYTES must be > 0 and <= MAX_BODY_BYTES")
	}
	if err := validatePowSecretPresent(c.InstallPowMode, c.InstallPowSecret); err != nil {
		return err
	}
	return nil
}

func validPublicModelID(id string) bool {
	if len(id) == 0 || len(id) > MaxPublicModelIDBytes {
		return false
	}
	for i := range len(id) {
		b := id[i]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
			(b >= '0' && b <= '9') || strings.ContainsRune("._-:/", rune(b)) {
			continue
		}
		return false
	}
	return true
}

// WorstCaseMemoryMiB estimates peak RSS in MiB: the Go heap soft limit, plus the
// SQLite page cache charged PER CONNECTION (write pool 1 + read pool
// ReadPoolMaxConns), plus mmap counted once (both pools share the file pages).
// The multiplier is (1+ReadPoolMaxConns), NOT the ×2 the old budget assumed.
//
// cache_size 是 per-connection:写池 1 份 + 读池 N 份;mmap 两池共享只记一次。
func (c *Config) WorstCaseMemoryMiB() int64 {
	cacheMiB := int64(c.SQLiteCacheKiB) / 1024
	mmapMiB := c.SQLiteMmapBytes / (1024 * 1024)
	return int64(c.GoMemLimitMiB) + cacheMiB*int64(1+c.ReadPoolMaxConns) + mmapMiB
}

// ErrMemoryBudget is the PERF-2 fail-fast: the worst-case RSS estimate does not
// leave at least MemSafetyMarginMiB headroom under MemBudgetMiB. Returned ONLY
// when GOMEMLIMIT_MIB > 0 (a real heap soft-limit). It is a typed error so the
// caller can distinguish it; the message names the exact knobs to lower.
type ErrMemoryBudget struct {
	WorstMiB   int64
	GoMemLimit int
	CacheMiB   int64
	ReadPool   int
	MmapMiB    int64
	BudgetMiB  int
	SafetyMiB  int
	CeilingMiB int64
}

func (e *ErrMemoryBudget) Error() string {
	return fmt.Sprintf(
		"PERF-2 memory budget exceeded: worst-case RSS ≈ %dMiB (GOMEMLIMIT %d + cache %dMiB×(1+READ_POOL %d) + mmap %dMiB) "+
			"> MEM_BUDGET_MIB %d - MEM_SAFETY_MARGIN_MIB %d = %dMiB; lower SQLITE_CACHE_KIB / READ_POOL_MAX_CONNS / GOMEMLIMIT_MIB / SQLITE_MMAP_MB",
		e.WorstMiB, e.GoMemLimit, e.CacheMiB, e.ReadPool, e.MmapMiB,
		e.BudgetMiB, e.SafetyMiB, e.CeilingMiB)
}

// ValidateMemoryBudget is the PERF-2 guard: worst-case RSS must leave at least
// MemSafetyMarginMiB headroom under MemBudgetMiB.
//
// Pure three-state result (no stderr — stays I/O-free; infra logs the advisory):
//   - pass within budget                          → (false, nil)
//   - GOMEMLIMIT_MIB==0 and over budget           → (true,  nil)  ADVISORY: heap
//     unbounded so the estimate is advisory; infra should WARN-but-allow.
//   - GOMEMLIMIT_MIB>0  and over budget           → (false, *ErrMemoryBudget) fail-fast.
//
// 纯函数三态:在预算内 → (false,nil);GOMEMLIMIT=0 且超 → (true,nil) 只警告;
// GOMEMLIMIT>0 且超 → fail-fast。绝不直接写 stderr(由 infra 记录 advisory)。
func (c *Config) ValidateMemoryBudget() (advisory bool, err error) {
	worst := c.WorstCaseMemoryMiB()
	ceiling := int64(c.MemBudgetMiB) - int64(c.MemSafetyMarginMiB)
	if worst <= ceiling {
		return false, nil
	}
	if c.GoMemLimitMiB == 0 {
		// Heap unbounded → estimate is advisory only; never fail-fast.
		return true, nil
	}
	return false, &ErrMemoryBudget{
		WorstMiB:   worst,
		GoMemLimit: c.GoMemLimitMiB,
		CacheMiB:   int64(c.SQLiteCacheKiB) / 1024,
		ReadPool:   c.ReadPoolMaxConns,
		MmapMiB:    c.SQLiteMmapBytes / (1024 * 1024),
		BudgetMiB:  c.MemBudgetMiB,
		SafetyMiB:  c.MemSafetyMarginMiB,
		CeilingMiB: ceiling,
	}
}

// Clone returns a deep-enough copy. Runtime overrides mutate only scalar fields;
// the API-key and PoW-secret slices are env-only and Location is startup-only, so
// none is mutated after publication. A shallow struct copy is therefore race-safe
// once the pointer is atomically swapped.
//
// 浅拷贝即足够:热更新只改标量;机密 slice 与 Location 发布后永不修改。
func (c *Config) Clone() *Config {
	cp := *c
	return &cp
}

// ApplyOverrides is the PURE, all-or-nothing runtime-hot override path. It clones
// base, applies each override in deterministic key order via the tier descriptor
// table (rejecting unknown/secret keys and startup-hard keys BY NAME), then
// re-runs ValidateSemantics. On any error it returns (nil, err) — never a partial
// config — so a bad batch can never half-land. The infra Provider wraps this with
// persist + atomic swap; the parsing/bounds/semantics live here so env-load and
// override share one source of truth.
//
// 纯函数、全有或全无:克隆 base → 按 key 排序逐项 apply(未知/机密/startup-hard 指名
// 拒绝)→ 重跑跨字段校验。任一失败返 (nil,err),绝不返回半生效配置。
func ApplyOverrides(base Config, overrides map[string]string) (Config, error) {
	c := base.Clone()
	keys := make([]string, 0, len(overrides))
	for k := range overrides {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic order → stable multi-key error
	for _, k := range keys {
		if err := applyOne(c, k, overrides[k]); err != nil {
			return Config{}, err
		}
	}
	if err := c.ValidateSemantics(); err != nil {
		return Config{}, err
	}
	return *c, nil
}

// reqInt64 / reqInt parse a bounded integer, mirroring the env-load per-field
// validation (BOTH floor and ceiling via the shared Bound* + Max* consts) so the
// override path can never relax a guardrail OR set an OOM-defeating value.
func reqInt64(name, raw string, min, max int64) (int64, error) {
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("%s: invalid int %q", name, raw)
	}
	if err := BoundInt64(name, n, min, max); err != nil {
		return 0, err
	}
	return n, nil
}

func reqInt(name, raw string, min, max int) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s: invalid int %q", name, raw)
	}
	if err := BoundInt(name, n, min, max); err != nil {
		return 0, err
	}
	return n, nil
}

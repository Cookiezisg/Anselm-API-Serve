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
// 持久化 → 原子 swap(持久化失败不 swap)。机密(DEEPSEEK_API_KEY / DASHSCOPE_API_KEY /
// DASHBOARD_* / INSTALL_POW_SECRET)只读 env,绝不入 overlay、绝不在 Dump/Snapshot 出真值。
package configprovider

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/sunweilin/anselm/gateway/internal/domain/billing"
	"github.com/sunweilin/anselm/gateway/internal/domain/config"
	"github.com/sunweilin/anselm/gateway/internal/pkg/secureurl"
)

// Named fail-fast errors for required env-only secrets, so callers and tests can
// assert the precise cause without string matching.
var (
	ErrDeepSeekKeyRequired = errors.New("DEEPSEEK_API_KEY is required")
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

	// DeepSeek key(s) remain required because the text route is the baseline
	// capability. Qwen is optional: without a key only multimodal requests return
	// MULTIMODAL_UNAVAILABLE; text service and readiness continue normally.
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
	validateHTTPBaseURL(g, "DEEPSEEK_BASE_URL", c.DeepSeekBaseURL)
	for _, k := range strings.Split(getenv("DASHSCOPE_API_KEY"), ",") {
		if k = strings.TrimSpace(k); k != "" {
			c.QwenAPIKeys = append(c.QwenAPIKeys, k)
		}
	}
	c.QwenBaseURL = qwenBaseURL(g, getenv)

	c.PublicModelID = g.str("PUBLIC_MODEL_ID", "anselm-auto")
	c.TextUpstreamModel = g.str("TEXT_UPSTREAM_MODEL", billing.DeepSeekV4Flash)
	c.MultimodalUpstreamModel = g.str("MULTIMODAL_UPSTREAM_MODEL", billing.Qwen37Plus)
	c.ImageUpstreamModel = g.str("IMAGE_UPSTREAM_MODEL", billing.QwenImage20)
	// The image route calls the NATIVE DashScope API (multimodal-generation), which lives on a
	// different origin than the OpenAI compatible-mode QwenBaseURL — never derive one from the other.
	// 图像路由打**原生** DashScope API(multimodal-generation),与 compatible-mode 的 QwenBaseURL
	// 不同 origin——绝不互相推导。
	c.DashScopeNativeBase = g.str("DASHSCOPE_NATIVE_BASE", "https://dashscope.aliyuncs.com")

	// --- runtime-hot numeric knobs (env default ← bounded, shared ceilings) ---
	c.MonthlyQuota = g.boundedInt64("MONTHLY_QUOTA", 5000, 1, config.MaxMonthlyQuota)

	monthlySpendMicro := g.boundedInt64("GLOBAL_MONTHLY_SPEND_MICRO_USD", 420_000_000, 1, config.MaxMonthlySpendMicroUSD)
	c.GlobalMonthlySpendPUSD = monthlySpendMicro * billing.PicoUSDPerMicroUSD

	c.MaxTokensCap = g.boundedInt64("MAX_TOKENS_CAP", 4096, 1, config.MaxTokensCap)
	// Compatibility-only legacy knob. Context admission is always delegated to
	// the selected upstream; keep parsing the value so old deployments roll
	// forward without an unknown/invalid setting.
	c.InputTokenCap = g.boundedInt64("INPUT_TOKEN_CAP", 0, 0, config.MaxInputTokenCap)
	c.MaxMessages = g.boundedInt("MAX_MESSAGES", 256, 1, config.MaxMessages)
	c.MaxMessageChars = g.boundedInt("MAX_MESSAGE_CHARS", 131072, 1, config.MaxMessageChars)
	// Default mirrors domain/chat.BodyDecodeLimit (the historical 256KiB contract).
	c.MaxBodyBytes = g.boundedInt64("MAX_BODY_BYTES", 256*1024, config.MinBodyBytes, config.MaxBodyBytesCeiling)
	c.MaxMediaParts = g.boundedInt("MAX_MEDIA_PARTS", 8, 1, config.MaxMediaParts)
	defaultMediaBytes := min(int64(3*1024*1024), c.MaxBodyBytes*3/4)
	c.MaxMediaDecodedBytes = g.boundedInt64("MAX_MEDIA_DECODED_BYTES", defaultMediaBytes, 1, config.MaxMediaDecodedBytes)
	// Durable media is deliberately startup-hard: body chunks, retention and the
	// signing key form one trust boundary and must not half-change under traffic.
	// It defaults off so an existing gateway cannot accidentally become a file
	// ingress before its persistent volume and secret are installed.
	c.MediaEnabled = g.boolean("MEDIA_ENABLED", false)
	// Image generation defaults off (a capability, not a birthright); the daily cap defaults to
	// the WRK-082 P8 product number so enabling the capability alone ships the intended budget.
	// 图像生成默认关(能力非天赋);日限默认取 P8 产品数,单开能力即得预期预算。
	c.ImageEnabled = g.boolean("IMAGE_ENABLED", false)
	c.ImageDailyLimit = g.boundedInt64("IMAGE_DAILY_LIMIT", 10, 0, config.MaxImageDailyLimit)
	c.MediaUploadMaxBytes = g.boundedInt64("MEDIA_UPLOAD_MAX_BYTES", 100*1024*1024, 1, config.MaxMediaUploadBytes)
	defaultChunkBytes := min(int64(4*1024*1024), c.MaxBodyBytes)
	c.MediaChunkMaxBytes = g.boundedInt64("MEDIA_CHUNK_MAX_BYTES", defaultChunkBytes, 1, c.MaxBodyBytes)
	uploadTTLSec := g.boundedInt("MEDIA_UPLOAD_TTL_SEC", 3600, 1, 7*24*3600)
	leaseTTLSec := g.boundedInt("MEDIA_LEASE_TTL_SEC", 3600, 1, 7*24*3600)
	c.MediaUploadTTL = time.Duration(uploadTTLSec) * time.Second
	c.MediaLeaseTTL = time.Duration(leaseTTLSec) * time.Second
	c.NGlobalConcurrency = g.boundedInt("N_GLOBAL_CONCURRENCY", 8, 1, config.MaxNGlobalConcurrency)
	c.RatePerMin = g.boundedInt("RATE_PER_MIN", 0, 0, config.MaxRatePerMin)
	c.DailySublimit = g.boundedInt64("DAILY_SUBLIMIT", 0, 0, config.MaxDailySublimit)
	c.InstallPerIPHour = g.boundedInt("INSTALL_PER_IP_HOUR", 0, 0, config.MaxInstallPerIPHour)

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
	// The staging root is derived from the durable database directory unless
	// explicitly pinned. It must be assembled before semantic validation because
	// MEDIA_ENABLED makes it a required persistence boundary.
	c.DBPath = g.str("GATEWAY_DB_PATH", "anselm-gateway.db")
	defaultMediaRoot := filepath.Join(filepath.Dir(c.DBPath), "anselm-media")
	c.MediaStagingRoot = g.str("MEDIA_STAGING_ROOT", defaultMediaRoot)
	// No default: this gateway cannot guess its own public origin, and guessing wrong would hand
	// upstream providers an unreachable (or worse, someone else's) URL. Validation requires it
	// whenever MEDIA_ENABLED. 无默认值:网关猜不出自己的公开 origin,猜错等于把不可达(更糟:别人的)URL
	// 交给上游。MEDIA_ENABLED 时校验强制要求它。
	if secret := strings.TrimSpace(getenv("MEDIA_SIGNING_SECRET")); secret != "" {
		c.MediaSigningSecret = []byte(secret)
		c.MediaSigningSecretSource = "configured"
	} else {
		c.MediaSigningSecretSource = "disabled"
	}

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

	// Dashboard auth is an env-only, startup-hard trust-boundary choice. The
	// default is disabled so an unconfigured deployment never exposes a new
	// management surface. builtin keeps Go's own session/CSRF wall; external is
	// for a preceding IAP and is still enforced loopback-only by bootstrap.
	c.DashboardAuthMode = g.str("DASHBOARD_AUTH_MODE", config.DashboardAuthModeDisabled)
	if err := config.ValidateDashboardAuthMode(c.DashboardAuthMode); err != nil {
		g.failErr(err)
	}

	// These credentials remain env-only secrets for builtin mode. external and
	// disabled deliberately tolerate stale values during migration; deploy omits
	// them from the server environment unless builtin is selected.
	c.DashboardUser = g.str("DASHBOARD_USER", "")
	c.DashboardPassword = g.str("DASHBOARD_PASSWORD", "")
	if c.DashboardAuthMode == config.DashboardAuthModeBuiltin &&
		((c.DashboardUser == "") != (c.DashboardPassword == "") || c.DashboardUser == "") {
		g.fail("DASHBOARD_AUTH_MODE=builtin requires DASHBOARD_USER and DASHBOARD_PASSWORD together")
	}
	c.DashboardDevInsecureCookie = g.boolean("DASHBOARD_DEV_INSECURE_COOKIE", false)

	if g.err != nil {
		return config.Config{}, g.err
	}
	return c, nil
}

func validateHTTPBaseURL(g *envReader, name, value string) {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !secureurl.AllowsCredentialTransport(u) {
		g.fail(name + " must be an absolute HTTPS base URL without userinfo, query, or fragment (HTTP is allowed only for a literal loopback IP)")
	}
}

// qwenBaseURL resolves the Singapore workspace-compatible endpoint without
// making callers hand-concatenate a hostname. An explicit base remains useful
// for local tests and controlled regional changes, but a configured Qwen key
// may never start without one of the two safe endpoint sources.
func qwenBaseURL(g *envReader, getenv func(string) string) string {
	if raw := strings.TrimSpace(getenv("DASHSCOPE_BASE_URL")); raw != "" {
		base := strings.TrimRight(raw, "/")
		validateHTTPBaseURL(g, "DASHSCOPE_BASE_URL", base)
		return base
	}
	workspace := strings.TrimSpace(getenv("DASHSCOPE_WORKSPACE_ID"))
	if workspace == "" {
		if strings.TrimSpace(getenv("DASHSCOPE_API_KEY")) != "" {
			g.fail("DASHSCOPE_API_KEY requires DASHSCOPE_WORKSPACE_ID or DASHSCOPE_BASE_URL")
		}
		return ""
	}
	if !validWorkspaceID(workspace) {
		g.fail("DASHSCOPE_WORKSPACE_ID must be 1..128 ASCII letters, digits, '_' or '-'")
		return ""
	}
	return "https://" + workspace + ".ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1"
}

func validWorkspaceID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for i := range len(id) {
		b := id[i]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
			(b >= '0' && b <= '9') || b == '_' || b == '-' {
			continue
		}
		return false
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

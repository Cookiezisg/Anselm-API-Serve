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
	domvideo "github.com/sunweilin/anselm/gateway/internal/domain/video"
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
	// The editing sibling defaults to qwen-image-edit — a DIFFERENT model id on the same endpoint
	// (官方文档核准 H9). It is priced on the same image card, so it needs no rate card of its own.
	// 改图兄弟默认 qwen-image-edit——同一条端点上的**不同** model id(H9 官方文档核准)。它按同一张图像
	// 价格卡计价,故不需要自己的费率卡。
	c.ImageEditUpstreamModel = g.str("IMAGE_EDIT_UPSTREAM_MODEL", "qwen-image-edit")
	// The generation routes call the NATIVE DashScope API, which shares the credential's HOST and
	// differs only by PATH. The default therefore DERIVES from QwenBaseURL rather than naming a
	// region: a Singapore workspace key sent to dashscope.aliyuncs.com (Beijing) answers 401
	// "Incorrect API key provided" — the same key, 200 in one region and 401 in the other. That is
	// a real 401 observed on the desktop side of this campaign, and hardcoding a region is exactly
	// how it happened. An explicit DASHSCOPE_NATIVE_BASE still wins.
	// 生成路由打**原生** DashScope API,与凭证共享 **host**、只在 **path** 上不同。故默认值从
	// QwenBaseURL **派生**、而不是写死一个区域:一把新加坡 workspace key 打到 dashscope.aliyuncs.com
	// (北京)会答 401 "Incorrect API key provided"——同一把 key,一个区域 200、另一个区域 401。那是
	// 本战役桌面侧实测到的真 401,而写死区域正是它发生的方式。显式配的 DASHSCOPE_NATIVE_BASE 仍然优先。
	c.DashScopeNativeBase = g.str("DASHSCOPE_NATIVE_BASE", nativeBaseFrom(c.QwenBaseURL))
	c.TTSUpstreamModel = g.str("TTS_UPSTREAM_MODEL", billing.Qwen3TTSFlash)
	c.VideoUpstreamModel = g.str("VIDEO_UPSTREAM_MODEL", billing.Wan27T2V)
	// Cherry is the cross-generation default voice (WRK-082 P10: the parameter stays on the wire,
	// the desktop settings page does not expose it — one good default beats a picker nobody tunes).
	// Cherry 是跨代默认音色(P10:参数留在线缆上、桌面设置页不开——一个好默认胜过没人调的选择器)。
	c.TTSDefaultVoice = g.str("TTS_DEFAULT_VOICE", "Cherry")

	// --- runtime-hot numeric knobs (env default ← bounded, shared ceilings) ---
	// GATEWAY_MODE governs every rationing knob in this block (config.EffectiveLimits).
	// It defaults to DEBUG because this gateway meets its own operator first: a fresh
	// install exists to build the client against, and a developer blocked by their own
	// daily caps has no way to tell which of a dozen gates stopped them. The numbers
	// below are still parsed, bounded and persisted exactly as configured — debug only
	// masks what is ENFORCED — so flipping GATEWAY_MODE=production (env or dashboard)
	// arms them immediately with nothing to re-enter.
	// GATEWAY_MODE 统管本段所有配额旋钮。默认 **debug**:全新部署首先面对的是运营者自己,
	// 而被自家日限挡住的开发者根本看不出是十几道闸里的哪一道。下面的数值照常解析、限界、持久化
	// ——debug 只掩**执行**——所以切成 production(env 或后台)即刻上膛,无需重填任何值。
	c.RuntimeMode = g.str("GATEWAY_MODE", config.RuntimeModeDebug)
	if err := config.ValidateRuntimeMode(c.RuntimeMode); err != nil {
		g.failErr(err)
	}
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
	// Speech is its own switch with its own unit: 50K characters/day (P8), not 10 of anything.
	// 语音是自己的开关、自己的单位:5 万字符/天(P8),不是十个什么东西。
	c.SpeechEnabled = g.boolean("SPEECH_ENABLED", false)
	c.SpeechDailyLimit = g.boundedInt64("SPEECH_DAILY_LIMIT", 50_000, 0, config.MaxSpeechDailyLimit)
	// Video is the third switch, its own unit again: 10 CLIPS a day (WRK-082 H1, 用户拍板).
	// Not seconds — the thing a person rations is whole videos.
	// 视频是第三个开关,又是自己的单位:一天 10 **条**(H1,用户拍板)。不是秒——人配给的是**整条**片子。
	c.VideoEnabled = g.boolean("VIDEO_ENABLED", false)
	c.VideoDailyLimit = g.boundedInt64("VIDEO_DAILY_LIMIT", 10, 0, config.MaxVideoDailyLimit)
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
		// The video handle key is DERIVED from the same material, with domain
		// separation — never read from its own env var. Enabling video therefore
		// costs the operator no new secret to place, rotate and leak, while a
		// compromise of one key still cannot forge the other. It is deliberately
		// not stored anywhere: it is recomputed on every load from the secret.
		// 视频句柄密钥由**同一份**材料**域分离**派生——绝不读自己的 env。于是开视频不必让运营者
		// 再放一个要轮换、要防泄的 secret,而其中一把被攻破仍伪造不出另一把。它刻意不落任何地方:
		// 每次 load 都从 secret 重算。
		c.VideoHandleKey = domvideo.DeriveKey(c.MediaSigningSecret)
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

// nativeBaseFrom derives the native DashScope origin from the credential's own
// compatible-mode base URL. Both APIs live on the SAME host and differ only by
// path, so the region is a property of the credential — never of a constant in
// this file. Hardcoding one produced a real 401 on the desktop side of WRK-082:
// a Singapore workspace key answered 200 in Singapore and "Incorrect API key
// provided" in Beijing, with nothing in the message hinting at geography.
//
// With no credential configured the fallback is the international host, because
// the only base URL this gateway can derive on its own (the DASHSCOPE_WORKSPACE_ID
// form) is itself an ap-southeast-1 endpoint — a Beijing fallback would contradict
// the very default it sits next to.
//
// nativeBaseFrom 从凭证自己的 compatible-mode base URL 派生原生 DashScope origin。两套 API 在
// **同一个 host** 上、只在 path 上不同,故区域是**凭证的属性**——绝不是本文件里一个常量的属性。
// 写死一个区域在 WRK-082 桌面侧造成过一次真 401:一把新加坡 workspace key 在新加坡答 200、在北京答
// "Incorrect API key provided",而报文里没有任何一个字暗示这跟地理有关。
//
// 没配凭证时兜底取国际 host,因为本网关唯一能自行派生的 base URL(DASHSCOPE_WORKSPACE_ID 那一形)
// 本身就是 ap-southeast-1 端点——一个北京兜底会与它紧挨着的那个默认值自相矛盾。
func nativeBaseFrom(qwenBase string) string {
	u := strings.TrimRight(strings.TrimSpace(qwenBase), "/")
	u = strings.TrimSuffix(u, "/compatible-mode/v1")
	u = strings.TrimRight(u, "/")
	if u == "" {
		return "https://dashscope-intl.aliyuncs.com"
	}
	return u
}

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
	MaxBodyBytesCeiling   int64 = 8 * 1024 * 1024
	MinBodyBytes          int64 = 4 * 1024
	MaxNGlobalConcurrency int   = 100_000
	MaxRatePerMin         int   = 10_000_000
	MaxDailySublimit      int64 = 1_000_000_000
	MaxImageDailyLimit    int64 = 100_000
	// MaxVoiceAccountCeiling bounds the CONFIGURED account-wide voice ceiling. Generous by design:
	// it exists to catch a typo'd extra zero, not to express a real provider limit — that one is
	// undocumented, which is exactly why the default is 0 (unset). See load.go.
	// MaxVoiceAccountCeiling 界**配置里**那条账号级音色上限。**刻意从宽**:它是为了接住多打的一个零,
	// 不是为了表达某个真实的供应商上限——那个没有文档,而这正是默认值为 0(不设)的理由。见 load.go。
	MaxVoiceAccountCeiling int64 = 1_000_000
	MaxSpeechDailyLimit    int64 = 100_000_000
	MaxVideoDailyLimit     int64 = 10_000
	// MaxVoiceDailyLimit bounds the CONFIGURED per-install daily enrollment cap. Deliberately low
	// next to its siblings: every unit here is a $0.2 purchase that never expires, so a value in
	// the thousands would not be a generous allowance, it would be a typo with a bill attached.
	// MaxVoiceDailyLimit 界**配置里**那条逐 install 每日登记上限。与兄弟们相比刻意**低**:这里每一个
	// 单位都是一笔 $0.2、永不过期的购买,故一个上千的值不是慷慨的额度,是一个附带账单的笔误。
	MaxVoiceDailyLimit          int64 = 100
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
	// A staged upload is persisted to disk rather than buffered in Go memory. The
	// ceiling protects disk capacity and keeps an accidental unit typo from
	// turning the gateway into an unbounded public file sink.
	MaxMediaUploadBytes int64 = 1024 * 1024 * 1024
)

// PoW mode enum — three states, not a bool. off is the default and
// keeps the whole PoW path dormant (byte-for-byte unchanged /v1/install).
//
// 领号 PoW 三态:off(dormant)/ shadow(验但失败仍放行,灰度观测)/ enforce。
const (
	PowModeOff     = "off"
	PowModeShadow  = "shadow"
	PowModeEnforce = "enforce"
)

// Runtime mode enum (GATEWAY_MODE). production enforces every configured
// rationing knob; debug opens all of them so the operator can develop the client
// against their own gateway without being stopped by their own guardrails.
//
// The mask is DERIVED, never destructive: the configured numbers stay on the
// Config the dashboard edits and persists, so flipping back to production
// restores them byte-for-byte with nothing to re-enter (EffectiveLimits). That is
// the whole reason this is a mode and not "go set twelve knobs to zero" — the
// latter loses the production values the moment it is used.
//
// 运行模式二元枚举(GATEWAY_MODE)。production 执行全部已配置的配额闸;debug 全开,
// 让运营者开发客户端时不被自家护栏挡住。掩码是**派生**的、绝不破坏原值:配置值仍留在
// dashboard 编辑/持久化的那份 Config 上,切回 production 逐字节恢复、无需重填。这正是
// 它做成「模式」而不是「把十二个旋钮手动改成 0」的理由——后者一用就丢掉了生产值。
const (
	RuntimeModeDebug      = "debug"
	RuntimeModeProduction = "production"
)

// Config is the immutable, validated runtime configuration. Client-facing model
// identity, provider targets, and fixed-point spend wallets are explicit facts;
// there are no token-budget or model-allowlist compatibility mirrors.
//
// 全配置启动期一次性读入并校验,运行期只读。客户端逻辑模型、上游目标与定点金额
// 钱包各自显式表达,不存在旧白名单/令牌预算镜像。
type Config struct {
	// QwenAPIKeys is REQUIRED: every route this gateway serves goes to Qwen, so a
	// deployment without it can serve nothing at all. The loader fails at boot
	// rather than answering every request with an outage.
	// QwenAPIKeys 是**必需**的:本网关服务的每一条路由都去 Qwen,故没有它的部署什么也服务
	// 不了。loader 在启动时失败,好过对每一个请求都答一次故障。
	QwenAPIKeys []string // DASHSCOPE_API_KEY(逗号分隔多 key,第一个为主)
	QwenBaseURL string   // DASHSCOPE_BASE_URL 或 workspace 推导的 compatible-mode base

	// PublicModelID is the single client-facing logical model. The actual upstream
	// model is a construction/config fact; a client model string never selects a
	// provider or a price tier.
	PublicModelID           string // PUBLIC_MODEL_ID(default anselm-auto)
	MultimodalUpstreamModel string // MULTIMODAL_UPSTREAM_MODEL(exact priced Qwen id;文本与媒体同走这一个)

	// RuntimeMode selects whether the rationing knobs below are enforced
	// (production) or masked open (debug). See EffectiveLimits for the exact
	// masked set; the zero value enforces, so a hand-built Config never
	// accidentally runs unguarded.
	RuntimeMode string // GATEWAY_MODE(debug|production,默认 debug)

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

	// ---- 付费生成能力 ----
	// All four reach the NATIVE DashScope API below and pay with the Qwen credential above, but each
	// has its OWN switch: an operator may want one and not another, and they bill in different units
	// (images / characters / seconds / enrollments). None of them is on by default.
	// 四个能力都走下面这个**原生** DashScope API、都用上面那把 Qwen 凭证付钱,但每个有**自己的**开关:
	// 运营者可能只要其中一个,且它们的计费单位不同(张 / 字符 / 秒 / 个)。默认全部关闭。
	DashScopeNativeBase string // DASHSCOPE_NATIVE_BASE(native DashScope API origin,非 compatible-mode)

	// Image generation: off until the operator enables it against a priced upstream
	// model. The daily limit is the per-install image-count cap (default 10; 0 disables).
	// 图像生成:在运营者对着一个有价上游模型打开它之前一直关着。日限额是逐 install 的张数上限
	// (默认 10;0 = 不设闸)。
	ImageEnabled       bool   // IMAGE_ENABLED
	ImageUpstreamModel string // IMAGE_UPSTREAM_MODEL(exact priced DashScope image model id)
	ImageDailyLimit    int64  // IMAGE_DAILY_LIMIT(per-install per-day image count;0=off)

	// Speech synthesis is its own capability, separate from image generation. It shares the native
	// origin and the Qwen credential because DashScope has NO OpenAI-compatible TTS endpoint — the
	// native multimodal-generation path is the only one that exists (调研实证).
	// 语音合成是自己的能力,与图像生成分开。它共用原生 origin 与 Qwen 凭证,因为 DashScope **没有**
	// OpenAI 兼容的 TTS 端点——原生 multimodal-generation 是唯一存在的那条路(调研实证)。
	SpeechEnabled    bool   // SPEECH_ENABLED
	TTSUpstreamModel string // TTS_UPSTREAM_MODEL(exact priced DashScope TTS model id)
	SpeechDailyLimit int64  // SPEECH_DAILY_LIMIT(per-install per-day characters;0=off)
	TTSDefaultVoice  string // TTS_DEFAULT_VOICE(used when the request omits voice)

	// Video generation is the only ASYNC capability: submit returns a handle, the desktop polls
	// minutes later. It therefore needs one thing the others do not — VideoHandleKey, the derived
	// secret that binds a task to the install that paid for it. That key is NOT an env var: it is
	// derived (with domain separation) from media signing material the deployment already has, so
	// enabling video costs the operator no new secret and a leak of one key still cannot forge the
	// other.
	// 视频生成是唯一**异步**的能力:提交返回句柄,桌面端几分钟后轮询。故它需要一样别的能力不需要的
	// 东西——VideoHandleKey,把任务绑到**付过钱的那个 install** 上的派生密钥。这把 key **不是** env:
	// 它由本部署已有的 media 签名材料**域分离**派生,于是开视频不必新配 secret,而其中一把泄露仍伪造
	// 不出另一把。
	VideoEnabled       bool   // VIDEO_ENABLED
	VideoUpstreamModel string // VIDEO_UPSTREAM_MODEL(exact priced DashScope video model id)
	VideoDailyLimit    int64  // VIDEO_DAILY_LIMIT(per-install per-day CLIP count;0=off)
	VideoHandleKey     []byte // derived, never read from env; empty => video unavailable

	// Voice cloning rides SPEECH_ENABLED (a deployment that cannot speak has no use for a voice) and
	// has no upstream-model knob of its own: enrollment is a fixed service, not a model choice. Its
	// two numbers are DIFFERENT KINDS of thing — VoiceDailyLimit is a per-install daily FLOW gate
	// that resets; VoiceAccountCeiling is an account-wide INVENTORY ceiling that nothing resets,
	// guarding a shared resource no client can see.
	// 音色克隆搭在 SPEECH_ENABLED 上(说不了话的部署要音色没有用),且没有自己的上游模型旋钮:登记是
	// 一个固定服务、不是一次模型选择。它这两个数是**两类不同的东西**——VoiceDailyLimit 是逐 install
	// 的**日流量闸**、会重置;VoiceAccountCeiling 是账号级的**库存**上限、没有东西会重置它,守的是一份
	// 任何客户端都看不见的共享资源。
	VoiceDailyLimit     int64 // VOICE_DAILY_LIMIT(per-install per-day voice ENROLLMENT count;0=off)
	VoiceAccountCeiling int64 // VOICE_ACCOUNT_CEILING(账号级克隆音色总数;0=不强制)

	// Durable media staging is an explicit capability. Keeping it off until its
	// signing secret and persistent directory are configured prevents a partial
	// rollout from accepting bytes that no provider can later fetch.
	MediaEnabled             bool          // MEDIA_ENABLED
	MediaStagingRoot         string        // MEDIA_STAGING_ROOT
	MediaSigningSecret       []byte        // MEDIA_SIGNING_SECRET(env-only)
	MediaSigningSecretSource string        // configured/disabled; never the secret
	MediaUploadMaxBytes      int64         // MEDIA_UPLOAD_MAX_BYTES
	MediaChunkMaxBytes       int64         // MEDIA_CHUNK_MAX_BYTES(<= MAX_BODY_BYTES)
	MediaUploadTTL           time.Duration // MEDIA_UPLOAD_TTL_SEC
	MediaLeaseTTL            time.Duration // MEDIA_LEASE_TTL_SEC
	// MediaFetchDomain is the PUBLIC host an upstream may fetch a lease's bytes from — and it must
	// NOT be the `api.` host, because that is the one shape the fetcher rejects.
	//
	// The `api.` prohibition comes from a live experiment (ADR 0012): the same lease URL on
	// `api.<domain>` failed 400 three times over while Caddy's access log proved the origin never
	// received a request, whereas identical bytes, path and token on a plain host answered 200. The
	// fetcher blacklists API-shaped hosts at its own edge — invisible, uncontestable. That ADR
	// parked a dedicated media subdomain for "if some provider ever truly needs a URL". Voice
	// enrollment IS that provider: `voice-enrollment` accepts no base64 at all.
	//
	// **Scope: enrollment only.** Chat and image inputs keep inlining their bytes (ADR 0012), so
	// this host is not a general re-opening of URL relay — it exists for the one upstream that
	// cannot be served any other way.
	//
	// MediaFetchDomain 是上游可以来取 lease 字节的**公开**主机——且它**绝不能**是 `api.` 那台,因为
	// 那正是拉取器唯一会拒的形状。
	//
	// 禁 `api.` 这条来自一次线上判别实验(ADR 0012):同一个 lease URL 放在 `api.<域>` 上连续三次
	// 400,而 Caddy 访问日志证明**源站从未收到请求**;同样的字节、同样的路径、同样的 token 放在普通
	// 主机上答 200。拉取器在**它自己的边缘**把 API 形主机拉黑——不可见、不可申诉。那篇 ADR 为「将来
	// 某个 provider 确需 URL 形态」留了一个专用媒体子域的门。音色登记**就是**那个 provider:
	// `voice-enrollment` 根本不收 base64。
	//
	// **范围:只给登记用。** chat 与图像输入继续内联字节(ADR 0012),故这台主机**不是**对 URL 直通的
	// 全面重开——它只为那个别无他法的上游而存在。
	MediaFetchDomain string // MEDIA_DOMAIN(host only, no scheme; empty = enrollment unavailable)

	ResetTZ  string         // RESET_TZ
	Location *time.Location // LoadLocation(ResetTZ) 结果

	ListenAddr string // LISTEN_ADDR
	AdminAddr  string // ADMIN_ADDR(/metrics 独立 admin 端口;必须 loopback)
	// DashboardAddr is where the dashboard listens — always, and always on
	// loopback. Authentication is NOT this process's job: a preceding IAP
	// (Cloudflare Access, Tailscale, an SSH tunnel) owns the login wall, and the
	// loopback bind is what makes that delegation safe rather than a promise.
	// The bind is fail-fast, so "reachable but unauthenticated" is not a state
	// this gateway can reach.
	//
	// DashboardAddr 是后台的监听地址——**恒定挂载**,且**恒定 loopback**。鉴权**不是**本进程的
	// 活:前置的 IAP(Cloudflare Access / Tailscale / SSH 隧道)拥有那道登录墙,而 loopback 绑定
	// 正是让这份委托**成立**而不是停留在口头承诺的东西。绑定 fail-fast,故「可达但未鉴权」不是
	// 本网关到得了的状态。
	DashboardAddr string // DASHBOARD_ADDR(管理后台独立 loopback 监听)
	LogLevel      string // LOG_LEVEL(debug|info|warn|error)
	DBPath        string // GATEWAY_DB_PATH(SQLite 落盘位置)
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

// ValidateRuntimeMode closes the GATEWAY_MODE enum. A typo must never silently
// land in either posture, because BOTH directions are bad: "prod" quietly meaning
// debug leaves every gate open on a public gateway, while "dbg" quietly meaning
// production throttles a developer who has no way to tell which of a dozen knobs
// stopped them. Exported for reuse on the env-load path.
//
// GATEWAY_MODE 是封闭枚举,拼错立即 fail-fast:两个方向都危险——"prod" 静默当成 debug =
// 公网网关门户大开;"dbg" 静默当成 production = 开发者被自家闸挡住却不知道是哪一道。
func ValidateRuntimeMode(m string) error {
	switch m {
	case RuntimeModeDebug, RuntimeModeProduction:
		return nil
	default:
		return fmt.Errorf("GATEWAY_MODE %q invalid: must be one of debug|production", m)
	}
}

// DebugMode reports whether the rationing mask is active. It is true ONLY for the
// exact literal "debug": every other value — including the zero value of a Config
// built in a test — enforces the configured limits. The asymmetry is deliberate;
// the failure direction of an unset mode must be "guardrails on".
//
// 只有恰好等于 "debug" 才开掩码;其余值(含零值 Config)一律执行限额。这个不对称是故意的:
// 模式没设时的失败方向必须是「护栏在」。
func (c *Config) DebugMode() bool { return c.RuntimeMode == RuntimeModeDebug }

// EffectiveLimits returns the config the ENFORCEMENT path must read: c unchanged
// in production, or a copy with every rationing knob opened in debug. It is pure
// and non-destructive — the receiver keeps the operator's configured numbers, and
// only the returned copy is masked, so the dashboard still edits (and settings
// still persists) real production values while debug is live.
//
// Masked = everything that RATIONS a user: the money gates (per-install monthly
// request entitlement, the operator monthly pUSD wallet), the throughput gates
// (rate bucket, daily request sublimit, anomaly auto-throttle), the per-category
// daily caps (image/speech/video), and the install-issuance gates (per-IP, global
// daily, per-fingerprint daily + cooldown, PoW).
//
// NOT masked = everything that protects the PROCESS rather than rationing a user:
// body/message/media shape caps, MAX_TOKENS_CAP, N_GLOBAL_CONCURRENCY, queue wait,
// disk floors and the memory budget. Debug mode must not be a way to OOM the box,
// and lifting those would change what the gateway can survive, not what it allows.
// Accounting itself is untouched in both modes: reserve/settle still run and
// spend_ledger still records every pUSD, so debug is "never denied", never
// "never counted" (GW-INV-01/06 hold verbatim).
//
// 掩码集 = 一切**配给用户**的闸:钱(月请求额度、operator 月钱包)、吞吐(令牌桶、日次数子限、
// 异常降速)、品类日闸(图/语音/视频)、领号闸(per-IP、全局日、per-fp 日 + 冷却、PoW)。
// **不**掩码 = 一切保护**进程**而非配给用户的东西:body/message/media 形状上限、MAX_TOKENS_CAP、
// N_GLOBAL_CONCURRENCY、排队窗口、磁盘下限、内存预算——debug 不该是把机器 OOM 掉的路子。
// 记账两种模式下都不变:reserve/settle 照跑、spend_ledger 照记每一 pUSD,所以 debug 是
// 「永不拒绝」,不是「永不记账」(GW-INV-01/06 逐字成立)。
func (c Config) EffectiveLimits() Config {
	if !c.DebugMode() {
		return c
	}
	m := c
	// Money: raised to the registry ceilings rather than to a sentinel, so every
	// gate keeps its ordinary "reserve against a limit" shape and no store needs a
	// second, untested "unlimited" code path.
	m.MonthlyQuota = MaxMonthlyQuota
	m.GlobalMonthlySpendPUSD = MaxMonthlySpendMicroUSD * billing.PicoUSDPerMicroUSD
	// Throughput + category + install gates: 0 is each one's documented "disabled".
	m.RatePerMin = 0
	m.DailySublimit = 0
	m.TokenAnomalyRPM = 0
	m.ImageDailyLimit = 0
	m.SpeechDailyLimit = 0
	m.VideoDailyLimit = 0
	m.VoiceDailyLimit = 0
	m.InstallPerIPHour = 0
	m.InstallGlobalDailyCap = 0
	m.InstallPerFPDaily = 0
	m.InstallPerFPCooldownSec = 0
	m.InstallPowMode = PowModeOff
	return m
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
// body memory envelope; effective PoW modes have a secret. Qwen-dependent checks
// are conditional on its optional credential: an intentionally text-only
// deployment must not fail startup because of an inactive media model.
func (c *Config) ValidateSemantics() error {
	if c.GlobalMonthlySpendPUSD <= 0 {
		return fmt.Errorf("SEC-2 config: GLOBAL_MONTHLY_SPEND_MICRO_USD must be > 0")
	}
	if !validPublicModelID(c.PublicModelID) {
		return fmt.Errorf("SEC-2 config: PUBLIC_MODEL_ID must be 1..%d ASCII bytes using letters, digits, '.', '_', '-', ':', or '/'", MaxPublicModelIDBytes)
	}
	// ONE route, ONE check. Text and media both go to MULTIMODAL_UPSTREAM_MODEL,
	// so validating a separate "text model" would price a model no request can
	// reach — and the old code did exactly that UNCONDITIONALLY while gating the
	// live route's check behind an optional credential. That was backwards.
	//
	// The bound is the model's FULL input/output limit rather than an estimate:
	// inline media bytes cannot prove visual tokenization, and the byte estimate
	// was never admission truth. Reserve the whole top-tier quote and settle only
	// from authoritative usage — this is an accounting guard, never context
	// admission (ADR-0016).
	//
	// **一条路由,一次校验。** 文本与媒体都去 MULTIMODAL_UPSTREAM_MODEL,故再验一次「文本
	// 模型」等于给一个没有请求到得了的模型定价——而旧代码正是**无条件**做那件事,却把**活着
	// 那条**路由的校验藏在一个可选凭证后面。方向正好反了。
	//
	// 界取模型的**完整**输入/输出上限、不是估算:内联媒体字节证明不了视觉 token 化,而字节估算
	// 从来不是准入依据。按顶档全额预留、只按权威 usage 结算——这是**账务**闸,不是上下文准入。
	plan, err := billing.NewPlan(billing.ProviderQwen, c.MultimodalUpstreamModel,
		billing.InputStandard, billing.Qwen37InputLimit, billing.Qwen37OutputLimit)
	if err != nil {
		return fmt.Errorf("SEC-2 config: MULTIMODAL_UPSTREAM_MODEL has no exact rate card: %w", err)
	}
	if plan.ReservedPUSD > c.GlobalMonthlySpendPUSD {
		return fmt.Errorf("SEC-2 config: worst-case request quote exceeds GLOBAL_MONTHLY_SPEND_MICRO_USD")
	}
	if c.MaxMediaParts < 1 || c.MaxMediaParts > MaxMediaParts {
		return fmt.Errorf("SEC-2 config: MAX_MEDIA_PARTS out of range")
	}
	if c.MaxMediaDecodedBytes < 1 || c.MaxMediaDecodedBytes > c.MaxBodyBytes {
		return fmt.Errorf("SEC-2 config: MAX_MEDIA_DECODED_BYTES must be > 0 and <= MAX_BODY_BYTES")
	}
	if c.ImageEnabled {
		// Image generation is a fail-fast capability: enabling it without a Qwen credential, a
		// priced model, or a native-API origin would produce a route that can only 503 at runtime.
		// 图像生成是 fail-fast 能力:开着却没 Qwen key、没定价模型、没原生 API origin,等于造一条
		// 只能在运行时 503 的路由。
		if len(c.QwenAPIKeys) == 0 {
			return fmt.Errorf("SEC-2 config: IMAGE_ENABLED requires DASHSCOPE_API_KEY")
		}
		if _, err := billing.NewUnitPlan(billing.ProviderQwen, c.ImageUpstreamModel, billing.InputImages, 1); err != nil {
			return fmt.Errorf("SEC-2 config: IMAGE_UPSTREAM_MODEL has no exact rate card: %w", err)
		}
		if strings.TrimSpace(c.DashScopeNativeBase) == "" {
			return fmt.Errorf("SEC-2 config: IMAGE_ENABLED requires DASHSCOPE_NATIVE_BASE")
		}
	}
	if c.SpeechEnabled {
		// Same fail-fast reasoning as images, one capability over. 与图像同款 fail-fast。
		if len(c.QwenAPIKeys) == 0 {
			return fmt.Errorf("SEC-2 config: SPEECH_ENABLED requires DASHSCOPE_API_KEY")
		}
		if _, err := billing.NewUnitPlan(billing.ProviderQwen, c.TTSUpstreamModel, billing.InputCharacters, 1); err != nil {
			return fmt.Errorf("SEC-2 config: TTS_UPSTREAM_MODEL has no exact rate card: %w", err)
		}
		if strings.TrimSpace(c.DashScopeNativeBase) == "" {
			return fmt.Errorf("SEC-2 config: SPEECH_ENABLED requires DASHSCOPE_NATIVE_BASE")
		}
		if strings.TrimSpace(c.TTSDefaultVoice) == "" {
			return fmt.Errorf("SEC-2 config: SPEECH_ENABLED requires TTS_DEFAULT_VOICE")
		}
	}
	if d := strings.TrimSpace(c.MediaFetchDomain); d != "" {
		// A bare host, never a URL: the gateway builds the scheme and path itself, so accepting a
		// URL here would let a typo silently point the upstream at somebody else's server.
		// **裸主机、绝不是 URL**:scheme 与路径由网关自己拼,故在这里收 URL 等于让一个笔误把上游
		// 静默地指向别人的服务器。
		if strings.Contains(d, "://") || strings.ContainsAny(d, "/?#") {
			return fmt.Errorf("CONFIG_MEDIA_DOMAIN_INVALID: MEDIA_DOMAIN must be a bare host, not a URL")
		}
		// **Refuse an `api.` host outright.** This is not stylistic. ADR 0012's production
		// experiment proved the upstream fetcher rejects API-shaped hosts at its own edge: the
		// same lease URL on `api.<domain>` failed three times over while the origin's access log
		// showed no request ever arrived, and identical bytes on a plain host answered 200. A
		// misconfiguration here would therefore fail INVISIBLY — no origin log, no upstream detail,
		// just enrollments that never work. Fail at boot instead.
		// **直接拒绝 `api.` 主机。** 这不是风格问题。ADR 0012 的生产实验证明拉取器在**它自己的边缘**
		// 拒绝 API 形主机:同一个 lease URL 放在 `api.<域>` 上连续三次失败,而源站访问日志显示**请求
		// 从未到达**;同样的字节放普通主机上答 200。故这里配错会**无形地**失败——没有源站日志、没有
		// 上游细节,只有永远不成功的登记。所以在启动时就失败。
		if strings.HasPrefix(d, "api.") {
			return fmt.Errorf("CONFIG_MEDIA_DOMAIN_INVALID: MEDIA_DOMAIN must not be an `api.` host — " +
				"the upstream fetcher blacklists that shape at its edge (ADR 0018/0012)")
		}
	}
	if c.VideoEnabled {
		// Same fail-fast reasoning as images and speech, plus one more half: without
		// handle-signing material a submission could never be polled, so the route
		// would spend a user's daily allowance on a video they can never reach.
		// 与图像、语音同款 fail-fast,外加一半:没有句柄签名材料,提交出去的任务永远轮询不到,
		// 那条路由会花掉用户当天的额度去换一条他拿不到的片子。
		if len(c.QwenAPIKeys) == 0 {
			return fmt.Errorf("SEC-2 config: VIDEO_ENABLED requires DASHSCOPE_API_KEY")
		}
		if _, err := billing.NewUnitPlan(billing.ProviderQwen, c.VideoUpstreamModel, billing.InputVideoSeconds, 1); err != nil {
			return fmt.Errorf("SEC-2 config: VIDEO_UPSTREAM_MODEL has no exact rate card: %w", err)
		}
		if strings.TrimSpace(c.DashScopeNativeBase) == "" {
			return fmt.Errorf("SEC-2 config: VIDEO_ENABLED requires DASHSCOPE_NATIVE_BASE")
		}
		if len(c.VideoHandleKey) == 0 {
			return fmt.Errorf("CONFIG_VIDEO_HANDLE_KEY_REQUIRED: VIDEO_ENABLED requires MEDIA_SIGNING_SECRET (the video handle key is derived from it)")
		}
	}
	if c.MediaEnabled {
		if strings.TrimSpace(c.MediaStagingRoot) == "" {
			return fmt.Errorf("SEC-2 config: MEDIA_ENABLED requires MEDIA_STAGING_ROOT")
		}
		if len(c.MediaSigningSecret) < 32 {
			return fmt.Errorf("CONFIG_MEDIA_SIGNING_SECRET_REQUIRED: MEDIA_ENABLED requires MEDIA_SIGNING_SECRET with at least 32 bytes (env-only)")
		}
		if c.MediaUploadMaxBytes <= 0 || c.MediaUploadMaxBytes > MaxMediaUploadBytes {
			return fmt.Errorf("SEC-2 config: MEDIA_UPLOAD_MAX_BYTES out of range")
		}
		if c.MediaChunkMaxBytes <= 0 || c.MediaChunkMaxBytes > c.MaxBodyBytes || c.MediaChunkMaxBytes > c.MediaUploadMaxBytes {
			return fmt.Errorf("SEC-2 config: MEDIA_CHUNK_MAX_BYTES must be > 0 and <= both MAX_BODY_BYTES and MEDIA_UPLOAD_MAX_BYTES")
		}
		if c.MediaUploadTTL <= 0 || c.MediaLeaseTTL <= 0 {
			return fmt.Errorf("SEC-2 config: media TTLs must be positive")
		}
	}
	if err := validatePowSecretPresent(c.InstallPowMode, c.InstallPowSecret); err != nil {
		return err
	}
	// An empty RuntimeMode is the enforcing zero value (see DebugMode) and stays
	// legal so a hand-built Config need not name a mode; a NON-empty typo is not,
	// because it is someone trying to select a posture and missing.
	// 空值是「执行限额」的零值,合法;非空拼错则不合法——那是有人想选一个姿态却选错了。
	if c.RuntimeMode != "" {
		if err := ValidateRuntimeMode(c.RuntimeMode); err != nil {
			return err
		}
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
// - pass within budget → (false, nil)
// - GOMEMLIMIT_MIB==0 and over budget → (true, nil) ADVISORY: heap
// unbounded so the estimate is advisory; infra should WARN-but-allow.
// - GOMEMLIMIT_MIB>0 and over budget → (false, *ErrMemoryBudget) fail-fast.
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

// Tier descriptor table — the single registry mapping every dashboard-surfaced
// config key to its tier, bounds, apply/get accessors, and RestartRequired flag.
// One table drives ALL of: the override apply path (applyOne), the dashboard Dump
// (read-only surface + client pre-validation hints), and the kind-based rejection
// of writes to secret/startup-hard keys. Secrets are NOT in this table at all
// (env-only; never persisted, never dumped).
package config

import (
	"strconv"
	"strings"
	"time"
)

// Tier classifies a config key for the dashboard surface and the override path.
type Tier int

const (
	// TierRuntimeHot: dashboard-editable, persisted to settings, hot-reloaded.
	TierRuntimeHot Tier = iota
	// TierStartupHard: startup hard-constraint (incl. the PERF-2 memory-budget
	// self-check inputs); dashboard read-only, env-only, change requires restart.
	// Applying one is REJECTED so a budget knob can never be hot-swapped past the
	// worst-case RSS guard.
	TierStartupHard
)

// Spec describes one settable key: how to apply a string onto a Config (apply)
// and render the current value (get), its tier, and — for bounded runtime
// numbers — the SAME inclusive [Min,Max] apply enforces (surfaced so the client
// can pre-validate before a round-trip). Bounded marks a numeric spec (logical
// model ids are strings with no numeric range; startup-hard specs are read-only).
// RestartRequired is true for startup-hard keys AND for
// N_GLOBAL_CONCURRENCY (persisted/validated hot, but the semaphore capacity only
// re-takes effect on restart).
type Spec struct {
	Key             string
	Tier            Tier
	RestartRequired bool
	Bounded         bool
	Min, Max        int64
	apply           func(c *Config, raw string) error // nil for startup-hard (never applied)
	get             func(c *Config) string
}

// Specs is the registry of every dashboard-surfaced config item, returned in a
// stable order. Runtime ones (TierRuntimeHot) are exactly the hot-editable set;
// startup-hard ones are surfaced read-only (incl. the memory-budget inputs which
// must NEVER hot-reload). Secrets (DEEPSEEK_API_KEY, DASHSCOPE_API_KEY, DASHBOARD_*, INSTALL_POW_SECRET)
// are deliberately absent — env-only, never persisted, never dumped.
func Specs() []Spec {
	return []Spec{
		{Key: "PUBLIC_MODEL_ID", Tier: TierRuntimeHot,
			apply: func(c *Config, raw string) error {
				id := strings.TrimSpace(raw)
				if id == "" {
					return errMsg("PUBLIC_MODEL_ID must not be empty")
				}
				c.PublicModelID = id
				return nil
			},
			get: func(c *Config) string { return c.PublicModelID }},
		{Key: "GLOBAL_MONTHLY_SPEND_MICRO_USD", Tier: TierRuntimeHot, Bounded: true, Min: 1, Max: MaxMonthlySpendMicroUSD,
			apply: func(c *Config, raw string) error {
				n, err := reqInt64("GLOBAL_MONTHLY_SPEND_MICRO_USD", raw, 1, MaxMonthlySpendMicroUSD)
				if err == nil {
					c.GlobalMonthlySpendPUSD = n * 1_000_000
				}
				return err
			},
			get: func(c *Config) string { return strconv.FormatInt(c.GlobalMonthlySpendPUSD/1_000_000, 10) }},
		{Key: "MONTHLY_QUOTA", Tier: TierRuntimeHot, Bounded: true, Min: 1, Max: MaxMonthlyQuota,
			apply: func(c *Config, raw string) error {
				n, err := reqInt64("MONTHLY_QUOTA", raw, 1, MaxMonthlyQuota)
				if err == nil {
					c.MonthlyQuota = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.FormatInt(c.MonthlyQuota, 10) }},
		{Key: "MAX_TOKENS_CAP", Tier: TierRuntimeHot, Bounded: true, Min: 1, Max: MaxTokensCap,
			apply: func(c *Config, raw string) error {
				n, err := reqInt64("MAX_TOKENS_CAP", raw, 1, MaxTokensCap)
				if err == nil {
					c.MaxTokensCap = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.FormatInt(c.MaxTokensCap, 10) }},
		// INPUT_TOKEN_CAP is retained only so an existing dashboard/env overlay can
		// roll through this release without becoming invalid. It has no admission
		// effect: byte estimates are accounting bounds, while the upstream model is
		// the context authority.
		{Key: "INPUT_TOKEN_CAP", Tier: TierRuntimeHot, Bounded: true, Min: 0, Max: MaxInputTokenCap,
			apply: func(c *Config, raw string) error {
				n, err := reqInt64("INPUT_TOKEN_CAP", raw, 0, MaxInputTokenCap)
				if err == nil {
					c.InputTokenCap = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.FormatInt(c.InputTokenCap, 10) }},
		{Key: "MAX_MESSAGES", Tier: TierRuntimeHot, Bounded: true, Min: 1, Max: int64(MaxMessages),
			apply: func(c *Config, raw string) error {
				n, err := reqInt("MAX_MESSAGES", raw, 1, MaxMessages)
				if err == nil {
					c.MaxMessages = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.Itoa(c.MaxMessages) }},
		{Key: "MAX_MESSAGE_CHARS", Tier: TierRuntimeHot, Bounded: true, Min: 1, Max: int64(MaxMessageChars),
			apply: func(c *Config, raw string) error {
				n, err := reqInt("MAX_MESSAGE_CHARS", raw, 1, MaxMessageChars)
				if err == nil {
					c.MaxMessageChars = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.Itoa(c.MaxMessageChars) }},
		{Key: "MAX_MEDIA_PARTS", Tier: TierRuntimeHot, Bounded: true, Min: 1, Max: int64(MaxMediaParts),
			apply: func(c *Config, raw string) error {
				n, err := reqInt("MAX_MEDIA_PARTS", raw, 1, MaxMediaParts)
				if err == nil {
					c.MaxMediaParts = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.Itoa(c.MaxMediaParts) }},
		{Key: "MAX_MEDIA_DECODED_BYTES", Tier: TierRuntimeHot, Bounded: true, Min: 1, Max: MaxMediaDecodedBytes,
			apply: func(c *Config, raw string) error {
				n, err := reqInt64("MAX_MEDIA_DECODED_BYTES", raw, 1, MaxMediaDecodedBytes)
				if err == nil {
					c.MaxMediaDecodedBytes = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.FormatInt(c.MaxMediaDecodedBytes, 10) }},
		// MAX_BODY_BYTES is the request-body byte cap (memory protection, §5.3): the
		// business middleware chain's MaxBytesReader bound. Like N_GLOBAL_CONCURRENCY
		// it is persisted/validated hot but the chain is assembled once at boot, so
		// it is flagged RestartRequired. Floor 4KiB (install/dashboard bodies must
		// fit); ceiling 8MiB (bodies buffer ~3.5× each — the PERF-2 box bound).
		{Key: "MAX_BODY_BYTES", Tier: TierRuntimeHot, RestartRequired: true, Bounded: true, Min: MinBodyBytes, Max: MaxBodyBytesCeiling,
			apply: func(c *Config, raw string) error {
				n, err := reqInt64("MAX_BODY_BYTES", raw, MinBodyBytes, MaxBodyBytesCeiling)
				if err == nil {
					c.MaxBodyBytes = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.FormatInt(c.MaxBodyBytes, 10) }},
		// N_GLOBAL_CONCURRENCY is runtime-hot (editable/persisted/validated) yet the
		// semaphore capacity is fixed at construction — the override only takes effect
		// for new requests after a restart, so it is flagged RestartRequired.
		{Key: "N_GLOBAL_CONCURRENCY", Tier: TierRuntimeHot, RestartRequired: true, Bounded: true, Min: 1, Max: int64(MaxNGlobalConcurrency),
			apply: func(c *Config, raw string) error {
				n, err := reqInt("N_GLOBAL_CONCURRENCY", raw, 1, MaxNGlobalConcurrency)
				if err == nil {
					c.NGlobalConcurrency = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.Itoa(c.NGlobalConcurrency) }},
		{Key: "RATE_PER_MIN", Tier: TierRuntimeHot, Bounded: true, Min: 0, Max: int64(MaxRatePerMin),
			apply: func(c *Config, raw string) error {
				n, err := reqInt("RATE_PER_MIN", raw, 0, MaxRatePerMin)
				if err == nil {
					c.RatePerMin = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.Itoa(c.RatePerMin) }},
		{Key: "DAILY_SUBLIMIT", Tier: TierRuntimeHot, Bounded: true, Min: 0, Max: MaxDailySublimit,
			apply: func(c *Config, raw string) error {
				n, err := reqInt64("DAILY_SUBLIMIT", raw, 0, MaxDailySublimit)
				if err == nil {
					c.DailySublimit = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.FormatInt(c.DailySublimit, 10) }},
		{Key: "IMAGE_DAILY_LIMIT", Tier: TierRuntimeHot, Bounded: true, Min: 0, Max: MaxImageDailyLimit,
			apply: func(c *Config, raw string) error {
				n, err := reqInt64("IMAGE_DAILY_LIMIT", raw, 0, MaxImageDailyLimit)
				if err == nil {
					c.ImageDailyLimit = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.FormatInt(c.ImageDailyLimit, 10) }},
		{Key: "INSTALL_PER_IP_HOUR", Tier: TierRuntimeHot, Bounded: true, Min: 0, Max: int64(MaxInstallPerIPHour),
			apply: func(c *Config, raw string) error {
				n, err := reqInt("INSTALL_PER_IP_HOUR", raw, 0, MaxInstallPerIPHour)
				if err == nil {
					c.InstallPerIPHour = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.Itoa(c.InstallPerIPHour) }},
		// M2 防 Sybil 领号闸(均 0=禁用;运行时可改、后台可在线调阀)。
		{Key: "INSTALL_GLOBAL_DAILY_CAP", Tier: TierRuntimeHot, Bounded: true, Min: 0, Max: MaxInstallGlobalDailyCap,
			apply: func(c *Config, raw string) error {
				n, err := reqInt64("INSTALL_GLOBAL_DAILY_CAP", raw, 0, MaxInstallGlobalDailyCap)
				if err == nil {
					c.InstallGlobalDailyCap = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.FormatInt(c.InstallGlobalDailyCap, 10) }},
		{Key: "INSTALL_PER_FP_DAILY", Tier: TierRuntimeHot, Bounded: true, Min: 0, Max: MaxInstallPerFPDaily,
			apply: func(c *Config, raw string) error {
				n, err := reqInt64("INSTALL_PER_FP_DAILY", raw, 0, MaxInstallPerFPDaily)
				if err == nil {
					c.InstallPerFPDaily = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.FormatInt(c.InstallPerFPDaily, 10) }},
		{Key: "INSTALL_PER_FP_COOLDOWN_SEC", Tier: TierRuntimeHot, Bounded: true, Min: 0, Max: int64(MaxInstallPerFPCooldownSec),
			apply: func(c *Config, raw string) error {
				n, err := reqInt("INSTALL_PER_FP_COOLDOWN_SEC", raw, 0, MaxInstallPerFPCooldownSec)
				if err == nil {
					c.InstallPerFPCooldownSec = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.Itoa(c.InstallPerFPCooldownSec) }},
		// M2 批次3 领号 PoW。MODE 走 string enum;DIFFICULTY 有界 int。
		// 🔴 INSTALL_POW_SECRET 是 env-only 机密,故意不在本 registry。
		{Key: "INSTALL_POW_MODE", Tier: TierRuntimeHot,
			apply: func(c *Config, raw string) error {
				m := strings.TrimSpace(raw)
				if err := ValidatePowMode(m); err != nil {
					return err
				}
				// 强一致:热改 mode→shadow/enforce 时 secret 来自 base 克隆;没配 secret
				// 就切非 off,在此 fail-fast(与 ValidateSemantics 同一校验,两路对齐)。
				if err := validatePowSecretPresent(m, c.InstallPowSecret); err != nil {
					return err
				}
				c.InstallPowMode = m
				return nil
			},
			get: func(c *Config) string { return c.InstallPowMode }},
		{Key: "INSTALL_POW_DIFFICULTY", Tier: TierRuntimeHot, Bounded: true, Min: 1, Max: int64(MaxInstallPowDifficulty),
			apply: func(c *Config, raw string) error {
				n, err := reqInt("INSTALL_POW_DIFFICULTY", raw, 1, MaxInstallPowDifficulty)
				if err == nil {
					c.InstallPowDifficulty = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.Itoa(c.InstallPowDifficulty) }},
		// M2 批次2 per-token 异常自动降速(ANOMALY_RPM=0=禁用)。
		{Key: "TOKEN_ANOMALY_RPM", Tier: TierRuntimeHot, Bounded: true, Min: 0, Max: int64(MaxTokenAnomalyRPM),
			apply: func(c *Config, raw string) error {
				n, err := reqInt("TOKEN_ANOMALY_RPM", raw, 0, MaxTokenAnomalyRPM)
				if err == nil {
					c.TokenAnomalyRPM = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.Itoa(c.TokenAnomalyRPM) }},
		{Key: "TOKEN_THROTTLE_FACTOR", Tier: TierRuntimeHot, Bounded: true, Min: 1, Max: int64(MaxTokenThrottleFactor),
			apply: func(c *Config, raw string) error {
				n, err := reqInt("TOKEN_THROTTLE_FACTOR", raw, 1, MaxTokenThrottleFactor)
				if err == nil {
					c.TokenThrottleFactor = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.Itoa(c.TokenThrottleFactor) }},
		{Key: "TOKEN_THROTTLE_COOLDOWN_SEC", Tier: TierRuntimeHot, Bounded: true, Min: 1, Max: int64(MaxTokenThrottleCooldownSec),
			apply: func(c *Config, raw string) error {
				n, err := reqInt("TOKEN_THROTTLE_COOLDOWN_SEC", raw, 1, MaxTokenThrottleCooldownSec)
				if err == nil {
					c.TokenThrottleCooldownSec = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.Itoa(c.TokenThrottleCooldownSec) }},
		{Key: "QUEUE_WAIT_MS", Tier: TierRuntimeHot, Bounded: true, Min: 0, Max: int64(MaxQueueWaitMS),
			apply: func(c *Config, raw string) error {
				n, err := reqInt("QUEUE_WAIT_MS", raw, 0, MaxQueueWaitMS)
				if err == nil {
					c.QueueWait = time.Duration(n) * time.Millisecond
				}
				return err
			},
			get: func(c *Config) string { return strconv.FormatInt(c.QueueWait.Milliseconds(), 10) }},
		{Key: "UPSTREAM_HEADER_TIMEOUT_SEC", Tier: TierRuntimeHot, Bounded: true, Min: 1, Max: int64(MaxUpstreamHeaderTimeoutSec),
			apply: func(c *Config, raw string) error {
				n, err := reqInt("UPSTREAM_HEADER_TIMEOUT_SEC", raw, 1, MaxUpstreamHeaderTimeoutSec)
				if err == nil {
					c.UpstreamHeaderTimeout = time.Duration(n) * time.Second
				}
				return err
			},
			get: func(c *Config) string { return strconv.FormatInt(int64(c.UpstreamHeaderTimeout.Seconds()), 10) }},
		{Key: "DISK_MIN_MB", Tier: TierRuntimeHot, Bounded: true, Min: 0, Max: int64(MaxDiskMinMB),
			apply: func(c *Config, raw string) error {
				n, err := reqInt("DISK_MIN_MB", raw, 0, MaxDiskMinMB)
				if err == nil {
					c.DiskMinMB = n
				}
				return err
			},
			get: func(c *Config) string { return strconv.Itoa(c.DiskMinMB) }},
		{Key: "DISK_MIN_PERCENT", Tier: TierRuntimeHot, Bounded: true, Min: 0, Max: 100,
			apply: func(c *Config, raw string) error {
				n, err := reqInt("DISK_MIN_PERCENT", raw, 0, 100)
				if err != nil {
					return err
				}
				c.DiskMinPercent = n
				return nil
			},
			get: func(c *Config) string { return strconv.Itoa(c.DiskMinPercent) }},

		// Startup hard-constraints (dashboard read-only; memory-budget inputs must
		// never hot-reload — they gate the PERF-2 worst-case RSS self-check).
		{Key: "TEXT_UPSTREAM_MODEL", Tier: TierStartupHard, RestartRequired: true, get: func(c *Config) string { return c.TextUpstreamModel }},
		{Key: "MULTIMODAL_UPSTREAM_MODEL", Tier: TierStartupHard, RestartRequired: true, get: func(c *Config) string { return c.MultimodalUpstreamModel }},
		{Key: "IMAGE_ENABLED", Tier: TierStartupHard, RestartRequired: true, get: func(c *Config) string { return strconv.FormatBool(c.ImageEnabled) }},
		{Key: "IMAGE_UPSTREAM_MODEL", Tier: TierStartupHard, RestartRequired: true, get: func(c *Config) string { return c.ImageUpstreamModel }},
		{Key: "DASHSCOPE_NATIVE_BASE", Tier: TierStartupHard, RestartRequired: true, get: func(c *Config) string { return c.DashScopeNativeBase }},
		{Key: "DEEPSEEK_BASE_URL", Tier: TierStartupHard, RestartRequired: true, get: func(c *Config) string { return c.DeepSeekBaseURL }},
		{Key: "DASHSCOPE_BASE_URL", Tier: TierStartupHard, RestartRequired: true, get: func(c *Config) string { return c.QwenBaseURL }},
		{Key: "GOMEMLIMIT_MIB", Tier: TierStartupHard, RestartRequired: true, get: func(c *Config) string { return strconv.Itoa(c.GoMemLimitMiB) }},
		{Key: "SQLITE_CACHE_KIB", Tier: TierStartupHard, RestartRequired: true, get: func(c *Config) string { return strconv.Itoa(c.SQLiteCacheKiB) }},
		{Key: "READ_POOL_MAX_CONNS", Tier: TierStartupHard, RestartRequired: true, get: func(c *Config) string { return strconv.Itoa(c.ReadPoolMaxConns) }},
		{Key: "SQLITE_MMAP_MB", Tier: TierStartupHard, RestartRequired: true, get: func(c *Config) string { return strconv.FormatInt(c.SQLiteMmapBytes/(1024*1024), 10) }},
		{Key: "ADMIN_ADDR", Tier: TierStartupHard, RestartRequired: true, get: func(c *Config) string { return c.AdminAddr }},
		{Key: "DASHBOARD_ADDR", Tier: TierStartupHard, RestartRequired: true, get: func(c *Config) string { return c.DashboardAddr }},
		{Key: "LISTEN_ADDR", Tier: TierStartupHard, RestartRequired: true, get: func(c *Config) string { return c.ListenAddr }},
		{Key: "RESET_TZ", Tier: TierStartupHard, RestartRequired: true, get: func(c *Config) string { return c.ResetTZ }},
		{Key: "GATEWAY_DB_PATH", Tier: TierStartupHard, RestartRequired: true, get: func(c *Config) string { return c.DBPath }},
	}
}

// specByKey indexes Specs for O(1) lookup.
func specByKey() map[string]Spec {
	m := make(map[string]Spec, len(Specs()))
	for _, s := range Specs() {
		m[s.Key] = s
	}
	return m
}

// applyOne applies one raw override onto c, enforcing the tier: secrets are not in
// the registry at all (unknown key); startup-hard is rejected BY NAME. Returns the
// classified error so ApplyOverrides surfaces a precise reason.
func applyOne(c *Config, key, raw string) error {
	spec, ok := specByKey()[key]
	if !ok {
		// Unknown key OR a secret (secrets are deliberately absent) — never apply.
		return errMsg(key + ": not a runtime-overridable setting (unknown or secret; secrets are env-only)")
	}
	if spec.Tier != TierRuntimeHot {
		return errMsg(key + ": startup hard-constraint — change requires a restart, not hot-reload")
	}
	return spec.apply(c, raw)
}

// DumpItem is one row of the dashboard config surface: the live value plus the
// client-side hints (Editable, RestartRequired, and inclusive Min/Max for bounded
// runtime numbers = the same server bounds, so the client can pre-validate).
// Secrets never appear (absent from Specs).
type DumpItem struct {
	Key             string
	Value           string
	Tier            Tier
	Editable        bool
	RestartRequired bool
	Bounded         bool
	Min, Max        int64
}

// Dump renders every registered key with its live value + surface hints, in the
// stable Specs order. Secrets are never included.
func (c *Config) Dump() []DumpItem {
	specs := Specs()
	out := make([]DumpItem, 0, len(specs))
	for _, s := range specs {
		out = append(out, DumpItem{
			Key:             s.Key,
			Value:           s.get(c),
			Tier:            s.Tier,
			Editable:        s.Tier == TierRuntimeHot,
			RestartRequired: s.RestartRequired,
			Bounded:         s.Bounded,
			Min:             s.Min,
			Max:             s.Max,
		})
	}
	return out
}

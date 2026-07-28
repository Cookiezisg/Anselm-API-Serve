package configprovider

import (
	"fmt"

	"github.com/sunweilin/anselm/gateway/internal/domain/config"
)

// DumpItem is one dashboard-surfaced config item: key, current effective value,
// whether it is editable (runtime-hot) vs restart-required, and — for a bounded
// numeric runtime knob — the inclusive Min/Max the dashboard pre-validates against
// (the SAME bounds ApplyOverrides enforces server-side, so a client check can
// never diverge). Min/Max are *int64 so a non-numeric spec (PUBLIC_MODEL_ID) or a
// read-only restart item omits them (null).
//
// 🔴 Dump 只含非机密项:机密(DEEPSEEK_API_KEY / DASHSCOPE_API_KEY / DASHBOARD_* / INSTALL_POW_SECRET)
// 不在 config.Specs,故永不出现在 Dump,绝不泄露真值。
type DumpItem struct {
	Key             string `json:"key"`
	Value           string `json:"value"`
	Editable        bool   `json:"editable"`
	RestartRequired bool   `json:"restartRequired"`
	Min             *int64 `json:"min,omitempty"`
	Max             *int64 `json:"max,omitempty"`
}

// Dump renders the CONFIGURED config as the dashboard's read model — every
// runtime + restart-constraint item with the value the operator set. SECRETS ARE
// NEVER INCLUDED (they are absent from config.Specs). The numeric bounds are
// surfaced only for a bounded runtime knob (the client pre-validation hint).
//
// It deliberately reads Configured(), not Load(): this table is an EDITOR, and an
// editor that showed the debug mask would invite the operator to "fix" a masked 0
// back to 8 — writing the mask into their own settings and losing the production
// value. The GATEWAY_MODE row is right there in the same table saying which
// posture is live.
//
// 刻意读 Configured() 而非 Load():这张表是**编辑器**,一个显示掩码值的编辑器会诱使运营者
// 把被掩成 0 的值「改回」8——等于把掩码写进自己的配置、丢掉生产值。同一张表里的
// GATEWAY_MODE 那一行会说清楚当前是哪种姿态。
func (p *Provider) Dump() []DumpItem {
	items := p.Configured().Dump()
	out := make([]DumpItem, 0, len(items))
	for _, it := range items {
		d := DumpItem{
			Key:             it.Key,
			Value:           it.Value,
			Editable:        it.Editable,
			RestartRequired: it.RestartRequired,
		}
		if it.Bounded && it.Editable {
			// Copy into locals so each item gets its own pointer (the range var aliases).
			lo, hi := it.Min, it.Max
			d.Min, d.Max = &lo, &hi
		}
		out = append(out, d)
	}
	return out
}

// Snapshot returns the effective config as SECRET-SAFE, low-cardinality log attrs
// for the startup config_snapshot line: provider keys → counts + fixed masked
// markers, and the PoW secret → a provenance marker
// (configured/disabled), everything else a plain scalar. No secret byte ever
// reaches journald (GW-INV: secret-env-only). Lives in infra (not domain) because
// the secret VALUES live only on the env-loaded Config the provider holds.
// Unlike Dump (the editor), this reports the EFFECTIVE config: the one question a
// startup log line has to answer is "what is this process enforcing right now",
// and under GATEWAY_MODE=debug that is the masked set. gateway_mode leads the
// line so the masked zeros are never read as a misconfiguration.
//
// 与 Dump(编辑器)相反,这里报**生效**配置:启动日志唯一要回答的问题是「这个进程此刻在执行
// 什么」,而 debug 下那就是掩码后的那一套。gateway_mode 排在最前,免得那些被掩成 0 的值
// 被误读成配置错误。
func (p *Provider) Snapshot() []any {
	c := p.Load()
	return []any{
		"gateway_mode", c.RuntimeMode,
		"deepseek_keys", fmt.Sprintf("sk-*** (%d configured)", len(c.DeepSeekAPIKeys)),
		"deepseek_base_url", c.DeepSeekBaseURL,
		"qwen_keys", fmt.Sprintf("*** (%d configured)", len(c.QwenAPIKeys)),
		"dashscope_base_url", c.QwenBaseURL,
		"public_model_id", c.PublicModelID,
		"text_upstream_model", c.TextUpstreamModel,
		"multimodal_upstream_model", c.MultimodalUpstreamModel,
		"monthly_quota", c.MonthlyQuota,
		"global_monthly_spend_micro_usd", c.GlobalMonthlySpendPUSD / 1_000_000,
		"max_tokens_cap", c.MaxTokensCap,
		"input_token_cap", c.InputTokenCap,
		"max_messages", c.MaxMessages,
		"max_message_chars", c.MaxMessageChars,
		"max_media_parts", c.MaxMediaParts,
		"max_media_decoded_bytes", c.MaxMediaDecodedBytes,
		"max_body_bytes", c.MaxBodyBytes,
		"n_global_concurrency", c.NGlobalConcurrency,
		"rate_per_min", c.RatePerMin,
		"daily_sublimit", c.DailySublimit,
		"image_enabled", c.ImageEnabled,
		"image_upstream_model", c.ImageUpstreamModel,
		"image_daily_limit", c.ImageDailyLimit,
		"media_fetch_domain", c.MediaFetchDomain,
		"voice_daily_limit", c.VoiceDailyLimit,
		"voice_account_ceiling", c.VoiceAccountCeiling,
		"dashscope_native_base", c.DashScopeNativeBase,
		"install_per_ip_hour", c.InstallPerIPHour,
		"install_global_daily_cap", c.InstallGlobalDailyCap,
		"install_per_fp_daily", c.InstallPerFPDaily,
		"install_per_fp_cooldown_sec", c.InstallPerFPCooldownSec,
		"install_pow_mode", c.InstallPowMode,
		"install_pow_difficulty", c.InstallPowDifficulty,
		"install_pow_secret", powSecretMasked(c),
		"token_anomaly_rpm", c.TokenAnomalyRPM,
		"token_throttle_factor", c.TokenThrottleFactor,
		"token_throttle_cooldown_sec", c.TokenThrottleCooldownSec,
		"upstream_header_timeout", c.UpstreamHeaderTimeout.String(),
		"queue_wait", c.QueueWait.String(),
		"reset_tz", c.ResetTZ,
		"listen_addr", c.ListenAddr,
		"admin_addr", c.AdminAddr,
		"dashboard_addr", c.DashboardAddr,
		"dashboard_auth_mode", c.DashboardAuthMode,
		"log_level", c.LogLevel,
		"db_path", c.DBPath,
		"disk_min_mb", c.DiskMinMB,
		"disk_min_percent", c.DiskMinPercent,
		"media_enabled", c.MediaEnabled,
		"media_staging_root", c.MediaStagingRoot,
		"media_upload_max_bytes", c.MediaUploadMaxBytes,
		"media_chunk_max_bytes", c.MediaChunkMaxBytes,
		"media_upload_ttl", c.MediaUploadTTL.String(),
		"media_lease_ttl", c.MediaLeaseTTL.String(),
		"media_signing_secret", c.MediaSigningSecretSource,
	}
}

// powSecretMasked reports secret provenance only (never the value) for the
// snapshot.
func powSecretMasked(c *config.Config) string {
	if c.InstallPowSecretSource == "" {
		return "disabled"
	}
	return c.InstallPowSecretSource
}

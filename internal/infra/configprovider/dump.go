package configprovider

import (
	"fmt"
	"strings"

	"github.com/sunweilin/anselm/gateway/internal/domain/config"
)

// DumpItem is one dashboard-surfaced config item: key, current effective value,
// whether it is editable (runtime-hot) vs restart-required, and — for a bounded
// numeric runtime knob — the inclusive Min/Max the dashboard pre-validates against
// (the SAME bounds ApplyOverrides enforces server-side, so a client check can
// never diverge). Min/Max are *int64 so a non-numeric spec (MODEL_ALLOWLIST) or a
// read-only restart item omits them (null).
//
// 🔴 Dump 只含非机密项:机密(DEEPSEEK_API_KEY / DASHBOARD_* / INSTALL_POW_SECRET)
// 不在 config.Specs,故永不出现在 Dump,绝不泄露真值。
type DumpItem struct {
	Key             string `json:"key"`
	Value           string `json:"value"`
	Editable        bool   `json:"editable"`
	RestartRequired bool   `json:"restartRequired"`
	Min             *int64 `json:"min,omitempty"`
	Max             *int64 `json:"max,omitempty"`
}

// Dump renders the current effective config as the dashboard's read model — every
// runtime + restart-constraint item with its live value. SECRETS ARE NEVER
// INCLUDED (they are absent from config.Specs). The numeric bounds are surfaced
// only for a bounded runtime knob (the client pre-validation hint).
func (p *Provider) Dump() []DumpItem {
	items := p.Load().Dump()
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
// for the startup config_snapshot line: DeepSeek key(s) → a count + a fixed
// `sk-***` marker, the PoW secret + dashboard auth → provenance markers
// (configured/disabled), everything else a plain scalar. No secret byte ever
// reaches journald (GW-INV: secret-env-only). Lives in infra (not domain) because
// the secret VALUES live only on the env-loaded Config the provider holds.
func (p *Provider) Snapshot() []any {
	c := p.Load()
	return []any{
		"deepseek_keys", fmt.Sprintf("sk-*** (%d configured)", len(c.DeepSeekAPIKeys)),
		"deepseek_base_url", c.DeepSeekBaseURL,
		"model_allowlist", strings.Join(c.ModelAllowlist, ","),
		"default_model", c.DefaultModel,
		"monthly_quota", c.MonthlyQuota,
		"global_daily_budget_tokens", c.GlobalDailyBudget,
		"install_daily_token_cap", c.InstallDailyTokenCap,
		"max_tokens_cap", c.MaxTokensCap,
		"input_token_cap", c.InputTokenCap,
		"max_messages", c.MaxMessages,
		"max_message_chars", c.MaxMessageChars,
		"max_body_bytes", c.MaxBodyBytes,
		"n_global_concurrency", c.NGlobalConcurrency,
		"rate_per_min", c.RatePerMin,
		"daily_sublimit", c.DailySublimit,
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
		"log_level", c.LogLevel,
		"db_path", c.DBPath,
		"disk_min_mb", c.DiskMinMB,
		"disk_min_percent", c.DiskMinPercent,
		"dashboard_auth", dashboardAuthMasked(c),
	}
}

// powSecretMasked / dashboardAuthMasked report secret PROVENANCE only (never the
// value) for the snapshot.
func powSecretMasked(c *config.Config) string {
	if c.InstallPowSecretSource == "" {
		return "disabled"
	}
	return c.InstallPowSecretSource
}

func dashboardAuthMasked(c *config.Config) string {
	if c.DashboardUser == "" {
		return "disabled"
	}
	return "configured (user+password set)"
}

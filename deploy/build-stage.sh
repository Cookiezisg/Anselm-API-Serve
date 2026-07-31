#!/usr/bin/env bash
# Build the complete deployment payload in a caller-owned, mode-0700 directory.
# Secrets are serialized for systemd EnvironmentFile without ever becoming
# command-line arguments. The output is content-addressed by manifest.sha256.
set -euo pipefail
set +x
umask 077

die() {
	printf 'build-stage: %s\n' "$*" >&2
	exit 1
}

file_mode() {
	stat -c '%a' "$1" 2>/dev/null || stat -f '%Lp' "$1"
}

require_single_line() {
	local name="$1" value="$2"
	[[ "${value}" != *$'\n'* && "${value}" != *$'\r'* ]] ||
		die "${name} must not contain CR or LF"
}

valid_hostname() {
	local value="$1" label
	[[ ${#value} -ge 1 && ${#value} -le 253 ]] || return 1
	[[ "${value}" =~ ^[A-Za-z0-9.-]+$ ]] || return 1
	[[ "${value}" != .* && "${value}" != *. && "${value}" != *..* ]] || return 1
	IFS=. read -r -a labels <<<"${value}"
	for label in "${labels[@]}"; do
		[[ ${#label} -ge 1 && ${#label} -le 63 ]] || return 1
		[[ "${label}" =~ ^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$ ]] || return 1
	done
}

# systemd.exec(5) double-quoted EnvironmentFile values recognize backslash
# escapes for backslash, double quote, dollar, and backtick. Escaping all four
# preserves the exact bytes while preventing interpolation by the parser.
quote_systemd_value() {
	local value="$1"
	value="${value//\\/\\\\}"
	value="${value//\"/\\\"}"
	value="${value//\$/\\\$}"
	value="${value//\`/\\\`}"
	printf '"%s"' "${value}"
}

write_env() {
	local name="$1" value="$2"
	[[ "${name}" =~ ^[A-Z][A-Z0-9_]*$ ]] || die "invalid environment name: ${name}"
	require_single_line "${name}" "${value}"
	printf '%s=' "${name}" >>"${ENV_FILE}"
	quote_systemd_value "${value}" >>"${ENV_FILE}"
	printf '\n' >>"${ENV_FILE}"
}

write_meta() {
	local name="$1" value="$2"
	require_single_line "${name}" "${value}"
	printf '%s' "${value}" >"${STAGE}/meta/${name}"
	chmod 0600 "${STAGE}/meta/${name}"
}

[[ $# -eq 3 ]] || die "usage: $0 STAGE_DIR BINARY REPO_ROOT"
STAGE="$1"
BINARY="$2"
REPO_ROOT="$3"

[[ -d "${STAGE}" && ! -L "${STAGE}" ]] || die "stage must be an existing directory"
[[ "$(file_mode "${STAGE}")" == 700 ]] || die "stage directory must have mode 0700"
[[ -f "${BINARY}" && ! -L "${BINARY}" ]] || die "binary must be a regular non-symlink"
[[ -d "${REPO_ROOT}" && ! -L "${REPO_ROOT}" ]] || die "repo root is invalid"

: "${DASHSCOPE_API_KEY:?DASHSCOPE_API_KEY is required}"
: "${DASHSCOPE_WORKSPACE_ID:?DASHSCOPE_WORKSPACE_ID is required}"
: "${MEDIA_SIGNING_SECRET:?MEDIA_SIGNING_SECRET is required}"
: "${GATEWAY_DOMAIN:?GATEWAY_DOMAIN is required}"
: "${ACME_EMAIL:?ACME_EMAIL is required}"
: "${SHA:?SHA is required}"
SITE_DOMAIN="${SITE_DOMAIN:-}"
MEDIA_DOMAIN="${MEDIA_DOMAIN:-}"
RESET_UNLAUNCHED_GATEWAY_DATA="${RESET_UNLAUNCHED_GATEWAY_DATA:-0}"

for secret_name in DASHSCOPE_API_KEY DASHSCOPE_WORKSPACE_ID MEDIA_SIGNING_SECRET; do
	require_single_line "${secret_name}" "${!secret_name}"
done
[[ ${#MEDIA_SIGNING_SECRET} -ge 32 ]] ||
	die "MEDIA_SIGNING_SECRET must contain at least 32 bytes"
[[ "${DASHSCOPE_WORKSPACE_ID}" =~ ^[A-Za-z0-9_-]{1,128}$ ]] ||
	die "DASHSCOPE_WORKSPACE_ID must contain only letters, digits, underscore, or hyphen"
require_single_line GATEWAY_DOMAIN "${GATEWAY_DOMAIN}"
valid_hostname "${GATEWAY_DOMAIN}" || die "GATEWAY_DOMAIN is not a valid hostname"
if [[ -z "${SITE_DOMAIN}" ]]; then
	[[ "${GATEWAY_DOMAIN}" == api.* ]] ||
		die "SITE_DOMAIN is unset and GATEWAY_DOMAIN is not api.<root>"
	SITE_DOMAIN="${GATEWAY_DOMAIN#api.}"
fi
require_single_line SITE_DOMAIN "${SITE_DOMAIN}"

# MEDIA_DOMAIN defaults to media.<root>, the same derivation SITE_DOMAIN uses. It is required
# because Caddy cannot render an empty host — the gateway binary tolerates an unset one (voice
# enrollment simply unavailable), but a deploy must make the choice explicit.
#
# It must NOT be an api.* name: the upstream fetcher blacklists that shape at its own edge
# (ADR 0012's production experiment — three 400s while the origin log proved no request arrived).
#
# MEDIA_DOMAIN 缺省取 media.<root>,与 SITE_DOMAIN 同一套推导。它是**必需**的,因为 Caddy 渲染不了
# 空主机名——网关二进制容许不设(音色登记不可用而已),但部署必须把这个选择摆到明面上。
#
# 它**绝不能**是 api.* 的名字:拉取器在它自己的边缘拒绝那个形状(ADR 0012 生产实验——三次 400,而
# 源站日志证明请求从未到达)。
if [[ -z "${MEDIA_DOMAIN}" ]]; then
	MEDIA_DOMAIN="media.${SITE_DOMAIN}"
fi
require_single_line MEDIA_DOMAIN "${MEDIA_DOMAIN}"
[[ "${MEDIA_DOMAIN}" != api.* ]] ||
	die "MEDIA_DOMAIN must not be an api.* host; the upstream fetcher blacklists that shape"
valid_hostname "${SITE_DOMAIN}" || die "SITE_DOMAIN is not a valid hostname"
require_single_line ACME_EMAIL "${ACME_EMAIL}"
[[ "${ACME_EMAIL}" =~ ^[A-Za-z0-9.!#$%\&\'*+/=?^_\`{|}~-]+@[A-Za-z0-9.-]+$ ]] ||
	die "ACME_EMAIL is not a valid deployment email"
[[ "${SHA}" =~ ^[0-9a-f]{12}$ ]] || die "SHA must be 12 lowercase hexadecimal characters"
[[ "${RESET_UNLAUNCHED_GATEWAY_DATA}" == 0 || "${RESET_UNLAUNCHED_GATEWAY_DATA}" == 1 ]] ||
	die "RESET_UNLAUNCHED_GATEWAY_DATA must be 0 or 1"

install -m 0755 "${BINARY}" "${STAGE}/anselm-gateway"
install -m 0644 "${REPO_ROOT}/Caddyfile" "${STAGE}/Caddyfile"
install -m 0644 "${REPO_ROOT}/deploy/anselm-gateway.service" "${STAGE}/anselm-gateway.service"
install -m 0644 "${REPO_ROOT}/deploy/anselm-gateway.socket" "${STAGE}/anselm-gateway.socket"
install -m 0644 "${REPO_ROOT}/deploy/anselm-caddy-deploy-guard.conf" "${STAGE}/anselm-caddy-deploy-guard.conf"
install -m 0755 "${REPO_ROOT}/deploy/install.sh" "${STAGE}/install.sh"
install -m 0755 "${REPO_ROOT}/deploy/render-caddy.sh" "${STAGE}/render-caddy.sh"
install -m 0755 "${REPO_ROOT}/deploy/rollback.sh" "${STAGE}/rollback.sh"
install -d -m 0700 "${STAGE}/site" "${STAGE}/meta"
install -m 0644 "${REPO_ROOT}/deploy/site/index.html" "${STAGE}/site/index.html"
install -m 0644 "${REPO_ROOT}/deploy/site/styles.css" "${STAGE}/site/styles.css"

ENV_FILE="${STAGE}/gateway.env"
: >"${ENV_FILE}"
chmod 0600 "${ENV_FILE}"
write_env DASHSCOPE_API_KEY "${DASHSCOPE_API_KEY}"
write_env DASHSCOPE_WORKSPACE_ID "${DASHSCOPE_WORKSPACE_ID}"
write_env MEDIA_SIGNING_SECRET "${MEDIA_SIGNING_SECRET}"

# Production-operable defaults. Runtime-editable values can still be overlaid
# from SQLite, but a fresh install is safe to expose without a later hardening
# pass.
#
# GATEWAY_MODE is the rationing master switch (config.EffectiveLimits). debug opens
# EVERY per-user gate — monthly request quota, operator spend wallet, rate bucket,
# daily sublimit, image/speech/video daily caps, install-issuance gates, PoW — while
# leaving body/media/memory protection and full spend accounting in place. It ships
# debug because this gateway is still being developed against; flip it to production
# (this file, or live from the dashboard — it is runtime-hot, no restart) before the
# gateway serves anyone but its operator.
#
# NOTE the values below are the CURRENT posture, most of them 0 = off from the dev
# phase. Selecting production arms them AS WRITTEN, so it is only a hardening if they
# hold hardened numbers. The set recorded in cde6b91 ("deploy: tighten public API
# abuse controls") was: RATE_PER_MIN=8, DAILY_SUBLIMIT=100, INSTALL_GLOBAL_DAILY_CAP=100,
# INSTALL_PER_FP_DAILY=3, INSTALL_PER_FP_COOLDOWN_SEC=3600, TOKEN_ANOMALY_RPM=8.
#
# GATEWAY_MODE 是配额总闸(config.EffectiveLimits)。debug 打开**每一道**面向用户的闸——月请求额度、
# operator 花费钱包、令牌桶、日次数子限、图/语音/视频日闸、领号闸、PoW——同时保留 body/media/内存
# 保护与**完整记账**。默认发 debug 是因为本网关仍在自研阶段;在它开始服务运营者以外的人之前,
# 把它切成 production(改本文件,或后台热切——它是 runtime-hot、不必重启)。
# 注意下面这些值是**当前**姿态、多数是开发期留下的 0=关。选 production 是**照写下的值**上膛,
# 所以只有当它们是收紧后的数字时才算收紧。cde6b91 记录的那套值见上方英文注释。
write_env GATEWAY_MODE "debug"
write_env PUBLIC_MODEL_ID "anselm-auto"
write_env MULTIMODAL_UPSTREAM_MODEL "qwen3.7-plus"
write_env GLOBAL_MONTHLY_SPEND_MICRO_USD "420000000"
write_env INPUT_TOKEN_CAP "0"
write_env MAX_TOKENS_CAP "16384"
write_env MAX_MESSAGES "4096"
write_env MAX_MESSAGE_CHARS "4194304"
write_env MAX_BODY_BYTES "8388608"
write_env MAX_MEDIA_PARTS "8"
write_env MAX_MEDIA_DECODED_BYTES "3145728"
write_env MEDIA_ENABLED "true"
write_env MEDIA_STAGING_ROOT "/var/lib/anselm-gateway/media-staging"
write_env MEDIA_UPLOAD_MAX_BYTES "104857600"
write_env MEDIA_CHUNK_MAX_BYTES "4194304"
write_env MEDIA_UPLOAD_TTL_SEC "3600"
write_env MEDIA_LEASE_TTL_SEC "3600"
# MEDIA_DOMAIN must reach the GATEWAY PROCESS too, not only Caddy. It was rendered into the
# reverse proxy (a vhost that served fine) while the binary was never told the host — so
# `PublicFetchURL` had nothing to build an address from and **every** enrollment answered 503
# VOICE_UNAVAILABLE, indistinguishable from "speech is switched off on this deployment". A value
# handed to the proxy but not to the app is the shape of misconfiguration that looks healthy from
# outside: the vhost answers, the certificate is valid, and the one thing that needed the name
# never had it.
# MEDIA_DOMAIN 必须**也**抵达网关进程、不能只到 Caddy。它此前只被渲染进反向代理(vhost 服务得好好的),
# 而二进制**从没被告知**那个主机名——于是 `PublicFetchURL` 没有东西可以拼地址,**每一次**登记都答 503
# VOICE_UNAVAILABLE,与「本部署关掉了语音」无从分辨。一个只给了代理、没给应用的值,正是**从外面看很
# 健康**的那类错配:vhost 会应答、证书有效,而唯一需要这个名字的东西从来没拿到它。
write_env MEDIA_DOMAIN "${MEDIA_DOMAIN}"
# The three generation capabilities. They ship DEFAULT-OFF in code — a
# capability, not a birthright — which means an unlisted one is simply absent in
# production no matter how complete its code is. Each carries its own daily cap in
# its own unit: images per PICTURE, speech per CHARACTER, video per CLIP.
# 三个生成能力。代码里默认**关**——能力非天赋——也就是说没在这里列出的那个,无论代码写得
# 多完整,在生产上**根本不存在**。各自带自己的日上限、各自的单位:图像按**张**、语音按**字符**、
# 视频按**条**。
write_env IMAGE_ENABLED "true"
write_env IMAGE_DAILY_LIMIT "10"
write_env SPEECH_ENABLED "true"
write_env SPEECH_DAILY_LIMIT "50000"
# TTS_DEFAULT_VOICE is deliberately NOT written: the correct value belongs to the MODEL, and the
# binary already knows it. Pinning it here is how production ended up overriding a working default
# (`longanhuan_v3.6`) with `Cherry` — a qwen3-tts name that the cosyvoice engine behind
# `qwen-audio-3.0-tts-flash` rejects outright, so **every** managed synthesis that omitted `voice`
# failed with `[cosyvoice:]Engine error [411]`. Two places holding one fact is how they drift.
# TTS_DEFAULT_VOICE 刻意**不写**:正确值属于**模型**,而二进制本来就知道它。在这里钉一个,正是生产用
# `Cherry` 覆盖掉一个能用的默认值(`longanhuan_v3.6`)的原因——那是 qwen3-tts 那套名字,而
# `qwen-audio-3.0-tts-flash` 背后的 cosyvoice 引擎**直接拒绝**它,于是**每一次**省略 `voice` 的受管合成
# 都以 `[cosyvoice:]Engine error [411]` 失败。一个事实存两处,就是它们分岔的方式。
write_env VIDEO_ENABLED "true"
write_env VIDEO_DAILY_LIMIT "10"
# DASHSCOPE_NATIVE_BASE is deliberately NOT written: it derives from the workspace
# credential, and pinning a region here is exactly how a Singapore key ends up
# asking Beijing and getting 401 "Incorrect API key provided".
# DASHSCOPE_NATIVE_BASE 刻意**不写**:它从 workspace 凭证派生,而在这里钉一个区域,正是一把新加坡
# key 去问北京、拿回 401 "Incorrect API key provided" 的成因。
# 由已校验的 GATEWAY_DOMAIN 派生,不新增部署输入——保证网关自报的公开 origin 与它实际被服务的域名
# 恒一致。它是 chat 把**相对** lease 引用绝对化后交给上游 provider 的前缀(ADR 0011)。
write_env N_GLOBAL_CONCURRENCY "16"
write_env QUEUE_WAIT_MS "1500"
write_env RATE_PER_MIN "0"
write_env DAILY_SUBLIMIT "0"
write_env INSTALL_GLOBAL_DAILY_CAP "0"
write_env INSTALL_PER_FP_DAILY "0"
write_env INSTALL_PER_FP_COOLDOWN_SEC "0"
write_env INSTALL_PER_IP_HOUR "0"
write_env TOKEN_ANOMALY_RPM "0"
write_env TOKEN_THROTTLE_FACTOR "4"
write_env TOKEN_THROTTLE_COOLDOWN_SEC "600"
write_env GOMEMLIMIT_MIB "768"
write_env SQLITE_CACHE_KIB "32768"
write_env SQLITE_MMAP_MB "256"
write_env READ_POOL_MAX_CONNS "4"
write_env DISK_MIN_MB "500"
write_env DISK_MIN_PERCENT "5"
write_env GATEWAY_DB_PATH "/var/lib/anselm-gateway/anselm-gateway.db"
write_env LISTEN_ADDR "127.0.0.1:8080"
write_env DASHBOARD_ADDR "127.0.0.1:8081"
write_env LOG_LEVEL "info"
write_env ADMIN_ADDR "127.0.0.1:9090"

write_meta gateway-domain "${GATEWAY_DOMAIN}"
write_meta site-domain "${SITE_DOMAIN}"
write_meta media-domain "${MEDIA_DOMAIN}"
write_meta acme-email "${ACME_EMAIL}"
write_meta reset-unlaunched-gateway-data "${RESET_UNLAUNCHED_GATEWAY_DATA}"
write_meta sha "${SHA}"

(
	cd "${STAGE}"
	find . -type f ! -name manifest.sha256 -print0 |
		LC_ALL=C sort -z |
		xargs -0 sha256sum >manifest.sha256
)
chmod 0600 "${STAGE}/manifest.sha256"

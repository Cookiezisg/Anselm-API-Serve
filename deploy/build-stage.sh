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

: "${DEEPSEEK_API_KEY:?DEEPSEEK_API_KEY is required}"
: "${GATEWAY_DOMAIN:?GATEWAY_DOMAIN is required}"
: "${ACME_EMAIL:?ACME_EMAIL is required}"
: "${SHA:?SHA is required}"
GEMINI_API_KEY="${GEMINI_API_KEY:-}"
DASHBOARD_USER="${DASHBOARD_USER:-}"
DASHBOARD_PASSWORD="${DASHBOARD_PASSWORD:-}"
SITE_DOMAIN="${SITE_DOMAIN:-}"

for secret_name in DEEPSEEK_API_KEY GEMINI_API_KEY DASHBOARD_USER DASHBOARD_PASSWORD; do
	require_single_line "${secret_name}" "${!secret_name}"
done
[[ -z "${DASHBOARD_USER}" && -z "${DASHBOARD_PASSWORD}" ||
	-n "${DASHBOARD_USER}" && -n "${DASHBOARD_PASSWORD}" ]] ||
	die "DASHBOARD_USER and DASHBOARD_PASSWORD must be set together"

require_single_line GATEWAY_DOMAIN "${GATEWAY_DOMAIN}"
valid_hostname "${GATEWAY_DOMAIN}" || die "GATEWAY_DOMAIN is not a valid hostname"
if [[ -z "${SITE_DOMAIN}" ]]; then
	[[ "${GATEWAY_DOMAIN}" == api.* ]] ||
		die "SITE_DOMAIN is unset and GATEWAY_DOMAIN is not api.<root>"
	SITE_DOMAIN="${GATEWAY_DOMAIN#api.}"
fi
require_single_line SITE_DOMAIN "${SITE_DOMAIN}"
valid_hostname "${SITE_DOMAIN}" || die "SITE_DOMAIN is not a valid hostname"
require_single_line ACME_EMAIL "${ACME_EMAIL}"
[[ "${ACME_EMAIL}" =~ ^[A-Za-z0-9.!#$%\&\'*+/=?^_\`{|}~-]+@[A-Za-z0-9.-]+$ ]] ||
	die "ACME_EMAIL is not a valid deployment email"
[[ "${SHA}" =~ ^[0-9a-f]{12}$ ]] || die "SHA must be 12 lowercase hexadecimal characters"

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
write_env DEEPSEEK_API_KEY "${DEEPSEEK_API_KEY}"
write_env GEMINI_API_KEY "${GEMINI_API_KEY}"
write_env DASHBOARD_USER "${DASHBOARD_USER}"
write_env DASHBOARD_PASSWORD "${DASHBOARD_PASSWORD}"

# Production-operable defaults. Runtime-editable values can still be overlaid
# from SQLite, but a fresh install is safe to expose without a later hardening
# pass.
write_env DEEPSEEK_BASE_URL "https://api.deepseek.com"
write_env GEMINI_BASE_URL "https://generativelanguage.googleapis.com/v1beta/openai"
write_env PUBLIC_MODEL_ID "anselm-auto"
write_env TEXT_UPSTREAM_MODEL "deepseek-v4-flash"
write_env MULTIMODAL_UPSTREAM_MODEL "gemini-3.1-flash-lite"
write_env GLOBAL_DAILY_SPEND_MICRO_USD "14000000"
write_env INSTALL_DAILY_SPEND_MICRO_USD "5600000"
write_env DEEPSEEK_DAILY_SPEND_MICRO_USD "14000000"
write_env GEMINI_DAILY_SPEND_MICRO_USD "14000000"
write_env INPUT_TOKEN_CAP "131072"
write_env MAX_TOKENS_CAP "16384"
write_env MAX_MESSAGES "1024"
write_env MAX_MESSAGE_CHARS "262144"
write_env MAX_BODY_BYTES "5242880"
write_env MAX_MEDIA_PARTS "8"
write_env MAX_MEDIA_DECODED_BYTES "3145728"
write_env N_GLOBAL_CONCURRENCY "16"
write_env QUEUE_WAIT_MS "1500"
write_env RATE_PER_MIN "8"
write_env DAILY_SUBLIMIT "100"
write_env INSTALL_GLOBAL_DAILY_CAP "100"
write_env INSTALL_PER_FP_DAILY "3"
write_env INSTALL_PER_FP_COOLDOWN_SEC "3600"
write_env INSTALL_PER_IP_HOUR "10"
write_env TOKEN_ANOMALY_RPM "8"
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
write_meta acme-email "${ACME_EMAIL}"
write_meta sha "${SHA}"

(
	cd "${STAGE}"
	find . -type f ! -name manifest.sha256 -print0 |
		LC_ALL=C sort -z |
		xargs -0 sha256sum >manifest.sha256
)
chmod 0600 "${STAGE}/manifest.sha256"

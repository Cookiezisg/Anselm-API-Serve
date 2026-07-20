#!/usr/bin/env bash
# Production EnvironmentFile and payload-manifest regression tests.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"
TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/anselm-build-stage-test.XXXXXXXXXX")"

cleanup_test() {
	[[ "${TEST_ROOT}" =~ /anselm-build-stage-test\.[A-Za-z0-9]{10}$ ]] || return 1
	rm -rf -- "${TEST_ROOT}"
}
trap cleanup_test EXIT

fail() {
	printf 'build_stage_test: %s\n' "$*" >&2
	exit 1
}

STAGE="${TEST_ROOT}/stage"
mkdir -m 0700 "${STAGE}"
DEEPSEEK_API_KEY='alpha\beta"gamma$delta' \
	KIMI_API_KEY='' \
	DASHBOARD_USER='' \
	DASHBOARD_PASSWORD='' \
	GATEWAY_DOMAIN='api.example.com' \
	SITE_DOMAIN='' \
	ACME_EMAIL='ops+anselm@example.com' \
	SHA='0123456789ab' \
	bash "${SCRIPT_DIR}/build-stage.sh" "${STAGE}" "${REPO_ROOT}/go.mod" "${REPO_ROOT}"

first_line="$(head -n 1 "${STAGE}/gateway.env")"
[[ "${first_line}" == 'DEEPSEEK_API_KEY="alpha\\beta\"gamma\$delta"' ]] ||
	fail "systemd quoting did not preserve/escape secret bytes"

for pair in \
	'INPUT_TOKEN_CAP="131072"' \
	'MAX_TOKENS_CAP="16384"' \
	'MAX_MESSAGES="1024"' \
	'MAX_MESSAGE_CHARS="262144"' \
	'RATE_PER_MIN="8"' \
	'DAILY_SUBLIMIT="100"' \
	'INSTALL_GLOBAL_DAILY_CAP="100"' \
	'INSTALL_PER_FP_DAILY="0"' \
	'INSTALL_PER_FP_COOLDOWN_SEC="0"' \
	'INSTALL_PER_IP_HOUR="10"' \
	'TOKEN_ANOMALY_RPM="8"'; do
	grep -Fqx "${pair}" "${STAGE}/gateway.env" || fail "missing production config: ${pair}"
done
(cd "${STAGE}" && sha256sum --strict -c manifest.sha256 >/dev/null) || fail "payload manifest failed"
cmp -s "${STAGE}/rollback.sh" "${SCRIPT_DIR}/rollback.sh" ||
	fail "bundle recovery program differs from reviewed rollback implementation"
grep -Eq '^[0-9a-f]{64}  \./rollback\.sh$' "${STAGE}/manifest.sha256" ||
	fail "bundle recovery program is not covered by the payload manifest"

# The old global rollback entry must be snapshotted before the durable marker,
# and the new entry may be switched only after that marker exists. Automatic
# restore must use the bundle-local program because the global entry is itself a
# restored artifact.
INSTALL_SCRIPT="${SCRIPT_DIR}/install.sh"
snapshot_line="$(grep -nF 'snapshot_regular "${ROLLBACK_TOOL}" anselm-gateway-rollback had-rollback-tool' "${INSTALL_SCRIPT}" | cut -d: -f1)"
marker_line="$(grep -nF 'publish_transition_marker deploy' "${INSTALL_SCRIPT}" | cut -d: -f1)"
switch_line="$(grep -nF 'sudo mv -Tf "${ROLLBACK_TOOL_TMP}" "${ROLLBACK_TOOL}"' "${INSTALL_SCRIPT}" | cut -d: -f1)"
[[ -n "${snapshot_line}" && -n "${marker_line}" && -n "${switch_line}" ]] ||
	fail "rollback-entry transaction boundaries are missing"
(( snapshot_line < marker_line && marker_line < switch_line )) ||
	fail "rollback entry is not snapshotted/marked/switched in crash-safe order"
grep -Fq 'sudo "${BUNDLE}/recovery/rollback.sh" --automatic --bundle "${BUNDLE}"' "${INSTALL_SCRIPT}" ||
	fail "automatic restore does not use the bundle-local recovery program"

RENDERED_CADDY="${TEST_ROOT}/rendered.Caddyfile"
bash "${STAGE}/render-caddy.sh" \
	"${STAGE}/Caddyfile" "${RENDERED_CADDY}" \
	'api.example.com' 'example.com' 'ops+anselm@example.com'
grep -Fq 'api.example.com {' "${RENDERED_CADDY}" || fail "gateway domain was not rendered"
grep -Fq 'example.com {' "${RENDERED_CADDY}" || fail "site domain was not rendered"
grep -Fq 'email ops+anselm@example.com' "${RENDERED_CADDY}" || fail "ACME email was not rendered"
if grep -Fq '{$' "${RENDERED_CADDY}"; then
	fail "Caddy renderer left a literal deployment placeholder"
fi

BAD_STAGE="${TEST_ROOT}/bad-stage"
mkdir -m 0700 "${BAD_STAGE}"
if DEEPSEEK_API_KEY=$'bad\nsecret' \
	KIMI_API_KEY='' \
	DASHBOARD_USER='' \
	DASHBOARD_PASSWORD='' \
	GATEWAY_DOMAIN='api.example.com' \
	SITE_DOMAIN='example.com' \
	ACME_EMAIL='ops@example.com' \
	SHA='0123456789ab' \
	bash "${SCRIPT_DIR}/build-stage.sh" "${BAD_STAGE}" "${REPO_ROOT}/go.mod" "${REPO_ROOT}" \
	>/dev/null 2>&1; then
	fail "multiline secret was accepted"
fi

printf 'build-stage quoting/config/manifest/Caddy-render tests OK\n'

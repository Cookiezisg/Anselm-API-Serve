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
	DASHSCOPE_API_KEY='dashscope-test-key' \
	DASHSCOPE_WORKSPACE_ID='ws-test' \
	MEDIA_SIGNING_SECRET='media-signing-secret-at-least-32-bytes' \
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
	'GATEWAY_MODE="debug"' \
	'DASHBOARD_AUTH_MODE="disabled"' \
	'GLOBAL_MONTHLY_SPEND_MICRO_USD="420000000"' \
	'INPUT_TOKEN_CAP="0"' \
	'MAX_TOKENS_CAP="16384"' \
	'MAX_MESSAGES="4096"' \
	'MAX_MESSAGE_CHARS="4194304"' \
	'MAX_BODY_BYTES="8388608"' \
	'MEDIA_ENABLED="true"' \
	'MEDIA_STAGING_ROOT="/var/lib/anselm-gateway/media-staging"' \
	'MEDIA_UPLOAD_MAX_BYTES="104857600"' \
	'MEDIA_CHUNK_MAX_BYTES="4194304"' \
	'MEDIA_UPLOAD_TTL_SEC="3600"' \
	'MEDIA_LEASE_TTL_SEC="3600"' \
	'IMAGE_ENABLED="true"' \
	'IMAGE_DAILY_LIMIT="10"' \
	'SPEECH_ENABLED="true"' \
	'SPEECH_DAILY_LIMIT="50000"' \
	'VIDEO_ENABLED="true"' \
	'VIDEO_DAILY_LIMIT="10"' \
	'RATE_PER_MIN="0"' \
	'DAILY_SUBLIMIT="0"' \
	'INSTALL_GLOBAL_DAILY_CAP="0"' \
	'INSTALL_PER_FP_DAILY="0"' \
	'INSTALL_PER_FP_COOLDOWN_SEC="0"' \
	'INSTALL_PER_IP_HOUR="0"' \
	'TOKEN_ANOMALY_RPM="0"'; do
	grep -Fqx "${pair}" "${STAGE}/gateway.env" || fail "missing production config: ${pair}"
done
# The generation origin must stay DERIVED from the credential. Pinning a region in
# the deploy env is how a Singapore workspace key ends up asking Beijing and getting
# 401 — the bundle must not carry that decision at all.
# 生成 origin 必须保持**从凭证派生**。在部署 env 里钉死区域,正是一把新加坡 workspace key 去问北京
# 拿 401 的成因——bundle 里根本不该带这个决定。
if grep -q '^DASHSCOPE_NATIVE_BASE=' "${STAGE}/gateway.env"; then
	fail "deploy env must not pin DASHSCOPE_NATIVE_BASE (it derives from the credential)"
fi
if grep -q '^DASHBOARD_USER=' "${STAGE}/gateway.env" || grep -q '^DASHBOARD_PASSWORD=' "${STAGE}/gateway.env"; then
	fail "disabled dashboard mode must not materialise builtin credentials"
fi
(cd "${STAGE}" && sha256sum --strict -c manifest.sha256 >/dev/null) || fail "payload manifest failed"
grep -Fqx '0' "${STAGE}/meta/reset-unlaunched-gateway-data" || fail "default reset flag is not disabled"
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
grep -Fq 'diagnose_gateway_start_failure()' "${INSTALL_SCRIPT}" ||
	fail "gateway startup diagnostics are missing"
grep -Fq 'if ! sudo systemctl start anselm-gateway.service; then' "${INSTALL_SCRIPT}" ||
	fail "gateway startup failure is not diagnosed before rollback"
redactor_output="$(printf '%s\n' \
	'Authorization: Bearer bearer-secret' \
	'X-API-Key: header-secret' \
	'DASHSCOPE_API_KEY=env-secret' \
	'MEDIA_SIGNING_SECRET=media-signing-secret' \
	'sk-ws-raw-secret-token' | \
	bash -c "$(sed -n '/^redact_diagnostics()/,/^}/p' "${INSTALL_SCRIPT}"); redact_diagnostics")"
[[ "${redactor_output}" != *bearer-secret* && "${redactor_output}" != *header-secret* && \
	"${redactor_output}" != *env-secret* && "${redactor_output}" != *media-signing-secret* && \
	"${redactor_output}" != *sk-ws-raw-secret-token* ]] ||
	fail "gateway startup diagnostics do not redact credentials"

BUILTIN_STAGE="${TEST_ROOT}/builtin-stage"
mkdir -m 0700 "${BUILTIN_STAGE}"
DEEPSEEK_API_KEY='key' \
	DASHSCOPE_API_KEY='dashscope-test-key' \
	DASHSCOPE_WORKSPACE_ID='ws-test' \
	MEDIA_SIGNING_SECRET='media-signing-secret-at-least-32-bytes' \
	DASHBOARD_AUTH_MODE='builtin' \
	DASHBOARD_USER='admin' \
	DASHBOARD_PASSWORD='builtin-secret' \
	GATEWAY_DOMAIN='api.example.com' \
	SITE_DOMAIN='example.com' \
	ACME_EMAIL='ops@example.com' \
	SHA='0123456789ab' \
	bash "${SCRIPT_DIR}/build-stage.sh" "${BUILTIN_STAGE}" "${REPO_ROOT}/go.mod" "${REPO_ROOT}"
grep -Fqx 'DASHBOARD_AUTH_MODE="builtin"' "${BUILTIN_STAGE}/gateway.env" || fail "builtin mode missing"
grep -Fqx 'DASHBOARD_USER="admin"' "${BUILTIN_STAGE}/gateway.env" || fail "builtin user missing"
grep -Fqx 'DASHBOARD_PASSWORD="builtin-secret"' "${BUILTIN_STAGE}/gateway.env" || fail "builtin password missing"

EXTERNAL_STAGE="${TEST_ROOT}/external-stage"
mkdir -m 0700 "${EXTERNAL_STAGE}"
DEEPSEEK_API_KEY='key' \
	DASHSCOPE_API_KEY='dashscope-test-key' \
	DASHSCOPE_WORKSPACE_ID='ws-test' \
	MEDIA_SIGNING_SECRET='media-signing-secret-at-least-32-bytes' \
	RESET_UNLAUNCHED_GATEWAY_DATA='1' \
	DASHBOARD_AUTH_MODE='external' \
	DASHBOARD_USER='stale-user' \
	DASHBOARD_PASSWORD='stale-password' \
	GATEWAY_DOMAIN='api.example.com' \
	SITE_DOMAIN='example.com' \
	ACME_EMAIL='ops@example.com' \
	SHA='0123456789ab' \
	bash "${SCRIPT_DIR}/build-stage.sh" "${EXTERNAL_STAGE}" "${REPO_ROOT}/go.mod" "${REPO_ROOT}"
grep -Fqx 'DASHBOARD_AUTH_MODE="external"' "${EXTERNAL_STAGE}/gateway.env" || fail "external mode missing"
grep -Fqx '1' "${EXTERNAL_STAGE}/meta/reset-unlaunched-gateway-data" || fail "confirmed reset flag missing"
if grep -q '^DASHBOARD_USER=' "${EXTERNAL_STAGE}/gateway.env" || grep -q '^DASHBOARD_PASSWORD=' "${EXTERNAL_STAGE}/gateway.env"; then
	fail "external dashboard mode must not materialise stale builtin credentials"
fi

RENDERED_CADDY="${TEST_ROOT}/rendered.Caddyfile"
bash "${STAGE}/render-caddy.sh" \
	"${STAGE}/Caddyfile" "${RENDERED_CADDY}" \
	'api.example.com' 'example.com' 'ops+anselm@example.com' 'media.example.com'
grep -Fq 'api.example.com {' "${RENDERED_CADDY}" || fail "gateway domain was not rendered"
grep -Fq 'example.com {' "${RENDERED_CADDY}" || fail "site domain was not rendered"
grep -Fq 'email ops+anselm@example.com' "${RENDERED_CADDY}" || fail "ACME email was not rendered"
grep -Fq 'media.example.com {' "${RENDERED_CADDY}" || fail "media domain was not rendered"

# The media host serves the lease route and NOTHING else. Copying the whole API surface onto a
# second hostname would open a second front door nobody is watching.
# 媒体主机只服务 lease 那一条路由、别无其他。把整个 API 面复制到第二个主机名下,等于开了一扇没人
# 在看的正门。
grep -Fq 'handle /v1/media/leases/*' "${RENDERED_CADDY}" || fail "media host does not scope to the lease route"

# An api.* media host fails INVISIBLY against the real upstream (ADR 0012: three 400s, origin log
# empty), so the renderer must refuse it rather than emit a config that looks fine.
# api.* 的媒体主机对着真上游会**无形地**失败(ADR 0012:三次 400、源站日志为空),故渲染器必须拒绝
# 它,而不是吐出一份看着没问题的配置。
if bash "${STAGE}/render-caddy.sh" "${STAGE}/Caddyfile" "${TEST_ROOT}/nope.Caddyfile" \
	'api.example.com' 'example.com' 'ops@example.com' 'api.media.example.com' 2>/dev/null; then
	fail "renderer accepted an api.* MEDIA_DOMAIN"
fi
if grep -Fq '{$' "${RENDERED_CADDY}"; then
	fail "Caddy renderer left a literal deployment placeholder"
fi

BAD_STAGE="${TEST_ROOT}/bad-stage"
mkdir -m 0700 "${BAD_STAGE}"
if DEEPSEEK_API_KEY=$'bad\nsecret' \
	DASHSCOPE_API_KEY='dashscope-test-key' \
	DASHSCOPE_WORKSPACE_ID='ws-test' \
	MEDIA_SIGNING_SECRET='media-signing-secret-at-least-32-bytes' \
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

# The two halves of the payload contract must AGREE: build-stage.sh decides what to ship, and
# remote-entrypoint.sh decides what it will accept. Nothing used to compare them, so H9b's
# `meta/media-domain` shipped for three commits and the server refused every one of them with
# "stage file set differs from the reviewed payload" — a deploy failure whose cause is nowhere near
# its message. This check is the missing comparison, and it lives here because this is the only
# place that has a real stage in hand.
#
# 载荷契约的两半必须**一致**:build-stage.sh 决定发什么,remote-entrypoint.sh 决定收什么。此前没有任何
# 东西比对过它们,于是 H9b 的 `meta/media-domain` 发了三个提交、服务器每一次都以「stage file set differs
# from the reviewed payload」拒收——一句离病因十万八千里的部署失败。这条检查就是那次缺失的比对,放在这里
# 是因为只有这里手上有一个**真的** stage。
# The stage must NOT pin the default voice: that value belongs to the model and lives in the binary.
# 舞台**不得**钉默认音色:那个值属于模型、住在二进制里。
if grep -q '^TTS_DEFAULT_VOICE=' "${STAGE}/gateway.env"; then
	fail "the stage pins TTS_DEFAULT_VOICE — the model's own default must win (a wrong name fails every synthesis)"
fi

EXPECTED_FROM_ENTRYPOINT="$(sed -n "/^EXPECTED_FILES=/,/LC_ALL=C sort)/p" "${SCRIPT_DIR}/remote-entrypoint.sh" |
	grep -oE "'[^']+'" | tr -d "'" | grep -v '%' | LC_ALL=C sort)"
[[ -n "${EXPECTED_FROM_ENTRYPOINT}" ]] || fail "could not read EXPECTED_FILES out of remote-entrypoint.sh"
ACTUAL_FROM_STAGE="$(cd "${STAGE}" && find . -mindepth 1 -type f | sed 's|^\./||' | LC_ALL=C sort)"
if [[ "${EXPECTED_FROM_ENTRYPOINT}" != "${ACTUAL_FROM_STAGE}" ]]; then
	printf 'build_stage_test: staged set and remote-entrypoint EXPECTED_FILES disagree\n' >&2
	diff <(printf '%s\n' "${EXPECTED_FROM_ENTRYPOINT}") <(printf '%s\n' "${ACTUAL_FROM_STAGE}") >&2 || true
	fail "payload contract drift (left = entrypoint accepts, right = build-stage ships)"
fi

printf 'build-stage quoting/config/manifest/Caddy-render/payload-contract tests OK\n'

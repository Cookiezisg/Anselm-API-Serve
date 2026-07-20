#!/usr/bin/env bash
# Failure-propagation tests for the rollback library. These deliberately invoke
# functions from the left side of `if`, the Bash context that disables errexit in
# an entire call stack, so correctness must come from explicit `|| return 1`.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=deploy/rollback.sh
source "${SCRIPT_DIR}/rollback.sh"

fail() {
	printf 'rollback_test: %s\n' "$*" >&2
	exit 1
}

assert_eq() {
	local want="$1" got="$2" label="$3"
	[[ "${got}" == "${want}" ]] || fail "${label}: got '${got}', want '${want}'"
}

TEST_ROOT="$(mktemp -d "${TMPDIR:-/tmp}/anselm-rollback-test.XXXXXXXXXX")"
cleanup_test() {
	[[ "${TEST_ROOT}" =~ /anselm-rollback-test\.[A-Za-z0-9]{10}$ ]] || return 1
	rm -rf -- "${TEST_ROOT}"
}
trap cleanup_test EXIT

# The rollback entry is part of the compatibility unit. Restore its previous
# bytes atomically, and restore true absence on a first-deploy rollback. Stub the
# privileged GNU install/mv flags so this unit test stays runnable on macOS too.
(
	mkdir -p "${TEST_ROOT}/tool-bundle/state" "${TEST_ROOT}/tool-bundle/files"
	ROLLBACK_TOOL="${TEST_ROOT}/live-rollback"
	printf '%s' 'legacy rollback bytes' >"${TEST_ROOT}/tool-bundle/files/anselm-gateway-rollback"
	: >"${TEST_ROOT}/tool-bundle/state/had-rollback-tool"
	printf '%s' 'new rollback bytes' >"${ROLLBACK_TOOL}"
	install() {
		[[ "$1" == -o && "$2" == root && "$3" == -g && "$4" == root && "$5" == -m && "$6" == 0750 ]] || return 1
		command cp "$7" "$8" || return 1
		command chmod 0750 "$8"
	}
	mv() {
		[[ "$1" == -Tf ]] || return 1
		command mv -f "$2" "$3"
	}
	restore_rollback_tool "${TEST_ROOT}/tool-bundle" || fail "could not restore previous rollback entry"
	assert_eq 'legacy rollback bytes' "$(<"${ROLLBACK_TOOL}")" 'rollback entry bytes'
	[[ -x "${ROLLBACK_TOOL}" ]] || fail "restored rollback entry is not executable"

	command rm -f "${TEST_ROOT}/tool-bundle/state/had-rollback-tool"
	command rm -f "${TEST_ROOT}/tool-bundle/files/anselm-gateway-rollback"
	printf '%s' 'new rollback bytes' >"${ROLLBACK_TOOL}"
	restore_rollback_tool "${TEST_ROOT}/tool-bundle" || fail "could not restore absent rollback entry"
	[[ ! -e "${ROLLBACK_TOOL}" && ! -L "${ROLLBACK_TOOL}" ]] ||
		fail "first-deploy rollback left a global rollback entry behind"
)

# A Caddy stop failure must prevent the socket/service stop sequence from being
# treated as a later success.
CALLS=''
stop_and_confirm() {
	CALLS="${CALLS}${1};"
	[[ "$1" != caddy ]]
}
if stop_traffic_and_writers; then
	fail "stop_traffic_and_writers accepted a Caddy stop failure"
fi
assert_eq 'caddy;' "${CALLS}" 'stop sequence must short-circuit'

CALLS=''
stop_and_confirm() {
	CALLS="${CALLS}${1};"
	[[ "$1" != anselm-gateway.socket ]]
}
if stop_traffic_and_writers; then
	fail "stop_traffic_and_writers accepted a socket stop failure"
fi
assert_eq 'caddy;anselm-gateway.socket;' "${CALLS}" 'service stop must not wash out socket failure'

# A middle config-restore failure must stop before binary/site/DB/runtime work.
CALLS=''
stop_traffic_and_writers() {
	CALLS="${CALLS}stop;"
	return 0
}
systemctl() {
	CALLS="${CALLS}systemctl:$*;"
	return 0
}
restore_regular_or_absent() {
	CALLS="${CALLS}regular:$2;"
	[[ "$2" != had-socket ]]
}
restore_binary_and_link() {
	CALLS="${CALLS}binary;"
	return 0
}
restore_site() {
	CALLS="${CALLS}site;"
	return 0
}
restore_database_if_snapshotted() {
	CALLS="${CALLS}db;"
	return 0
}
restore_runtime_state() {
	CALLS="${CALLS}runtime;"
	return 0
}
validate_restored_caddy() {
	CALLS="${CALLS}validate-caddy;"
	return 0
}
if perform_restore "${TEST_ROOT}"; then
	fail "perform_restore accepted a middle restore failure"
fi
assert_eq 'stop;systemctl:disable anselm-gateway.socket;regular:had-service;regular:had-socket;' \
	"${CALLS}" 'restore sequence must short-circuit'

# daemon-reload is itself a hard boundary; enable/start/readiness work must not
# proceed if systemd rejected the restored unit set.
# shellcheck source=deploy/rollback.sh
source "${SCRIPT_DIR}/rollback.sh"
CALLS=''
systemctl() {
	CALLS="${CALLS}systemctl:$*;"
	return 1
}
restore_enablement() {
	CALLS="${CALLS}enablement;"
	return 0
}
if restore_runtime_state "${TEST_ROOT}"; then
	fail "restore_runtime_state accepted daemon-reload failure"
fi
assert_eq 'systemctl:daemon-reload;' "${CALLS}" 'runtime restore must short-circuit at daemon-reload'

# A persistent-guard activation failure must stop before any state mutation.
CALLS=''
activate_deploy_guard() {
	CALLS="${CALLS}guard-on;"
	return 1
}
perform_restore() {
	CALLS="${CALLS}perform;"
	return 0
}
if restore_bundle "${TEST_ROOT}" automatic; then
	fail "restore_bundle accepted a deploy-guard activation failure"
fi
assert_eq 'guard-on;' "${CALLS}" 'state restore must not start without persistent guard'

# A failed state restore must keep the guard and never reach clear/reopen.
CALLS=''
activate_deploy_guard() {
	CALLS="${CALLS}guard-on;"
	return 0
}
clear_deploy_guard() {
	CALLS="${CALLS}guard-off;"
	return 0
}
perform_restore() {
	CALLS="${CALLS}perform;"
	return 1
}
reopen_restored_caddy() {
	CALLS="${CALLS}reopen;"
	return 0
}
if restore_bundle "${TEST_ROOT}" automatic; then
	fail "restore_bundle accepted a failed state restore"
fi
assert_eq 'guard-on;perform;' "${CALLS}" 'guard must remain after restore failure'
[[ -f "${TEST_ROOT}/RESTORE_FAILED" ]] || fail "restore failure marker missing"

# Even after the state copy succeeds, a durability failure must still prevent
# public ingress from reopening.
: >"${TEST_ROOT}/READY"
CALLS=''
perform_restore() {
	CALLS="${CALLS}perform;"
	return 0
}
sync() {
	CALLS="${CALLS}sync;"
	return 1
}
reopen_restored_caddy() {
	CALLS="${CALLS}reopen;"
	return 0
}
if restore_bundle "${TEST_ROOT}" manual; then
	fail "restore_bundle accepted a durability failure"
fi
assert_eq 'guard-on;perform;sync;' "${CALLS}" 'guard must remain before durable commit'

# A completed durable restore still must not reopen ingress when clearing the
# persistent marker fails.
CALLS=''
activate_deploy_guard() {
	CALLS="${CALLS}guard-on;"
	return 0
}
perform_restore() {
	CALLS="${CALLS}perform;"
	return 0
}
sync() {
	CALLS="${CALLS}sync;"
	return 0
}
clear_deploy_guard() {
	CALLS="${CALLS}guard-off;"
	return 1
}
reopen_restored_caddy() {
	CALLS="${CALLS}reopen;"
	return 0
}
if restore_bundle "${TEST_ROOT}" automatic; then
	fail "restore_bundle accepted a deploy-guard clear failure"
fi
assert_eq 'guard-on;perform;sync;guard-off;' "${CALLS}" 'Caddy must not reopen while guard remains'

# Once guard clear commits the restored state, a Caddy start failure must not
# replay perform_restore/DB restoration.
CALLS=''
clear_deploy_guard() {
	CALLS="${CALLS}guard-off;"
	return 0
}
reopen_restored_caddy() {
	CALLS="${CALLS}reopen;"
	return 1
}
if restore_bundle "${TEST_ROOT}" automatic; then
	fail "restore_bundle accepted a post-commit Caddy reopen failure"
fi
assert_eq 'guard-on;perform;sync;guard-off;reopen;' "${CALLS}" 'restored DB must not replay after guard clear'

# Each local readiness attempt has its own deadline; otherwise one accepted but
# stalled connection defeats the outer retry count forever.
CALLS=''
curl() {
	CALLS="$*"
	return 0
}
wait_ready || fail "bounded readiness stub unexpectedly failed"
assert_eq '-fsS --connect-timeout 1 --max-time 2 http://127.0.0.1:9090/readyz' \
	"${CALLS}" 'rollback readiness curl must carry connect and total deadlines'

# install.sh is intentionally not sourceable; assert both production probes use
# the same reviewed deadline contract.
grep -Fq 'curl -fsS --connect-timeout 1 --max-time 2 "${ADMIN_READYZ}"' \
	"${SCRIPT_DIR}/install.sh" || fail "installer readyz probe is not bounded"
grep -Fq 'curl -fsS --connect-timeout 1 --max-time 2 "${LOCAL_HEALTHZ}"' \
	"${SCRIPT_DIR}/install.sh" || fail "installer healthz probe is not bounded"

printf 'rollback failure-propagation tests OK\n'

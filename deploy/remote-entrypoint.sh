#!/usr/bin/env bash
# Trusted over the pinned SSH channel via stdin. Validate the entire unpredictable
# remote stage before executing any uploaded code, then always remove that stage.
set -euo pipefail
set +x
umask 077

LOCK_DIR=/run/anselm-gateway-deploy

die() {
	printf 'remote-entrypoint: %s\n' "$*" >&2
	exit 1
}

# Execute one command under a lock file that lives inside a root-only directory.
# Never open a predictable path directly in shared /run/lock: flock follows
# symlinks, which could otherwise let a local user redirect a root open/truncate.
with_deploy_lock() {
	sudo -n bash -c '
		set -euo pipefail
		lock_dir="$1"
		shift
		if [[ -e "$lock_dir" || -L "$lock_dir" ]]; then
			[[ -d "$lock_dir" && ! -L "$lock_dir" ]]
			[[ "$(stat -c "%u:%g:%a" "$lock_dir")" == 0:0:700 ]]
		else
			install -d -o root -g root -m 0700 "$lock_dir"
		fi
		lock="$lock_dir/lock"
		if [[ -e "$lock" || -L "$lock" ]]; then
			[[ -f "$lock" && ! -L "$lock" ]]
			[[ "$(stat -c "%u:%g:%a" "$lock")" == 0:0:600 ]]
		else
			install -o root -g root -m 0600 /dev/null "$lock"
		fi
		exec 9<>"$lock"
		flock -n 9
		exec "$@"
	' _ "${LOCK_DIR}" "$@"
}

[[ $# -eq 2 ]] || die "usage: remote-entrypoint STAGE MANIFEST_SHA256"
STAGE="$1"
EXPECTED_MANIFEST_SHA="$2"
[[ "${STAGE}" =~ ^/tmp/anselm-deploy\.[A-Za-z0-9]{10}$ ]] || die "unsafe stage path"
[[ "${EXPECTED_MANIFEST_SHA}" =~ ^[0-9a-f]{64}$ ]] || die "invalid manifest checksum"
[[ -d "${STAGE}" && ! -L "${STAGE}" ]] || die "stage is not a real directory"
[[ "$(stat -c '%u' "${STAGE}")" == "$(id -u)" ]] || die "stage owner mismatch"
[[ "$(stat -c '%a' "${STAGE}")" == 700 ]] || die "stage mode must be 0700"

cleanup() {
	local status=$?
	trap - EXIT HUP INT TERM
	# Serialize cleanup with the root installer/manual rollback lock. If an SSH
	# transport failure leaves the installer running, no second cleanup can remove
	# its staged rollback program or EnvironmentFile out from under it.
	if ! with_deploy_lock bash -c '
		set -euo pipefail
		stage="$1"
		[[ "$stage" =~ ^/tmp/anselm-deploy\.[A-Za-z0-9]{10}$ ]]
		[[ ! -e "$stage" || -d "$stage" && ! -L "$stage" ]]
		chmod -R u+rwX "$stage" >/dev/null 2>&1 || true
		rm -rf -- "$stage"
	' _ "${STAGE}" >/dev/null 2>&1; then
		# Lock contention before this stage's installer starts is harmless: the
		# unique stage has only deploy-user-owned upload files and can be removed
		# without touching the other deployment.
		if ! sudo -n test -e "${STAGE}/.installer-active"; then
			sudo -n chmod -R u+rwX "${STAGE}" >/dev/null 2>&1 || true
			sudo -n rm -rf -- "${STAGE}" >/dev/null 2>&1 || {
				chmod -R u+rwX "${STAGE}" >/dev/null 2>&1 || true
				rm -rf -- "${STAGE}" || true
			}
		fi
	fi
	exit "${status}"
}
trap cleanup EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

if find "${STAGE}" -type l -print -quit | grep -q .; then
	die "stage contains a symlink"
fi
if find "${STAGE}" ! -type d ! -type f -print -quit | grep -q .; then
	die "stage contains a special file"
fi
if find "${STAGE}" ! -uid "$(id -u)" -print -quit | grep -q .; then
	die "stage contains a path owned by another user"
fi

EXPECTED_FILES=$(printf '%s\n' \
	'Caddyfile' \
	'anselm-caddy-deploy-guard.conf' \
	'anselm-gateway' \
	'anselm-gateway.service' \
	'anselm-gateway.socket' \
	'gateway.env' \
	'install.sh' \
	'manifest.sha256' \
	'meta/acme-email' \
	'meta/gateway-domain' \
	'meta/reset-unlaunched-gateway-data' \
	'meta/sha' \
	'meta/site-domain' \
	'render-caddy.sh' \
	'rollback.sh' \
	'site/index.html' \
	'site/styles.css' | LC_ALL=C sort)
ACTUAL_FILES=$(find "${STAGE}" -mindepth 1 -type f -printf '%P\n' | LC_ALL=C sort)
[[ "${ACTUAL_FILES}" == "${EXPECTED_FILES}" ]] || die "stage file set differs from the reviewed payload"

EXPECTED_DIRS=$(printf '%s\n' meta site | LC_ALL=C sort)
ACTUAL_DIRS=$(find "${STAGE}" -mindepth 1 -type d -printf '%P\n' | LC_ALL=C sort)
[[ "${ACTUAL_DIRS}" == "${EXPECTED_DIRS}" ]] || die "stage directory set differs from the reviewed payload"

command -v flock >/dev/null 2>&1 || die "flock is required"
sudo -n true || die "passwordless sudo is required"
# Seal the structure-checked stage root:root before the first uploaded program is
# evaluated. Even another process under the deploy account can no longer swap a
# file in the verify→execute interval. chown -h never follows a raced symlink;
# the complete tree and hashes are rechecked as root after sealing.
sudo -n find "${STAGE}" -exec chown -h root:root {} +
sudo -n find "${STAGE}" -type d -exec chmod 0700 {} +
sudo -n find "${STAGE}" -type f -exec chmod 0600 {} +
if sudo -n find "${STAGE}" -type l -print -quit | grep -q .; then
	die "sealed stage contains a symlink"
fi
if sudo -n find "${STAGE}" ! -type d ! -type f -print -quit | grep -q .; then
	die "sealed stage contains a special file"
fi
ACTUAL_FILES=$(sudo -n find "${STAGE}" -mindepth 1 -type f -printf '%P\n' | LC_ALL=C sort)
[[ "${ACTUAL_FILES}" == "${EXPECTED_FILES}" ]] || die "sealed stage file set changed"
ACTUAL_DIRS=$(sudo -n find "${STAGE}" -mindepth 1 -type d -printf '%P\n' | LC_ALL=C sort)
[[ "${ACTUAL_DIRS}" == "${EXPECTED_DIRS}" ]] || die "sealed stage directory set changed"
ACTUAL_MANIFEST_SHA="$(sudo -n sha256sum "${STAGE}/manifest.sha256" | awk '{print $1}')"
[[ "${ACTUAL_MANIFEST_SHA}" == "${EXPECTED_MANIFEST_SHA}" ]] || die "manifest checksum mismatch"
sudo -n bash -c 'cd "$1" && sha256sum --strict -c manifest.sha256 >/dev/null' _ "${STAGE}" ||
	die "artifact checksum mismatch"

with_deploy_lock bash -c '
	set -euo pipefail
	stage="$1"
	install -o root -g root -m 0600 /dev/null "$stage/.installer-active"
	exec bash "$stage/install.sh" "$stage"
' _ "${STAGE}" || die "installer failed or another deploy/rollback holds the lock"

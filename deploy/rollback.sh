#!/usr/bin/env bash
# Schema-aware Anselm Gateway rollback.
#
# A deploy snapshot is one indivisible compatibility unit: SQLite main/WAL/SHM,
# binary, live symlink, EnvironmentFile, units, Caddyfile, static site, and the
# rollback entry that understands the retained bundle format. A binary-only
# downgrade is intentionally impossible because an older binary may reject (or
# misinterpret) a newer database schema.
set -euo pipefail
set +x
umask 077

ROLLBACK_ROOT=/var/lib/anselm-gateway-rollbacks
LOCK_DIR=/run/anselm-gateway-deploy
LOCK_PATH=${LOCK_DIR}/lock
DEPLOY_MARKER=/etc/anselm-gateway-deploy-in-progress
CADDY_GUARD_DIR=/etc/systemd/system/caddy.service.d
CADDY_GUARD_PATH=${CADDY_GUARD_DIR}/90-anselm-gateway-deploy-guard.conf
DATA_DIR=/var/lib/anselm-gateway
DB_PATH=${DATA_DIR}/anselm-gateway.db
BIN_DIR=/usr/local/bin
LINK=${BIN_DIR}/anselm-gateway
ENV_PATH=/etc/anselm-gateway.env
SERVICE_PATH=/etc/systemd/system/anselm-gateway.service
SOCKET_PATH=/etc/systemd/system/anselm-gateway.socket
CADDY_PATH=/etc/caddy/Caddyfile
SITE_PATH=/var/www/anselm-site
ROLLBACK_TOOL=/usr/local/sbin/anselm-gateway-rollback
ADMIN_READYZ=http://127.0.0.1:9090/readyz

die() {
	printf 'rollback: %s\n' "$*" >&2
	exit 1
}

log() {
	printf 'rollback: %s\n' "$*" >&2
}

deploy_guard_valid() {
	local content
	[[ -d "${CADDY_GUARD_DIR}" && ! -L "${CADDY_GUARD_DIR}" ]] || return 1
	[[ -f "${CADDY_GUARD_PATH}" && ! -L "${CADDY_GUARD_PATH}" ]] || return 1
	[[ "$(stat -c '%u:%g:%a' "${CADDY_GUARD_PATH}")" == 0:0:644 ]] || return 1
	content="$(<"${CADDY_GUARD_PATH}")" || return 1
	[[ "${content}" == $'[Unit]\nConditionPathExists=!/etc/anselm-gateway-deploy-in-progress' ]]
}

marker_bundle() {
	local content
	[[ -f "${DEPLOY_MARKER}" && ! -L "${DEPLOY_MARKER}" ]] || return 1
	[[ "$(stat -c '%u:%g:%a' "${DEPLOY_MARKER}")" == 0:0:600 ]] || return 1
	content="$(<"${DEPLOY_MARKER}")" || return 1
	[[ "${content}" =~ ^bundle=(/var/lib/anselm-gateway-rollbacks/bundle-[0-9a-f]{12}\.[A-Za-z0-9]{10})\ phase=(deploy|commit-ready|rollback)$ ]] || return 1
	printf '%s' "${BASH_REMATCH[1]}"
}

activate_deploy_guard() {
	local bundle="$1" phase="$2" existing='' tmp content
	deploy_guard_valid || return 1
	[[ "${bundle}" =~ ^/var/lib/anselm-gateway-rollbacks/bundle-[0-9a-f]{12}\.[A-Za-z0-9]{10}$ ]] || return 1
	[[ "${phase}" =~ ^(deploy|commit-ready|rollback)$ ]] || return 1
	if [[ -e "${DEPLOY_MARKER}" || -L "${DEPLOY_MARKER}" ]]; then
		existing="$(marker_bundle)" || return 1
		[[ "${existing}" == "${bundle}" ]] || return 1
	fi
	content="bundle=${bundle} phase=${phase}"
	tmp="${DEPLOY_MARKER}.tmp.$$"
	printf '%s' "${content}" >"${tmp}" || return 1
	chown root:root "${tmp}" || return 1
	chmod 0600 "${tmp}" || return 1
	mv -Tf "${tmp}" "${DEPLOY_MARKER}" || return 1
	sync || return 1
}

clear_deploy_guard() {
	local bundle="$1" content
	[[ -f "${DEPLOY_MARKER}" && ! -L "${DEPLOY_MARKER}" ]] || return 1
	[[ "$(stat -c '%u:%g:%a' "${DEPLOY_MARKER}")" == 0:0:600 ]] || return 1
	content="$(<"${DEPLOY_MARKER}")" || return 1
	[[ "${content}" == "bundle=${bundle} phase=rollback" ]] || return 1
	rm -f -- "${DEPLOY_MARKER}" || return 1
	sync || return 1
}

acquire_deploy_lock() {
	if [[ -e "${LOCK_DIR}" || -L "${LOCK_DIR}" ]]; then
		[[ -d "${LOCK_DIR}" && ! -L "${LOCK_DIR}" ]] || return 1
		[[ "$(stat -c '%u:%g:%a' "${LOCK_DIR}")" == 0:0:700 ]] || return 1
	else
		install -d -o root -g root -m 0700 "${LOCK_DIR}" || return 1
	fi
	if [[ -e "${LOCK_PATH}" || -L "${LOCK_PATH}" ]]; then
		[[ -f "${LOCK_PATH}" && ! -L "${LOCK_PATH}" ]] || return 1
		[[ "$(stat -c '%u:%g:%a' "${LOCK_PATH}")" == 0:0:600 ]] || return 1
	else
		install -o root -g root -m 0600 /dev/null "${LOCK_PATH}" || return 1
	fi
	exec 9<>"${LOCK_PATH}" || return 1
	flock -n 9
}

safe_bundle_path() {
	local path="$1"
	[[ "${path}" =~ ^/var/lib/anselm-gateway-rollbacks/bundle-[0-9a-f]{12}\.[A-Za-z0-9]{10}$ ]] || return 1
	[[ -d "${path}" && ! -L "${path}" ]] || return 1
	[[ "$(stat -c '%u' "${path}")" == 0 ]] || return 1
	[[ "$(stat -c '%a' "${path}")" == 700 ]] || return 1
}

validate_bundle() {
	local bundle="$1" kind="$2" db_part marker
	safe_bundle_path "${bundle}" || die "unsafe rollback bundle path: ${bundle}"
	[[ -f "${bundle}/state/snapshot-meta-complete" && ! -L "${bundle}/state/snapshot-meta-complete" ]] ||
		die "snapshot metadata is incomplete"
	if [[ "${kind}" == manual ]]; then
		[[ -f "${bundle}/READY" && ! -L "${bundle}/READY" ]] || die "bundle is not committed/ready"
		[[ -f "${bundle}/state/db-snapshot-complete" ]] || die "bundle has no complete database snapshot"
	fi
	if find "${bundle}" -type l -print -quit | grep -q .; then
		die "rollback bundle contains a symlink"
	fi
	if find "${bundle}" ! -type d ! -type f -print -quit | grep -q .; then
		die "rollback bundle contains a special file"
	fi
	if find "${bundle}" ! -user root -print -quit | grep -q .; then
		die "rollback bundle contains a non-root-owned path"
	fi
	[[ -f "${bundle}/SNAPSHOT.sha256" ]] || die "rollback bundle checksum manifest is missing"
	[[ -f "${bundle}/recovery/rollback.sh" && ! -L "${bundle}/recovery/rollback.sh" ]] ||
		die "rollback bundle recovery program is missing"
	if grep -Eq '  (\.\./|/|.*[/]\.\./)' "${bundle}/SNAPSHOT.sha256"; then
		die "rollback bundle checksum manifest contains an unsafe path"
	fi
	grep -Eq '^[0-9a-f]{64}  \./recovery/rollback\.sh$' "${bundle}/SNAPSHOT.sha256" ||
		die "rollback recovery program is not checksummed"
	(cd "${bundle}" && sha256sum --strict -c SNAPSHOT.sha256 >/dev/null) ||
		die "rollback bundle checksum verification failed"
	if [[ -f "${bundle}/state/had-rollback-tool" ]]; then
		[[ -f "${bundle}/files/anselm-gateway-rollback" && ! -L "${bundle}/files/anselm-gateway-rollback" ]] ||
			die "rollback entry snapshot is missing"
		grep -Eq '^[0-9a-f]{64}  \./state/had-rollback-tool$' "${bundle}/SNAPSHOT.sha256" ||
			die "rollback entry presence marker is not checksummed"
		grep -Eq '^[0-9a-f]{64}  \./files/anselm-gateway-rollback$' "${bundle}/SNAPSHOT.sha256" ||
			die "rollback entry snapshot is not checksummed"
	elif [[ -e "${bundle}/files/anselm-gateway-rollback" ]]; then
		die "rollback entry snapshot exists without its presence marker"
	fi
	if [[ -f "${bundle}/state/db-snapshot-complete" ]]; then
		# Completion is published only after these files and their checksum entries
		# are durable. Reject an older/partial manifest instead of restoring DB
		# bytes that were merely left beside a valid metadata-only manifest.
		for db_part in main wal shm; do
			marker="had-db-${db_part}"
			if [[ -f "${bundle}/state/${marker}" ]]; then
				[[ -f "${bundle}/db/${db_part}" ]] || die "database snapshot is missing ${db_part}"
				grep -Eq "^[0-9a-f]{64}  \\./state/${marker}$" "${bundle}/SNAPSHOT.sha256" ||
					die "database marker ${marker} is not checksummed"
				grep -Eq "^[0-9a-f]{64}  \\./db/${db_part}$" "${bundle}/SNAPSHOT.sha256" ||
					die "database snapshot ${db_part} is not checksummed"
			elif [[ -e "${bundle}/db/${db_part}" ]]; then
				die "database snapshot ${db_part} exists without its presence marker"
			fi
		done
	fi
}

unit_state() {
	systemctl is-active "$1" 2>/dev/null || true
}

stop_and_confirm() {
	local unit="$1" state i
	systemctl stop "${unit}" >/dev/null 2>&1 || true
	for i in $(seq 1 30); do
		state="$(unit_state "${unit}")"
		case "${state}" in
			inactive | failed | unknown) return 0 ;;
		esac
		sleep 1
	done
	log "${unit} did not stop (state=$(unit_state "${unit}"))"
	return 1
}

stop_traffic_and_writers() {
	# Public ingress first, then the socket that can queue new requests, then the
	# process owning SQLite. Nothing may write between DB restore and service gate.
	stop_and_confirm caddy || return 1
	stop_and_confirm anselm-gateway.socket || return 1
	stop_and_confirm anselm-gateway.service || return 1
	return 0
}

restore_regular_or_absent() {
	local bundle="$1" marker="$2" saved="$3" target="$4" mode="$5"
	if [[ -f "${bundle}/state/${marker}" ]]; then
		[[ -f "${bundle}/files/${saved}" && ! -L "${bundle}/files/${saved}" ]] ||
			return 1
		install -o root -g root -m "${mode}" "${bundle}/files/${saved}" "${target}" || return 1
	else
		[[ ! -d "${target}" ]] || return 1
		rm -f -- "${target}" || return 1
	fi
	return 0
}

restore_rollback_tool() {
	local bundle="$1" tmp="${ROLLBACK_TOOL}.restore.$$"
	rm -f -- "${tmp}" || return 1
	if [[ -f "${bundle}/state/had-rollback-tool" ]]; then
		[[ -f "${bundle}/files/anselm-gateway-rollback" && ! -L "${bundle}/files/anselm-gateway-rollback" ]] ||
			return 1
		# Replace the directory entry atomically. The currently running shell may
		# itself be the global tool, so truncating that inode in place is unsafe.
		if ! install -o root -g root -m 0750 "${bundle}/files/anselm-gateway-rollback" "${tmp}"; then
			rm -f -- "${tmp}" || true
			return 1
		fi
		if ! mv -Tf "${tmp}" "${ROLLBACK_TOOL}"; then
			rm -f -- "${tmp}" || true
			return 1
		fi
	else
		[[ ! -d "${ROLLBACK_TOOL}" ]] || return 1
		rm -f -- "${ROLLBACK_TOOL}" || return 1
	fi
	return 0
}

restore_binary_and_link() {
	local bundle="$1" resolved raw tmp
	rm -f -- "${LINK}.tmp" || return 1
	if [[ -f "${bundle}/state/link-symlink" ]]; then
		[[ -f "${bundle}/state/link-resolved-target" && ! -L "${bundle}/state/link-resolved-target" ]] || return 1
		[[ -f "${bundle}/state/link-raw-target" && ! -L "${bundle}/state/link-raw-target" ]] || return 1
		resolved="$(<"${bundle}/state/link-resolved-target")" || return 1
		raw="$(<"${bundle}/state/link-raw-target")" || return 1
		[[ "${resolved}" =~ ^/usr/local/bin/anselm-gateway-[0-9a-f]{12}$ ]] || return 1
		[[ "${raw}" =~ ^(/usr/local/bin/)?anselm-gateway-[0-9a-f]{12}$ ]] || return 1
		[[ -f "${bundle}/files/anselm-gateway" ]] || return 1
		tmp="${resolved}.restore.$$"
		install -o root -g root -m 0755 "${bundle}/files/anselm-gateway" "${tmp}" || return 1
		mv -Tf "${tmp}" "${resolved}" || return 1
		ln -s "${raw}" "${LINK}.tmp" || return 1
		mv -Tf "${LINK}.tmp" "${LINK}" || return 1
	elif [[ -f "${bundle}/state/link-regular" ]]; then
		[[ -f "${bundle}/files/anselm-gateway" ]] || return 1
		tmp="${LINK}.restore.$$"
		install -o root -g root -m 0755 "${bundle}/files/anselm-gateway" "${tmp}" || return 1
		mv -Tf "${tmp}" "${LINK}" || return 1
	elif [[ -f "${bundle}/state/link-absent" ]]; then
		[[ ! -d "${LINK}" ]] || return 1
		rm -f -- "${LINK}" || return 1
	else
		return 1
	fi
	return 0
}

restore_site() {
	local bundle="$1"
	[[ "${SITE_PATH}" == /var/www/anselm-site ]] || return 1
	rm -rf -- "${SITE_PATH}" || return 1
	if [[ -f "${bundle}/state/had-site" ]]; then
		[[ -d "${bundle}/files/site" && ! -L "${bundle}/files/site" ]] || return 1
		mkdir -p "$(dirname "${SITE_PATH}")" || return 1
		cp -a "${bundle}/files/site" "${SITE_PATH}" || return 1
		find "${SITE_PATH}" -exec chown root:root {} + || return 1
	fi
	return 0
}

restore_db_file() {
	local bundle="$1" marker="$2" saved="$3" target="$4"
	if [[ -f "${bundle}/state/${marker}" ]]; then
		[[ -f "${bundle}/db/${saved}" && ! -L "${bundle}/db/${saved}" ]] || return 1
		install -o anselm -g anselm -m 0600 "${bundle}/db/${saved}" "${target}" || return 1
	fi
	return 0
}

restore_database_if_snapshotted() {
	local bundle="$1"
	[[ -f "${bundle}/state/db-snapshot-complete" ]] || return 0
	[[ "${DATA_DIR}" == /var/lib/anselm-gateway && "${DB_PATH}" == /var/lib/anselm-gateway/anselm-gateway.db ]] ||
		return 1
	[[ ! -L "${DATA_DIR}" ]] || return 1
	install -d -o anselm -g anselm -m 0700 "${DATA_DIR}" || return 1
	# The service and socket are confirmed stopped before these exact files are
	# removed. Restoring the main file without its matching WAL can lose committed
	# rows, so the three files are always treated as a set.
	rm -f -- "${DB_PATH}" "${DB_PATH}-wal" "${DB_PATH}-shm" || return 1
	restore_db_file "${bundle}" had-db-main main "${DB_PATH}" || return 1
	restore_db_file "${bundle}" had-db-wal wal "${DB_PATH}-wal" || return 1
	restore_db_file "${bundle}" had-db-shm shm "${DB_PATH}-shm" || return 1
	chmod 0700 "${DATA_DIR}" || return 1
	for file in "${DB_PATH}" "${DB_PATH}-wal" "${DB_PATH}-shm"; do
		if [[ -e "${file}" ]]; then
			[[ -f "${file}" && ! -L "${file}" ]] || return 1
			chown anselm:anselm "${file}" || return 1
			chmod 0600 "${file}" || return 1
		fi
	done
	sync || return 1
	return 0
}

disable_socket_enablement() {
	local wants_link=/etc/systemd/system/sockets.target.wants/anselm-gateway.socket
	# systemctl may report failure when the old unit was absent (first deploy).
	# Removing the one exact WantedBy symlink is the authoritative cleanup.
	systemctl disable anselm-gateway.socket >/dev/null 2>&1 || true
	if [[ -e "${wants_link}" || -L "${wants_link}" ]]; then
		rm -f -- "${wants_link}" || return 1
	fi
	return 0
}

restore_enablement() {
	local bundle="$1"
	# Disable first to remove enablement added by a failed first deploy. The old
	# unit is already restored (or removed); daemon-reload makes that state real.
	disable_socket_enablement || return 1
	if [[ -f "${bundle}/state/socket-enabled" && -f "${bundle}/state/had-socket" ]]; then
		systemctl enable anselm-gateway.socket >/dev/null || return 1
	elif [[ -f "${bundle}/state/socket-enabled-runtime" && -f "${bundle}/state/had-socket" ]]; then
		systemctl enable --runtime anselm-gateway.socket >/dev/null || return 1
	fi
	return 0
}

wait_ready() {
	local i
	for i in $(seq 1 20); do
		if curl -fsS --connect-timeout 1 --max-time 2 "${ADMIN_READYZ}" >/dev/null 2>&1; then
			return 0
		fi
		sleep 3
	done
	return 1
}

restore_runtime_state() {
	local bundle="$1"
	systemctl daemon-reload || return 1
	restore_enablement "${bundle}" || return 1

	if [[ -f "${bundle}/state/socket-active" ]]; then
		systemctl start anselm-gateway.socket || return 1
	fi
	if [[ -f "${bundle}/state/service-active" ]]; then
		systemctl start anselm-gateway.service || return 1
		wait_ready || return 1
	fi
	return 0
}

validate_restored_caddy() {
	local bundle="$1"
	if [[ -f "${bundle}/state/caddy-active" ]]; then
		caddy validate --config "${CADDY_PATH}" --adapter caddyfile >/dev/null || return 1
	fi
	return 0
}

reopen_restored_caddy() {
	local bundle="$1"
	if [[ -f "${bundle}/state/caddy-active" ]]; then
		systemctl start caddy || return 1
		[[ "$(unit_state caddy)" == active ]] || return 1
	fi
	return 0
}

perform_restore() {
	local bundle="$1"
	stop_traffic_and_writers || return 1
	# Remove enablement while the newly installed unit still exists; this also
	# cleans a first deploy's wants/ symlink before an absent old unit is restored.
	disable_socket_enablement || return 1

	# Restore configuration and content while no listener can admit traffic.
	restore_regular_or_absent "${bundle}" had-service anselm-gateway.service "${SERVICE_PATH}" 0644 || return 1
	restore_regular_or_absent "${bundle}" had-socket anselm-gateway.socket "${SOCKET_PATH}" 0644 || return 1
	restore_regular_or_absent "${bundle}" had-caddy Caddyfile "${CADDY_PATH}" 0644 || return 1
	restore_regular_or_absent "${bundle}" had-env anselm-gateway.env "${ENV_PATH}" 0600 || return 1
	restore_binary_and_link "${bundle}" || return 1
	restore_site "${bundle}" || return 1
	restore_database_if_snapshotted "${bundle}" || return 1
	restore_runtime_state "${bundle}" || return 1
	validate_restored_caddy "${bundle}" || return 1
	# Restore the old dispatcher last. If an earlier compatibility component
	# fails, the currently installed new dispatcher can still recover this bundle.
	restore_rollback_tool "${bundle}" || return 1
	return 0
}

latest_ready_bundle() {
	local item newest=''
	shopt -s nullglob
	for item in "${ROLLBACK_ROOT}"/bundle-*; do
		[[ -d "${item}" && ! -L "${item}" && -f "${item}/READY" ]] || continue
		if [[ -z "${newest}" || "${item}" -nt "${newest}" ]]; then
			newest="${item}"
		fi
	done
	printf '%s' "${newest}"
}

restore_bundle() {
	local bundle="$1" mode="$2" marker
	# Persist the root-filesystem ingress inhibit before the first stop/copy. It
	# survives process death and host reboot; every failure below deliberately
	# leaves it present so Caddy cannot boot a half-restored compatibility set.
	if ! activate_deploy_guard "${bundle}" rollback; then
		printf '%s\n' "could not activate persistent Caddy deploy guard" >"${bundle}/RESTORE_FAILED" || true
		chmod 0600 "${bundle}/RESTORE_FAILED" || true
		return 1
	fi
	if ! perform_restore "${bundle}"; then
		printf '%s\n' "restore failed; snapshot deliberately retained for recovery" >"${bundle}/RESTORE_FAILED" || true
		chmod 0600 "${bundle}/RESTORE_FAILED" || true
		return 1
	fi

	# Consume the bundle while ingress is still stopped. If the script is killed
	# after Caddy starts, the same snapshot cannot be selected again and rewind rows
	# accepted in that post-restore window.
	if [[ "${mode}" == manual ]]; then
		marker=CONSUMED
		printf '%s\n' "manual restore committed at $(date -u +%FT%TZ)" >"${bundle}/${marker}" || return 1
	else
		marker=AUTO_RESTORED
		printf '%s\n' "automatic restore committed at $(date -u +%FT%TZ)" >"${bundle}/${marker}" || return 1
	fi
	chmod 0600 "${bundle}/${marker}" || return 1
	rm -f -- "${bundle}/READY" || return 1
	sync || return 1

	# Public ingress is reopened only after the DB/binary/config unit is healthy,
	# the snapshot has been consumed, and all of that state is durable. Clearing
	# the marker is the commit boundary: after it, recovery must never replay DB.
	if ! clear_deploy_guard "${bundle}"; then
		printf '%s\n' "restore complete but persistent Caddy guard could not be cleared" >"${bundle}/RESTORE_FAILED" || true
		chmod 0600 "${bundle}/RESTORE_FAILED" || true
		return 1
	fi

	# A Caddy start failure after marker removal must not replay the destructive DB
	# restore; it records the committed restored state for an operator instead.
	if ! reopen_restored_caddy "${bundle}"; then
		printf '%s\n' "restore committed but Caddy failed to reopen" >"${bundle}/RESTORE_FAILED" || true
		chmod 0600 "${bundle}/RESTORE_FAILED" || true
		return 1
	fi
	return 0
}

rollback_main() {
	local mode=manual mode_flag=0 bundle='' assume_yes=0 answer validate_kind
	[[ ${EUID} -eq 0 ]] || die "must run as root"

	while [[ $# -gt 0 ]]; do
		case "$1" in
		--automatic)
			[[ ${mode_flag} -eq 0 ]] || die "rollback modes are mutually exclusive"
			mode=automatic
			mode_flag=1
			shift
			;;
		--recover-incomplete)
			[[ ${mode_flag} -eq 0 ]] || die "rollback modes are mutually exclusive"
			mode=recovery
			mode_flag=1
			shift
			;;
		--bundle)
			[[ $# -ge 2 ]] || die "--bundle requires a path"
			bundle="$2"
			shift 2
			;;
		--yes)
			assume_yes=1
			shift
			;;
		*) die "unknown argument: $1" ;;
		esac
	done

	if [[ "${mode}" == automatic ]]; then
		[[ -n "${bundle}" ]] || die "automatic rollback requires --bundle"
	else
		command -v flock >/dev/null 2>&1 || die "flock is required"
		acquire_deploy_lock || die "another deploy/rollback is running or the root-owned lock is unsafe"
		[[ -z "${bundle}" ]] || die "--bundle is reserved for internal automatic rollback"
		if [[ "${mode}" == recovery ]]; then
			bundle="$(marker_bundle)" || die "no valid interrupted-deployment marker is available"
		else
			[[ ! -e "${DEPLOY_MARKER}" && ! -L "${DEPLOY_MARKER}" ]] ||
				die "an interrupted deployment exists; use --recover-incomplete first"
			bundle="$(latest_ready_bundle)"
			[[ -n "${bundle}" ]] || die "no committed schema-aware rollback bundle is available"
		fi
		log "this restores DB + binary + env + units + Caddy + static site + rollback entry from $(basename "${bundle}")"
		log "all requests committed after that snapshot will be discarded"
		if [[ ${assume_yes} -ne 1 ]]; then
			[[ -t 0 ]] || die "non-interactive use requires --yes"
			printf 'Type RESTORE to continue: ' >&2
			IFS= read -r answer
			[[ "${answer}" == RESTORE ]] || die "confirmation not received"
		fi
	fi

	validate_kind="${mode}"
	[[ "${mode}" == recovery ]] && validate_kind=automatic
	validate_bundle "${bundle}" "${validate_kind}"
	log "blocking public traffic and restoring $(basename "${bundle}")"
	restore_bundle "${bundle}" "${validate_kind}" ||
		die "restore incomplete; verify ingress/service state immediately; snapshot retained at ${bundle}"
	log "schema-aware ${mode} restore complete"
}

if [[ "${BASH_SOURCE[0]}" == "$0" ]]; then
	rollback_main "$@"
fi

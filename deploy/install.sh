#!/usr/bin/env bash
# Remote production installer. The caller has already authenticated the host and
# verified the staged payload manifest. This script adds the stateful guarantee:
# no public traffic can arrive between the SQLite snapshot and the commit point.
set -euo pipefail
set +x
umask 077

ROLLBACK_ROOT=/var/lib/anselm-gateway-rollbacks
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
DEPLOY_MARKER=/etc/anselm-gateway-deploy-in-progress
CADDY_GUARD_DIR=/etc/systemd/system/caddy.service.d
CADDY_GUARD_PATH=${CADDY_GUARD_DIR}/90-anselm-gateway-deploy-guard.conf
ADMIN_READYZ=http://127.0.0.1:9090/readyz
LOCAL_HEALTHZ=http://127.0.0.1:8080/healthz

die() {
	printf 'install: %s\n' "$*" >&2
	exit 1
}

log() {
	printf 'install: %s\n' "$*"
}

# Deployment failures are reported in the CI job, which is the only practical
# place to diagnose a remote systemd startup failure.  Keep that useful without
# turning the job log into a secret-exfiltration channel: service output and the
# short, current-boot journal excerpt are passed through this conservative
# redactor before they leave the host.
redact_diagnostics() {
	sed -E \
		-e 's/([Aa]uthorization:[[:space:]]*[Bb]earer)[[:space:]]+[^[:space:]]+/\1 [REDACTED]/g' \
		-e 's/([Xx]-[Aa][Pp][Ii]-[Kk][Ee][Yy][[:space:]]*:[[:space:]]*)[^[:space:]]+/\1[REDACTED]/g' \
		-e 's/([A-Za-z_]*[Aa][Pp][Ii]_[Kk][Ee][Yy][A-Za-z_]*[[:space:]]*=[[:space:]]*)[^[:space:]]+/\1[REDACTED]/g' \
		-e 's/([A-Za-z_]*[Ss][Ee][Cc][Rr][Ee][Tt][A-Za-z_]*[[:space:]]*=[[:space:]]*)[^[:space:]]+/\1[REDACTED]/g' \
		-e 's/(sk-[A-Za-z0-9._-]{8,})/[REDACTED]/g'
}

diagnose_gateway_start_failure() {
	log "anselm-gateway.service failed to start; sanitized diagnostics follow"
	{
		sudo systemctl show anselm-gateway.service \
			-p Result -p ExecMainCode -p ExecMainStatus -p ActiveState -p SubState || true
		sudo systemctl status anselm-gateway.service --no-pager --full -n 30 || true
		sudo journalctl -u anselm-gateway.service --boot --no-pager -o cat -n 80 || true
	} 2>&1 | redact_diagnostics
}

[[ $# -eq 1 ]] || die "usage: $0 VERIFIED_STAGE"
STAGE="$1"
[[ "${STAGE}" =~ ^/tmp/anselm-deploy\.[A-Za-z0-9]{10}$ ]] || die "unsafe stage path"
[[ -d "${STAGE}" && ! -L "${STAGE}" ]] || die "stage is not a directory"
[[ "$(stat -c '%a' "${STAGE}")" == 700 ]] || die "stage must have mode 0700"

read_meta() {
	local name="$1" path="${STAGE}/meta/$1" value
	[[ -f "${path}" && ! -L "${path}" ]] || die "missing metadata: ${name}"
	value="$(<"${path}")"
	[[ "${value}" != *$'\n'* && "${value}" != *$'\r'* ]] || die "invalid metadata: ${name}"
	printf '%s' "${value}"
}

GATEWAY_DOMAIN="$(read_meta gateway-domain)"
SITE_DOMAIN="$(read_meta site-domain)"
ACME_EMAIL="$(read_meta acme-email)"
RESET_UNLAUNCHED_GATEWAY_DATA="$(read_meta reset-unlaunched-gateway-data)"
SHA="$(read_meta sha)"
[[ "${GATEWAY_DOMAIN}" =~ ^[A-Za-z0-9.-]+$ ]] || die "invalid gateway domain"
[[ "${SITE_DOMAIN}" =~ ^[A-Za-z0-9.-]+$ ]] || die "invalid site domain"
[[ "${SHA}" =~ ^[0-9a-f]{12}$ ]] || die "invalid SHA"
[[ -n "${ACME_EMAIL}" ]] || die "ACME email is empty"
[[ "${RESET_UNLAUNCHED_GATEWAY_DATA}" == 0 || "${RESET_UNLAUNCHED_GATEWAY_DATA}" == 1 ]] ||
	die "invalid reset-unlaunched-gateway-data flag"

for required in anselm-gateway Caddyfile anselm-caddy-deploy-guard.conf \
	anselm-gateway.service anselm-gateway.socket render-caddy.sh rollback.sh gateway.env; do
	[[ -f "${STAGE}/${required}" && ! -L "${STAGE}/${required}" ]] ||
		die "required staged file is invalid: ${required}"
done

sudo -n true || die "passwordless sudo is required"
for command in caddy cmp curl getent groupadd sha256sum systemctl useradd; do
	command -v "${command}" >/dev/null 2>&1 || die "required command is missing: ${command}"
done

# The old deployment is only safely snapshot-able if it uses the production DB
# path this installer knows how to restore. Missing means the binary default,
# which resolves to the same path under the unit's WorkingDirectory.
if sudo test -f "${ENV_PATH}"; then
	OLD_DB_LINES="$(sudo awk '/^[[:space:]]*GATEWAY_DB_PATH[[:space:]]*=/{print}' "${ENV_PATH}")"
	OLD_DB_COUNT="$(printf '%s\n' "${OLD_DB_LINES}" | awk 'NF{n++} END{print n+0}')"
	[[ "${OLD_DB_COUNT}" -le 1 ]] || die "live EnvironmentFile has duplicate GATEWAY_DB_PATH"
	if [[ "${OLD_DB_COUNT}" -eq 1 ]]; then
		case "${OLD_DB_LINES}" in
			GATEWAY_DB_PATH=/var/lib/anselm-gateway/anselm-gateway.db | \
			GATEWAY_DB_PATH=\"/var/lib/anselm-gateway/anselm-gateway.db\" | \
			GATEWAY_DB_PATH=\'/var/lib/anselm-gateway/anselm-gateway.db\') ;;
			*) die "live GATEWAY_DB_PATH is not the supported production path" ;;
		esac
	fi
fi
if sudo test -e "${DATA_DIR}" || sudo test -L "${DATA_DIR}"; then
	sudo test -d "${DATA_DIR}" && ! sudo test -L "${DATA_DIR}" || die "data directory is not a real directory"
fi
for db_file in "${DB_PATH}" "${DB_PATH}-wal" "${DB_PATH}-shm"; do
	if sudo test -e "${db_file}" || sudo test -L "${db_file}"; then
		sudo test -f "${db_file}" && ! sudo test -L "${db_file}" || die "unsafe SQLite path: ${db_file}"
	fi
done

# Render and validate Caddy before touching the running deployment. The renderer
# owns literal-placeholder quoting and is directly regression-tested.
WORK="${STAGE}/work"
mkdir -m 0700 "${WORK}"
bash "${STAGE}/render-caddy.sh" \
	"${STAGE}/Caddyfile" "${WORK}/Caddyfile" \
	"${GATEWAY_DOMAIN}" "${SITE_DOMAIN}" "${ACME_EMAIL}"
caddy validate --config "${WORK}/Caddyfile" --adapter caddyfile >/dev/null

if sudo test -e "${ROLLBACK_ROOT}" || sudo test -L "${ROLLBACK_ROOT}"; then
	sudo test -d "${ROLLBACK_ROOT}" && ! sudo test -L "${ROLLBACK_ROOT}" ||
		die "rollback root is not a real directory"
	[[ "$(sudo stat -c '%u:%g:%a' "${ROLLBACK_ROOT}")" == 0:0:700 ]] ||
		die "rollback root must be root:root mode 0700"
else
	sudo install -d -o root -g root -m 0700 "${ROLLBACK_ROOT}"
fi

# A persisted marker means a previous transition was interrupted across reboot.
# The process flock lives on tmpfs and cannot prove otherwise after boot, so a
# new deployment must not create a second bundle or overwrite recovery state.
if sudo test -e "${DEPLOY_MARKER}" || sudo test -L "${DEPLOY_MARKER}"; then
	die "unfinished deployment marker exists at ${DEPLOY_MARKER}; recover it before deploying again"
fi

# Install the permanent Caddy crash guard before a transition marker can ever be
# published. The drop-in remains installed after success; with no marker its
# negative condition is inert. Existing bytes must match exactly, so a same-name
# local policy is never silently overwritten. The global rollback entry is
# switched later, only after its previous bytes/absence are durably snapshotted.
if sudo test -e "${CADDY_GUARD_DIR}" || sudo test -L "${CADDY_GUARD_DIR}"; then
	sudo test -d "${CADDY_GUARD_DIR}" && ! sudo test -L "${CADDY_GUARD_DIR}" ||
		die "Caddy guard directory is unsafe"
	GUARD_DIR_STAT="$(sudo stat -c '%u:%g:%a' "${CADDY_GUARD_DIR}")"
	[[ "${GUARD_DIR_STAT}" =~ ^0:0:([0-7]{3,4})$ ]] || die "Caddy guard directory must be root-owned"
	GUARD_DIR_MODE="${BASH_REMATCH[1]}"
	(( (8#${GUARD_DIR_MODE} & 0022) == 0 )) || die "Caddy guard directory must not be group/world writable"
else
	sudo install -d -o root -g root -m 0755 "${CADDY_GUARD_DIR}"
fi
if sudo test -e "${CADDY_GUARD_PATH}" || sudo test -L "${CADDY_GUARD_PATH}"; then
	sudo test -f "${CADDY_GUARD_PATH}" && ! sudo test -L "${CADDY_GUARD_PATH}" ||
		die "Caddy deploy guard is unsafe"
	[[ "$(sudo stat -c '%u:%g:%a' "${CADDY_GUARD_PATH}")" == 0:0:644 ]] ||
		die "Caddy deploy guard must be root:root mode 0644"
	sudo cmp -s "${STAGE}/anselm-caddy-deploy-guard.conf" "${CADDY_GUARD_PATH}" ||
		die "installed Caddy deploy guard differs from the reviewed policy"
else
	GUARD_TMP="${CADDY_GUARD_PATH}.install.$$"
	sudo install -o root -g root -m 0644 "${STAGE}/anselm-caddy-deploy-guard.conf" "${GUARD_TMP}"
	sudo mv -Tf "${GUARD_TMP}" "${CADDY_GUARD_PATH}"
fi
sudo sync
sudo systemctl daemon-reload

BUNDLE="$(sudo mktemp -d "${ROLLBACK_ROOT}/bundle-${SHA}.XXXXXXXXXX")"
[[ "${BUNDLE}" =~ ^/var/lib/anselm-gateway-rollbacks/bundle-[0-9a-f]{12}\.[A-Za-z0-9]{10}$ ]] ||
	die "mktemp returned an unsafe rollback path"
sudo chown root:root "${BUNDLE}"
sudo chmod 0700 "${BUNDLE}"
sudo install -d -o root -g root -m 0700 \
	"${BUNDLE}/state" "${BUNDLE}/files" "${BUNDLE}/db" "${BUNDLE}/recovery"
sudo install -o root -g root -m 0500 "${STAGE}/rollback.sh" "${BUNDLE}/recovery/rollback.sh"

MUTATED=0
COMMITTED=0
RESTORE_FAILED=0
ROLLBACK_TOOL_TMP="${ROLLBACK_TOOL}.install.$$"

safe_remove_candidate() {
	[[ "${BUNDLE}" =~ ^/var/lib/anselm-gateway-rollbacks/bundle-[0-9a-f]{12}\.[A-Za-z0-9]{10}$ ]] || return 1
	sudo test -d "${BUNDLE}" && ! sudo test -L "${BUNDLE}" || return 1
	sudo rm -rf -- "${BUNDLE}"
}

on_exit() {
	local status=$?
	trap - EXIT HUP INT TERM
	if [[ ${status} -ne 0 && ${COMMITTED} -eq 0 ]]; then
		if [[ ${MUTATED} -eq 1 ]]; then
			log "deploy failed before commit; restoring the complete pre-deploy snapshot"
			# Run the recovery program belonging to this exact bundle. The global
			# entry is itself part of the compatibility snapshot and may be between
			# versions (or absent on first deploy) at this failure boundary.
			if sudo "${BUNDLE}/recovery/rollback.sh" --automatic --bundle "${BUNDLE}"; then
				safe_remove_candidate || true
			else
				RESTORE_FAILED=1
				log "automatic restore failed; snapshot retained root-only at ${BUNDLE}"
			fi
		else
			safe_remove_candidate || true
		fi
	fi
	sudo rm -f -- "${ROLLBACK_TOOL_TMP}" >/dev/null 2>&1 || true
	if [[ ${RESTORE_FAILED} -eq 1 ]]; then
		log "AUTOMATIC RESTORE FAILED; verify ingress/service state immediately; manual recovery is required"
	fi
	exit "${status}"
}
trap on_exit EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM

mark() {
	sudo install -o root -g root -m 0600 /dev/null "${BUNDLE}/state/$1"
}

write_state() {
	local name="$1" value="$2"
	[[ "${value}" != *$'\n'* && "${value}" != *$'\r'* ]] || return 1
	printf '%s' "${value}" | sudo tee "${BUNDLE}/state/${name}" >/dev/null
	sudo chown root:root "${BUNDLE}/state/${name}"
	sudo chmod 0600 "${BUNDLE}/state/${name}"
}

publish_transition_marker() {
	local phase="$1" tmp="${DEPLOY_MARKER}.tmp.$$" content
	[[ "${phase}" =~ ^(deploy|commit-ready)$ ]] || return 1
	content="bundle=${BUNDLE} phase=${phase}"
	printf '%s' "${content}" | sudo tee "${tmp}" >/dev/null
	sudo chown root:root "${tmp}"
	sudo chmod 0600 "${tmp}"
	sudo mv -Tf "${tmp}" "${DEPLOY_MARKER}"
}

clear_transition_marker() {
	local content
	sudo test -f "${DEPLOY_MARKER}" && ! sudo test -L "${DEPLOY_MARKER}" || return 1
	content="$(sudo cat "${DEPLOY_MARKER}")"
	[[ "${content}" == "bundle=${BUNDLE} phase=commit-ready" ]] || return 1
	sudo rm -f -- "${DEPLOY_MARKER}"
}

snapshot_regular() {
	local source="$1" saved="$2" marker="$3"
	if sudo test -e "${source}" || sudo test -L "${source}"; then
		sudo test -f "${source}" && ! sudo test -L "${source}" || die "unsafe live file: ${source}"
		sudo install -o root -g root -m 0600 "${source}" "${BUNDLE}/files/${saved}"
		mark "${marker}"
	fi
}

record_active() {
	local unit="$1" marker="$2" state
	state="$(systemctl is-active "${unit}" 2>/dev/null || true)"
	case "${state}" in
	active) mark "${marker}" ;;
	inactive | failed | unknown) return 0 ;;
	*) die "cannot snapshot ${unit} runtime state (${state:-no state})" ;;
	esac
}

# Capture every non-DB object before the first stop. The state markers distinguish
# "absent before deploy" from "copy failed" so first deploy rollback is exact.
snapshot_regular "${SERVICE_PATH}" anselm-gateway.service had-service
snapshot_regular "${SOCKET_PATH}" anselm-gateway.socket had-socket
snapshot_regular "${CADDY_PATH}" Caddyfile had-caddy
snapshot_regular "${ENV_PATH}" anselm-gateway.env had-env
snapshot_regular "${ROLLBACK_TOOL}" anselm-gateway-rollback had-rollback-tool

if sudo test -e "${SITE_PATH}" || sudo test -L "${SITE_PATH}"; then
	sudo test -d "${SITE_PATH}" && ! sudo test -L "${SITE_PATH}" || die "unsafe static site path"
	[[ -z "$(sudo find "${SITE_PATH}" -type l -print -quit)" ]] || die "static site contains a symlink"
	[[ -z "$(sudo find "${SITE_PATH}" ! -type d ! -type f -print -quit)" ]] || die "static site contains a special file"
	sudo cp -a "${SITE_PATH}" "${BUNDLE}/files/site"
	sudo find "${BUNDLE}/files/site" -exec chown root:root {} +
	mark had-site
fi

if sudo test -L "${LINK}"; then
	PREV_RAW="$(sudo readlink "${LINK}")"
	PREV_RESOLVED="$(sudo readlink -f "${LINK}")"
	[[ "${PREV_RAW}" =~ ^(/usr/local/bin/)?anselm-gateway-[0-9a-f]{12}$ ]] ||
		die "live symlink uses an unsupported target spelling"
	[[ "${PREV_RESOLVED}" =~ ^/usr/local/bin/anselm-gateway-[0-9a-f]{12}$ ]] ||
		die "live symlink target is outside the versioned binary namespace"
	sudo test -f "${PREV_RESOLVED}" && ! sudo test -L "${PREV_RESOLVED}" || die "live binary target is unsafe"
	sudo install -o root -g root -m 0600 "${PREV_RESOLVED}" "${BUNDLE}/files/anselm-gateway"
	write_state link-raw-target "${PREV_RAW}"
	write_state link-resolved-target "${PREV_RESOLVED}"
	mark link-symlink
elif sudo test -e "${LINK}"; then
	sudo test -f "${LINK}" || die "live binary path is not a regular file"
	sudo install -o root -g root -m 0600 "${LINK}" "${BUNDLE}/files/anselm-gateway"
	mark link-regular
else
	mark link-absent
fi

record_active caddy caddy-active
record_active anselm-gateway.socket socket-active
record_active anselm-gateway.service service-active
SOCKET_ENABLEMENT="$(systemctl is-enabled anselm-gateway.socket 2>/dev/null || true)"
case "${SOCKET_ENABLEMENT}" in
	enabled) mark socket-enabled ;;
	enabled-runtime) mark socket-enabled-runtime ;;
	disabled | static | indirect | masked | not-found) ;;
	*) die "cannot snapshot anselm-gateway.socket enablement (${SOCKET_ENABLEMENT:-no state})" ;;
esac
mark snapshot-meta-complete

write_snapshot_manifest() {
	# Atomic replacement keeps the pre-stop manifest usable if a later DB copy or
	# checksum operation is interrupted.
	sudo bash -c '
		set -euo pipefail
		cd "$1"
		tmp="SNAPSHOT.sha256.tmp.$$"
		trap '\''rm -f -- "$tmp"'\'' EXIT
		find . -type f \
			! -name SNAPSHOT.sha256 \
			! -name "SNAPSHOT.sha256.tmp.*" \
			! -name READY ! -name CONSUMED ! -name AUTO_RESTORED ! -name RESTORE_FAILED \
			! -name db-snapshot-complete \
			-print0 | LC_ALL=C sort -z | xargs -0 sha256sum >"$tmp"
		chmod 0600 "$tmp"
		mv -f "$tmp" SNAPSHOT.sha256
		trap - EXIT
	' _ "${BUNDLE}"
}
write_snapshot_manifest
# The marker is a reboot-persistent promise that its referenced recovery bundle
# is complete. Flush the metadata snapshot (including the old rollback entry and
# bundle-local recovery program) before that promise can become durable.
sudo sync

unit_state() {
	systemctl is-active "$1" 2>/dev/null || true
}

stop_and_confirm() {
	local unit="$1" state i
	sudo systemctl stop "${unit}" >/dev/null 2>&1 || true
	for i in $(seq 1 30); do
		state="$(unit_state "${unit}")"
		case "${state}" in
			inactive | failed | unknown) return 0 ;;
		esac
		sleep 1
	done
	die "${unit} did not stop (state=$(unit_state "${unit}"))"
}

# DB restoration always writes files as anselm:anselm. Establish both NSS
# identities while the old deployment and public ingress are still untouched;
# first-install account provisioning must never strand the transition stopped.
getent group anselm >/dev/null 2>&1 || sudo groupadd --system anselm
getent passwd anselm >/dev/null 2>&1 ||
	sudo useradd --system --gid anselm --no-create-home --home-dir "${DATA_DIR}" --shell /usr/sbin/nologin anselm
ANSELM_GROUP="$(getent group anselm)"
IFS=: read -r ANSELM_GROUP_NAME ANSELM_GROUP_PASSWORD ANSELM_GROUP_ID ANSELM_GROUP_MEMBERS <<<"${ANSELM_GROUP}"
[[ "${ANSELM_GROUP_NAME}" == anselm && "${ANSELM_GROUP_ID}" =~ ^[0-9]+$ ]] ||
	die "anselm service group is malformed"
ANSELM_PASSWD="$(getent passwd anselm)"
IFS=: read -r ANSELM_NAME ANSELM_PASSWORD ANSELM_UID ANSELM_GID ANSELM_GECOS ANSELM_HOME ANSELM_SHELL <<<"${ANSELM_PASSWD}"
[[ "${ANSELM_NAME}" == anselm && "${ANSELM_UID}" =~ ^[0-9]+$ && "${ANSELM_GID}" =~ ^[0-9]+$ ]] ||
	die "anselm service identity is malformed"
[[ "${ANSELM_UID}" -ne 0 && "${ANSELM_GROUP_ID}" -ne 0 && "${ANSELM_GID}" == "${ANSELM_GROUP_ID}" ]] ||
	die "anselm must use a non-root same-name primary group"
case "${ANSELM_SHELL}" in
/usr/sbin/nologin | /sbin/nologin | /bin/false) ;;
*) die "anselm service identity must use a non-login shell" ;;
esac

MUTATED=1
publish_transition_marker deploy
sudo sync
# The marker is now durable and names a self-contained recovery program, so a
# crash at any point in this atomic switch remains recoverable. Keeping the old
# global entry until this boundary also means every pre-transition failure leaves
# the retained old READY bundle paired with the tool version that created it.
sudo install -d -o root -g root -m 0755 /usr/local/sbin
sudo rm -f -- "${ROLLBACK_TOOL_TMP}"
sudo install -o root -g root -m 0750 "${STAGE}/rollback.sh" "${ROLLBACK_TOOL_TMP}"
sudo mv -Tf "${ROLLBACK_TOOL_TMP}" "${ROLLBACK_TOOL}"
sudo sync
# Caddy first blocks the public edge; stopping the socket then the process drains
# every possible writer. The local gate runs while Caddy remains stopped.
stop_and_confirm caddy
stop_and_confirm anselm-gateway.socket
stop_and_confirm anselm-gateway.service

snapshot_db_file() {
	local source="$1" saved="$2" marker="$3"
	if sudo test -e "${source}" || sudo test -L "${source}"; then
		sudo test -f "${source}" && ! sudo test -L "${source}" || die "unsafe SQLite file after stop: ${source}"
		sudo install -o root -g root -m 0600 "${source}" "${BUNDLE}/db/${saved}"
		mark "${marker}"
	fi
}

# No sqlite3 dependency: with Caddy/socket/service confirmed stopped, these
# immutable copies form one coherent SQLite main+WAL+SHM set.
snapshot_db_file "${DB_PATH}" main had-db-main
snapshot_db_file "${DB_PATH}-wal" wal had-db-wal
snapshot_db_file "${DB_PATH}-shm" shm had-db-shm
write_snapshot_manifest
# The completion marker is deliberately excluded from the manifest. First make
# the DB copies, their presence markers, and the manifest durable; only then may
# rollback consider the DB snapshot usable. A failure before this point leaves
# the live, still-unmodified DB in place and automatic rollback ignores the
# incomplete DB directory.
sudo sync
mark db-snapshot-complete
sudo sync

# The compatibility snapshot is durable before the new binary can migrate the
# database. This does not require sqlite3 and is executed with every writer down.
if [[ "${RESET_UNLAUNCHED_GATEWAY_DATA}" == 1 ]]; then
	# A manual deployment may request this only after the workflow's exact
	# confirmation phrase.  This is deliberately narrow: no directory or glob is
	# removed, only the validated gateway SQLite main/WAL/SHM files.  The durable
	# snapshot above still makes a failure before commit automatically recoverable.
	log "confirmed pre-launch reset: removing the legacy gateway SQLite database"
	for db_file in "${DB_PATH}" "${DB_PATH}-wal" "${DB_PATH}-shm"; do
		if sudo test -e "${db_file}" || sudo test -L "${db_file}"; then
			sudo test -f "${db_file}" && ! sudo test -L "${db_file}" || die "unsafe reset SQLite path: ${db_file}"
			sudo rm -f -- "${db_file}"
		fi
	done
	sudo sync
fi

# Install all new state while public traffic is still blocked.
sudo install -d -o root -g root -m 0755 "${BIN_DIR}" /usr/local/sbin /etc/caddy /var/www
sudo install -d -o anselm -g anselm -m 0700 "${DATA_DIR}"

NEW="${BIN_DIR}/anselm-gateway-${SHA}"
NEW_TMP="${NEW}.install.$$"
sudo install -o root -g root -m 0755 "${STAGE}/anselm-gateway" "${NEW_TMP}"
sudo mv -Tf "${NEW_TMP}" "${NEW}"

sudo install -o root -g root -m 0644 "${STAGE}/anselm-gateway.service" "${SERVICE_PATH}"
sudo install -o root -g root -m 0644 "${STAGE}/anselm-gateway.socket" "${SOCKET_PATH}"
sudo install -o root -g root -m 0600 "${STAGE}/gateway.env" "${ENV_PATH}"
sudo install -o root -g root -m 0644 "${WORK}/Caddyfile" "${CADDY_PATH}"

[[ "${SITE_PATH}" == /var/www/anselm-site ]] || die "unsafe static site target"
sudo rm -rf -- "${SITE_PATH}"
sudo install -d -o root -g root -m 0755 "${SITE_PATH}"
sudo install -o root -g root -m 0444 "${STAGE}/site/index.html" "${SITE_PATH}/index.html"
sudo install -o root -g root -m 0444 "${STAGE}/site/styles.css" "${SITE_PATH}/styles.css"

sudo ln -sfn "${NEW}" "${LINK}.tmp"
sudo mv -Tf "${LINK}.tmp" "${LINK}"
sudo systemctl daemon-reload
sudo systemctl enable anselm-gateway.socket >/dev/null
sudo systemctl start anselm-gateway.socket
if ! sudo systemctl start anselm-gateway.service; then
	diagnose_gateway_start_failure
	die "anselm-gateway.service failed to start"
fi

gate_ok() {
	local ready=0 healthy=0 i
	for i in $(seq 1 20); do
		curl -fsS --connect-timeout 1 --max-time 2 "${ADMIN_READYZ}" >/dev/null 2>&1 && { ready=1; break; }
		sleep 3
	done
	for i in $(seq 1 20); do
		curl -fsS --connect-timeout 1 --max-time 2 "${LOCAL_HEALTHZ}" 2>/dev/null | grep -q '"ok"' && { healthy=1; break; }
		sleep 3
	done
	[[ ${ready} -eq 1 && ${healthy} -eq 1 ]]
}
gate_ok || die "new binary failed the local readyz/healthz gate"

# Enforce on-disk secrecy before public ingress reopens. UMask=0077 in the unit
# protects subsequently created WAL/SHM files.
sudo chmod 0700 "${DATA_DIR}"
for db_file in "${DB_PATH}" "${DB_PATH}-wal" "${DB_PATH}-shm"; do
	if sudo test -e "${db_file}"; then
		sudo test -f "${db_file}" && ! sudo test -L "${db_file}" || die "unsafe runtime SQLite file: ${db_file}"
		sudo chown anselm:anselm "${db_file}"
		sudo chmod 0600 "${db_file}"
	fi
done

# READY is excluded from the checksum manifest and denotes a complete manual
# rollback unit. Keep the older READY bundle until Caddy successfully starts.
sudo install -o root -g root -m 0600 /dev/null "${BUNDLE}/READY"
sudo caddy validate --config "${CADDY_PATH}" --adapter caddyfile >/dev/null

# Commit point: local gates passed, all new state and the rollback bundle are
# durable, and public ingress is still blocked by both the stopped unit and the
# persistent marker condition. A crash before marker removal stays fail-closed
# and recoverable; a crash after removal boots only this already-gated state.
sudo sync
publish_transition_marker commit-ready
sudo sync
clear_transition_marker
sudo sync
COMMITTED=1
if ! sudo systemctl start caddy; then
	die "local deploy committed but Caddy failed to start; verify ingress state; both rollback bundles are retained"
fi
[[ "$(unit_state caddy)" == active ]] ||
	die "local deploy committed but Caddy is not active; verify ingress state; both rollback bundles are retained"
log "deploy gate OK on ${SHA}; public ingress reopened without a post-traffic DB rewind window"

# Retain exactly one committed rollback target. Forensic snapshots whose restore
# failed are deliberately kept (without READY) and are never pruned here.
prune_old_ready_bundles() {
	# The deploy user cannot enumerate this 0700 root directory, so selection and
	# deletion both run in one constrained root shell. Failed forensic snapshots
	# have no READY marker and survive.
	sudo bash -c '
		set -euo pipefail
		root="$1"
		keep="$2"
		[[ "$root" == /var/lib/anselm-gateway-rollbacks ]]
		[[ "$keep" =~ ^/var/lib/anselm-gateway-rollbacks/bundle-[0-9a-f]{12}\.[A-Za-z0-9]{10}$ ]]
		shopt -s nullglob
		for candidate in "$root"/bundle-*; do
			[[ "$candidate" != "$keep" && -d "$candidate" && ! -L "$candidate" ]] || continue
			[[ "$candidate" =~ ^/var/lib/anselm-gateway-rollbacks/bundle-[0-9a-f]{12}\.[A-Za-z0-9]{10}$ ]] || continue
			[[ -f "$candidate/READY" && ! -L "$candidate/READY" ]] || continue
			rm -rf -- "$candidate"
		done
		ready_count=0
		for candidate in "$root"/bundle-*; do
			[[ -d "$candidate" && ! -L "$candidate" && -f "$candidate/READY" && ! -L "$candidate/READY" ]] || continue
			ready_count=$((ready_count + 1))
		done
		[[ "$ready_count" -eq 1 && -f "$keep/READY" ]]
	' _ "${ROLLBACK_ROOT}" "${BUNDLE}"
}
prune_old_ready_bundles || die "deploy is live but rollback bundle pruning failed; no DB rewind was attempted"

# Binary pruning is post-commit and best-effort. The retained rollback bundle has
# its own copy, so schema-aware recovery never depends on this version cache.
prune_old_binaries() {
	local current old count=0
	current="$(readlink -f "${LINK}")"
	while IFS= read -r old; do
		count=$((count + 1))
		[[ ${count} -le 5 ]] && continue
		[[ "$(readlink -f "${old}")" == "${current}" ]] && continue
		sudo rm -f -- "${old}"
	done < <(find "${BIN_DIR}" -maxdepth 1 -type f -name 'anselm-gateway-????????????' \
		-printf '%T@ %p\n' | LC_ALL=C sort -nr | cut -d' ' -f2-)
}
prune_old_binaries || log "WARNING: could not prune an old binary"

# Public probes are observational only. They cannot trigger a state rewind after
# the commit point; ACME/DNS propagation may legitimately lag a healthy deploy.
if curl -fsS --max-time 15 "https://${GATEWAY_DOMAIN}/healthz" 2>/dev/null | grep -q '"ok"'; then
	log "public API healthz is green"
else
	log "WARNING: public API probe is not green yet (deploy remains committed)"
fi
if curl -fsS --max-time 15 "https://${SITE_DOMAIN}/" 2>/dev/null | grep -q '<title>Anselm</title>'; then
	log "public static site is green"
else
	log "WARNING: public site probe is not green yet (deploy remains committed)"
fi

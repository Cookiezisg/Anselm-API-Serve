#!/usr/bin/env bash
# Render the reviewed Caddy template without letting the caller's shell expand
# its literal {$NAME} placeholders. This is kept separate so the exact renderer
# used by production is covered by a local regression test.
set -euo pipefail
set +x
umask 077

die() {
	printf 'render-caddy: %s\n' "$*" >&2
	exit 1
}

[[ $# -eq 5 ]] || die "usage: $0 TEMPLATE OUTPUT GATEWAY_DOMAIN SITE_DOMAIN ACME_EMAIL"
TEMPLATE="$1"
OUTPUT="$2"
GATEWAY_DOMAIN="$3"
SITE_DOMAIN="$4"
ACME_EMAIL="$5"

[[ -f "${TEMPLATE}" && ! -L "${TEMPLATE}" ]] || die "template must be a regular non-symlink"
for value_name in GATEWAY_DOMAIN SITE_DOMAIN ACME_EMAIL; do
	value="${!value_name}"
	[[ -n "${value}" ]] || die "${value_name} is empty"
	[[ "${value}" != *$'\n'* && "${value}" != *$'\r'* ]] ||
		die "${value_name} contains CR or LF"
done

for placeholder in '{$GATEWAY_DOMAIN}' '{$SITE_DOMAIN}' '{$ACME_EMAIL}'; do
	grep -Fq -- "${placeholder}" "${TEMPLATE}" ||
		die "template is missing required placeholder ${placeholder}"
done

sed_replacement() {
	printf '%s' "$1" | sed 's/[\&|]/\\&/g'
}
GW_REPL="$(sed_replacement "${GATEWAY_DOMAIN}")"
SITE_REPL="$(sed_replacement "${SITE_DOMAIN}")"
EMAIL_REPL="$(sed_replacement "${ACME_EMAIL}")"

# Character classes make '$' unambiguously literal in the sed program; shell
# variables are concatenated only into the replacement side.
sed -e 's|[{][$]GATEWAY_DOMAIN[}]|'"${GW_REPL}"'|g' \
	-e 's|[{][$]SITE_DOMAIN[}]|'"${SITE_REPL}"'|g' \
	-e 's|[{][$]ACME_EMAIL[}]|'"${EMAIL_REPL}"'|g' \
	"${TEMPLATE}" >"${OUTPUT}"

for placeholder in '{$GATEWAY_DOMAIN}' '{$SITE_DOMAIN}' '{$ACME_EMAIL}'; do
	if grep -Fq -- "${placeholder}" "${OUTPUT}"; then
		die "render left placeholder ${placeholder} unresolved"
	fi
done

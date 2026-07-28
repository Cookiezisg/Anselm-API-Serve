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

[[ $# -eq 6 ]] || die "usage: $0 TEMPLATE OUTPUT GATEWAY_DOMAIN SITE_DOMAIN ACME_EMAIL MEDIA_DOMAIN"
TEMPLATE="$1"
OUTPUT="$2"
GATEWAY_DOMAIN="$3"
SITE_DOMAIN="$4"
ACME_EMAIL="$5"
# MEDIA_DOMAIN is REQUIRED here even though the gateway binary tolerates an empty one (enrollment
# simply unavailable). Caddy cannot render an empty host, so the deploy must make the choice
# explicit rather than emit a broken site block.
# MEDIA_DOMAIN 在这里**必填**,尽管网关二进制容许它为空(只是登记不可用)。Caddy 渲染不了空主机名,
# 故部署必须把这个选择摆到明面上,而不是吐出一个坏掉的 site 块。
MEDIA_DOMAIN="$6"

[[ -f "${TEMPLATE}" && ! -L "${TEMPLATE}" ]] || die "template must be a regular non-symlink"
for value_name in GATEWAY_DOMAIN SITE_DOMAIN ACME_EMAIL MEDIA_DOMAIN; do
	value="${!value_name}"
	[[ -n "${value}" ]] || die "${value_name} is empty"
	[[ "${value}" != *$'\n'* && "${value}" != *$'\r'* ]] ||
		die "${value_name} contains CR or LF"
done

for placeholder in '{$GATEWAY_DOMAIN}' '{$SITE_DOMAIN}' '{$ACME_EMAIL}' '{$MEDIA_DOMAIN}'; do
	grep -Fq -- "${placeholder}" "${TEMPLATE}" ||
		die "template is missing required placeholder ${placeholder}"
done

# The one shape the upstream fetcher rejects at its own edge (ADR 0012's production experiment:
# api.<domain> failed 400 three times while the origin log proved no request arrived). A media host
# under `api.` fails INVISIBLY, so refuse it at render time too — the gateway's own boot check
# cannot see the Caddy side.
# 那是拉取器在它自己边缘唯一会拒的形状(ADR 0012 生产实验:api.<域> 连续三次 400,而源站日志证明
# 请求从未到达)。`api.` 开头的媒体主机会**无形地**失败,故渲染时也拒——网关自己的启动检查看不到
# Caddy 这一侧。
[[ "${MEDIA_DOMAIN}" != api.* ]] ||
	die "MEDIA_DOMAIN must not be an api.* host; the upstream fetcher blacklists that shape"

sed_replacement() {
	printf '%s' "$1" | sed 's/[\&|]/\\&/g'
}
GW_REPL="$(sed_replacement "${GATEWAY_DOMAIN}")"
MEDIA_REPL="$(sed_replacement "${MEDIA_DOMAIN}")"
SITE_REPL="$(sed_replacement "${SITE_DOMAIN}")"
EMAIL_REPL="$(sed_replacement "${ACME_EMAIL}")"

# Character classes make '$' unambiguously literal in the sed program; shell
# variables are concatenated only into the replacement side.
sed -e 's|[{][$]GATEWAY_DOMAIN[}]|'"${GW_REPL}"'|g' \
	-e 's|[{][$]SITE_DOMAIN[}]|'"${SITE_REPL}"'|g' \
	-e 's|[{][$]ACME_EMAIL[}]|'"${EMAIL_REPL}"'|g' \
	-e 's|[{][$]MEDIA_DOMAIN[}]|'"${MEDIA_REPL}"'|g' \
	"${TEMPLATE}" >"${OUTPUT}"

for placeholder in '{$GATEWAY_DOMAIN}' '{$SITE_DOMAIN}' '{$ACME_EMAIL}' '{$MEDIA_DOMAIN}'; do
	if grep -Fq -- "${placeholder}" "${OUTPUT}"; then
		die "render left placeholder ${placeholder} unresolved"
	fi
done

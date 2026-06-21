#!/usr/bin/env bash
# Anselm Free-Tier Gateway — manual rollback (OPS-1)
# 在服务器上以可 sudo 的用户运行:把 /usr/local/bin/anselm-gateway symlink 切回
# 上一个版本化二进制 + restart + readyz 验证。CI 部署 gate 失败时会自动回滚;
# 本脚本是人工兜底(例:发现回归想立刻退版)。
#
# 约定(与 deploy.yml 一致):
#   - 版本化二进制:/usr/local/bin/anselm-gateway-<gitsha>
#   - 当前指针:    /usr/local/bin/anselm-gateway -> 上述某个版本(原子 symlink)
#   - 保留最近 5 版,更旧的已被部署流水线清理。
#
# 用法:
#   sudo deploy/rollback.sh            # 回退到「上一个」版本(按 mtime 次新)
#   sudo deploy/rollback.sh <gitsha>   # 回退到指定版本
set -euo pipefail

BIN_DIR=/usr/local/bin
LINK="${BIN_DIR}/anselm-gateway"
ADMIN_READYZ="http://127.0.0.1:9090/readyz"

current_target() {
  # symlink 当前指向的版本化二进制(绝对路径)。
  readlink -f "${LINK}" || true
}

pick_previous() {
  # 列出所有版本化二进制,按 mtime 新→旧;跳过当前指向的那个,取次新。
  local cur
  cur="$(current_target)"
  # shellcheck disable=SC2012  # 文件名是受控 gitsha,无需 find -print0
  ls -t "${BIN_DIR}"/anselm-gateway-* 2>/dev/null | while read -r f; do
    [ "$(readlink -f "$f")" = "${cur}" ] && continue
    echo "$f"
    break
  done
}

main() {
  local target
  if [ "$#" -ge 1 ]; then
    target="${BIN_DIR}/anselm-gateway-$1"
  else
    target="$(pick_previous)"
  fi

  if [ -z "${target:-}" ] || [ ! -x "${target}" ]; then
    echo "rollback: no valid previous binary found (looked for ${target:-<none>})" >&2
    echo "available versions:" >&2
    ls -t "${BIN_DIR}"/anselm-gateway-* 2>/dev/null >&2 || echo "  (none)" >&2
    exit 1
  fi

  echo "rollback: switching ${LINK} -> ${target}"
  # Atomic swap: ln -sfn to a temp name then mv over the live link so a reader
  # never sees a missing symlink. (原子换指针,绝不出现悬空 symlink。)
  ln -sfn "${target}" "${LINK}.tmp"
  mv -Tf "${LINK}.tmp" "${LINK}"

  echo "rollback: restarting service"
  systemctl restart anselm-gateway

  echo "rollback: verifying readiness (${ADMIN_READYZ})"
  for i in $(seq 1 12); do
    if curl -fsS "${ADMIN_READYZ}" >/dev/null 2>&1; then
      echo "rollback: readyz OK after ${i} attempt(s) — now on $(basename "${target}")"
      exit 0
    fi
    sleep 5
  done

  echo "rollback: readyz did NOT come up within ~60s — manual intervention needed" >&2
  exit 1
}

main "$@"

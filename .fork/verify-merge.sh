#!/usr/bin/env bash
# 合并上游后的校验关卡：重新生成代码后编译、检查并测试前后端，不通过则不得推送。
# 覆盖 git 无法发现的语义冲突（例如上游改了函数签名，而 fork 独有代码仍按旧签名调用）。
#
# 用法：
#   ./.fork/verify-merge.sh          # 完整校验（推送前关卡）
#   ./.fork/verify-merge.sh --quick  # 生成 + 编译 + vet + 前端类型检查（供 AI 快速迭代）
#
# 输出保持精简：成功的步骤只打印一行，失败时打印该步骤日志末尾。
# 完整日志写入 $VERIFY_LOG_DIR（默认临时目录）。PNPM 可覆盖 pnpm 命令（例如 "npx -y pnpm@9"）。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

QUICK=false
if [[ "${1:-}" == "--quick" ]]; then
  QUICK=true
fi
PNPM="${PNPM:-pnpm}"
LOG_DIR="${VERIFY_LOG_DIR:-$(mktemp -d)}"
mkdir -p "$LOG_DIR"
TAIL_LINES="${VERIFY_TAIL_LINES:-120}"

run_step() {
  local name="$1" log
  shift
  log="$LOG_DIR/$(printf '%s' "$name" | tr -c 'A-Za-z0-9' '_').log"
  if "$@" >"$log" 2>&1; then
    echo "PASS  ${name}"
    return 0
  fi
  echo "FAIL  ${name}"
  echo "----- ${name}: last ${TAIL_LINES} lines (full log: ${log}) -----"
  tail -n "$TAIL_LINES" "$log"
  echo "-----"
  exit 1
}

go_unit_tests() {
  # 只保留失败信息，避免数百行 ok 淹没真正的错误
  local out status
  set +e
  out="$(cd backend && go test -tags=unit ./... 2>&1)"
  status=$?
  set -e
  printf '%s\n' "$out" | grep -vE '^(ok|\?)[[:space:]]' || true
  return "$status"
}

unmerged="$(git diff --name-only --diff-filter=U)"
if [[ -n "$unmerged" ]]; then
  echo "FAIL  仍有未解决的冲突文件："
  printf '%s\n' "$unmerged" | sed 's/^/  /'
  exit 1
fi

tree_state() {
  { git diff --no-ext-diff --binary; git ls-files --others --exclude-standard; } | cksum
}

run_step "regenerate ent/wire" bash .fork/regenerate-backend.sh
# 之后的步骤只读：若改动了仓库文件（例如包管理器改写配置），这些改动会被一并提交，必须拦下
state_after_regen="$(tree_state)"

run_step "go build" bash -c 'cd backend && go build ./...'
run_step "go vet (unit + integration tags)" \
  bash -c 'cd backend && go vet -tags=unit ./... && go vet -tags=integration ./...'
if [[ "$QUICK" == false ]]; then
  run_step "go unit tests" go_unit_tests
fi

run_step "frontend install" $PNPM --dir frontend install --frozen-lockfile
if [[ "$QUICK" == true ]]; then
  run_step "frontend typecheck" $PNPM --dir frontend run typecheck
else
  run_step "frontend lint + typecheck + critical vitest" make test-frontend
fi

if [[ "$(tree_state)" != "$state_after_regen" ]]; then
  echo "FAIL  校验步骤改动了仓库文件（只允许重新生成 ent/wire 代码）："
  git status --short | sed 's/^/  /'
  exit 1
fi
echo "verify-merge: all checks passed"

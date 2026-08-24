#!/usr/bin/env bash

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

fail() {
  echo "FAIL: $*" >&2
  exit 1
}

require_workflow_guard() {
  local workflow="$1"
  grep -Fq './.fork/restore-managed-workflows.sh origin/main' "$ROOT/$workflow" \
    || fail "$workflow 未在推送前恢复 fork 自管工作流"
  grep -Fq 'git diff --cached --quiet' "$ROOT/$workflow" \
    || fail "$workflow 未检查暂存区中的工作流恢复结果"
  grep -Fq 'git commit --amend --no-edit' "$ROOT/$workflow" \
    || fail "$workflow 必须修订原合并提交，不能追加包含 workflow 变更的中间提交"
}

require_workflow_guard ".github/workflows/mirror-upstream-release.yml"
require_workflow_guard ".github/workflows/sync-upstream.yml"

require_dynamic_go_check() {
  local workflow="$1"
  local expected_count="$2"
  local actual_count

  actual_count="$(grep -Fc 'EXPECTED_GO_VERSION="$(awk' "$ROOT/$workflow" || true)"
  [[ "$actual_count" -eq "$expected_count" ]] \
    || fail "$workflow 的 Go 版本校验未全部从 backend/go.mod 动态读取"

  if grep -Eq "grep -q ['\"]go[0-9]+\." "$ROOT/$workflow"; then
    fail "$workflow 仍包含硬编码 Go 版本"
  fi
}

require_dynamic_go_check ".github/workflows/backend-ci.yml" 2
require_dynamic_go_check ".github/workflows/release.yml" 1
require_dynamic_go_check ".github/workflows/security-scan.yml" 1

suite_tmp="$(mktemp -d)"
trap 'rm -rf "$suite_tmp"' EXIT

tmp="$suite_tmp/clean-merge"
mkdir -p "$tmp"

git -C "$tmp" init -q
git -C "$tmp" config user.name "Fork workflow test"
git -C "$tmp" config user.email "fork-workflow-test@example.invalid"
git -C "$tmp" config core.autocrlf false
fork_branch="$(git -C "$tmp" symbolic-ref --short HEAD)"

mkdir -p "$tmp/.github/workflows"
printf 'fork baseline\n' >"$tmp/.github/workflows/existing.yml"
printf 'must survive\n' >"$tmp/.github/workflows/deleted-upstream.yml"
printf 'fork app\n' >"$tmp/app.txt"
git -C "$tmp" add -A
git -C "$tmp" commit -qm "fork baseline"
git -C "$tmp" branch upstream

printf 'fork-only change\n' >"$tmp/fork.txt"
git -C "$tmp" add -A
git -C "$tmp" commit -qm "fork change"
base="$(git -C "$tmp" rev-parse HEAD)"

git -C "$tmp" checkout -q upstream

printf 'upstream replacement\n' >"$tmp/.github/workflows/existing.yml"
rm "$tmp/.github/workflows/deleted-upstream.yml"
printf 'unreviewed workflow\n' >"$tmp/.github/workflows/new-upstream.yml"
printf 'upstream app change\n' >"$tmp/app.txt"
git -C "$tmp" add -A
git -C "$tmp" commit -qm "upstream change"

git -C "$tmp" checkout -q "$fork_branch"
git -C "$tmp" merge --no-ff -qm "simulate upstream merge" upstream

mkdir -p "$tmp/.fork"
cp "$ROOT/.fork/restore-managed-workflows.sh" "$tmp/.fork/restore-managed-workflows.sh"
(
  cd "$tmp"
  bash ./.fork/restore-managed-workflows.sh "$base"
)

git -C "$tmp" add -A
git -C "$tmp" commit --amend --no-edit -q

git -C "$tmp" diff --exit-code "$base" HEAD -- .github/workflows \
  || fail "工作流目录未恢复为 fork 基线"
grep -Fqx 'upstream app change' "$tmp/app.txt" \
  || fail "保护工作流时不应回滚业务代码"

parent_count="$(git -C "$tmp" rev-list --parents -n 1 HEAD | awk '{print NF - 1}')"
[[ "$parent_count" -eq 2 ]] || fail "修订后必须保留双父合并历史"

merge_script="$ROOT/.fork/merge-upstream-preserving-workflows.sh"
[[ -f "$merge_script" ]] || fail "缺少统一的 workflow 冲突恢复脚本"
if grep -Eq '(^|[^[:alnum:]_])(mapfile|readarray)([^[:alnum:]_]|$)' "$merge_script"; then
  fail "统一合并脚本必须兼容 macOS Bash 3，不能使用 mapfile/readarray"
fi

workflow_conflict_tmp="$suite_tmp/workflow-conflict"
mkdir -p "$workflow_conflict_tmp/.github/workflows" "$workflow_conflict_tmp/.fork"
git -C "$workflow_conflict_tmp" init -q
git -C "$workflow_conflict_tmp" config user.name "Fork workflow test"
git -C "$workflow_conflict_tmp" config user.email "fork-workflow-test@example.invalid"
git -C "$workflow_conflict_tmp" config core.autocrlf false
workflow_conflict_branch="$(git -C "$workflow_conflict_tmp" symbolic-ref --short HEAD)"

printf 'name: baseline\n' >"$workflow_conflict_tmp/.github/workflows/ci.yml"
printf 'base app\n' >"$workflow_conflict_tmp/app.txt"
git -C "$workflow_conflict_tmp" add -A
git -C "$workflow_conflict_tmp" commit -qm "common baseline"
git -C "$workflow_conflict_tmp" branch upstream

printf 'name: fork managed\n' >"$workflow_conflict_tmp/.github/workflows/ci.yml"
git -C "$workflow_conflict_tmp" add -A
git -C "$workflow_conflict_tmp" commit -qm "fork workflow"
workflow_conflict_base="$(git -C "$workflow_conflict_tmp" rev-parse HEAD)"

git -C "$workflow_conflict_tmp" checkout -q upstream
printf 'name: upstream replacement\n' >"$workflow_conflict_tmp/.github/workflows/ci.yml"
printf 'upstream app\n' >"$workflow_conflict_tmp/app.txt"
git -C "$workflow_conflict_tmp" add -A
git -C "$workflow_conflict_tmp" commit -qm "upstream workflow and app"

git -C "$workflow_conflict_tmp" checkout -q "$workflow_conflict_branch"
cp "$ROOT/.fork/restore-managed-workflows.sh" "$workflow_conflict_tmp/.fork/restore-managed-workflows.sh"
cp "$merge_script" "$workflow_conflict_tmp/.fork/merge-upstream-preserving-workflows.sh"
(
  cd "$workflow_conflict_tmp"
  bash ./.fork/merge-upstream-preserving-workflows.sh \
    "$workflow_conflict_base" "test: merge workflow conflict" upstream
)

[[ -z "$(git -C "$workflow_conflict_tmp" diff --name-only --diff-filter=U)" ]] \
  || fail "仅 workflow 冲突时不应留下未解决文件"
git -C "$workflow_conflict_tmp" diff --exit-code \
  "$workflow_conflict_base" HEAD -- .github/workflows \
  || fail "workflow 冲突未恢复为 fork 基线"
grep -Fqx 'upstream app' "$workflow_conflict_tmp/app.txt" \
  || fail "恢复 workflow 冲突时必须保留上游业务变更"
workflow_parent_count="$(git -C "$workflow_conflict_tmp" rev-list --parents -n 1 HEAD | awk '{print NF - 1}')"
[[ "$workflow_parent_count" -eq 2 ]] || fail "workflow 冲突恢复后必须生成双父合并提交"

business_conflict_tmp="$suite_tmp/business-conflict"
mkdir -p "$business_conflict_tmp/.github/workflows" "$business_conflict_tmp/.fork"
git -C "$business_conflict_tmp" init -q
git -C "$business_conflict_tmp" config user.name "Fork workflow test"
git -C "$business_conflict_tmp" config user.email "fork-workflow-test@example.invalid"
git -C "$business_conflict_tmp" config core.autocrlf false
business_conflict_branch="$(git -C "$business_conflict_tmp" symbolic-ref --short HEAD)"

printf 'name: baseline\n' >"$business_conflict_tmp/.github/workflows/ci.yml"
printf 'base app\n' >"$business_conflict_tmp/app.txt"
git -C "$business_conflict_tmp" add -A
git -C "$business_conflict_tmp" commit -qm "common baseline"
git -C "$business_conflict_tmp" branch upstream

printf 'name: fork managed\n' >"$business_conflict_tmp/.github/workflows/ci.yml"
printf 'fork app\n' >"$business_conflict_tmp/app.txt"
git -C "$business_conflict_tmp" add -A
git -C "$business_conflict_tmp" commit -qm "fork workflow and app"
business_conflict_base="$(git -C "$business_conflict_tmp" rev-parse HEAD)"

git -C "$business_conflict_tmp" checkout -q upstream
printf 'name: upstream replacement\n' >"$business_conflict_tmp/.github/workflows/ci.yml"
printf 'upstream app\n' >"$business_conflict_tmp/app.txt"
git -C "$business_conflict_tmp" add -A
git -C "$business_conflict_tmp" commit -qm "upstream workflow and app"

git -C "$business_conflict_tmp" checkout -q "$business_conflict_branch"
cp "$ROOT/.fork/restore-managed-workflows.sh" "$business_conflict_tmp/.fork/restore-managed-workflows.sh"
cp "$merge_script" "$business_conflict_tmp/.fork/merge-upstream-preserving-workflows.sh"
set +e
(
  cd "$business_conflict_tmp"
  bash ./.fork/merge-upstream-preserving-workflows.sh \
    "$business_conflict_base" "test: keep business conflict" upstream
)
business_status=$?
set -e

[[ "$business_status" -eq 2 ]] \
  || fail "真实业务冲突应返回退出码 2，实际为 $business_status"
git -C "$business_conflict_tmp" diff --name-only --diff-filter=U \
  | grep -Fxq 'app.txt' \
  || fail "真实业务冲突必须保持未解决"
if git -C "$business_conflict_tmp" diff --name-only --diff-filter=U \
  | grep -Fq '.github/workflows/'; then
  fail "fork workflow 冲突应在报告业务冲突前完成恢复"
fi
git -C "$business_conflict_tmp" merge --abort

for workflow in \
  ".github/workflows/mirror-upstream-release.yml" \
  ".github/workflows/sync-upstream.yml"; do
  grep -Fq './.fork/merge-upstream-preserving-workflows.sh' "$ROOT/$workflow" \
    || fail "$workflow 未使用统一的 workflow 冲突恢复脚本"
done

if grep -Fq 'gh pr create' "$ROOT/.github/workflows/mirror-upstream-release.yml"; then
  fail "mirror workflow 不应再创建无内容冲突 PR"
fi
if grep -Fq 'git commit --allow-empty' "$ROOT/.github/workflows/sync-upstream.yml"; then
  fail "sync workflow 不应再创建空冲突分支"
fi

echo "PASS: 上游 workflow 变更被隔离，业务代码变更被保留"

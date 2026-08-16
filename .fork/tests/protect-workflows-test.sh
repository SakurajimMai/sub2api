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
  grep -Fq 'git merge --no-ff' "$ROOT/$workflow" \
    || fail "$workflow 必须强制生成可修订的双父合并提交"
}

require_workflow_guard ".github/workflows/mirror-upstream-release.yml"
require_workflow_guard ".github/workflows/sync-upstream.yml"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

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

echo "PASS: 上游 workflow 变更被隔离，业务代码变更被保留"

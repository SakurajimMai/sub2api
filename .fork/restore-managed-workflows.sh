#!/usr/bin/env bash
# 自动同步上游时，保留由 fork 自主管理的 GitHub Actions 工作流。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

BASE_REF="${1:-origin/main}"
MANAGED_PATH=".github/workflows"

if ! git rev-parse --verify "${BASE_REF}^{commit}" >/dev/null 2>&1; then
  echo "error: fork 基线不存在: ${BASE_REF}" >&2
  exit 1
fi

if ! git cat-file -e "${BASE_REF}:${MANAGED_PATH}" 2>/dev/null; then
  echo "error: fork 基线缺少 ${MANAGED_PATH}: ${BASE_REF}" >&2
  exit 1
fi

# 同时恢复索引和工作树，保留合并提交作为 HEAD。后续提交会记录这次 fork 修正。
# 这同时覆盖上游对工作流的修改、删除和新增，避免 GITHUB_TOKEN 推送被拒绝。
git restore --source="$BASE_REF" --staged --worktree -- "$MANAGED_PATH"

if git diff --quiet HEAD -- "$MANAGED_PATH"; then
  echo "fork-managed workflows unchanged"
else
  echo "restored fork-managed workflows from ${BASE_REF}:"
  git diff --name-status HEAD -- "$MANAGED_PATH"
fi

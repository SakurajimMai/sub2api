#!/usr/bin/env bash
# 合并上游时自动恢复 fork 自主管理的 GitHub Actions 工作流。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

BASE_REF="${1:?fork base ref is required}"
MERGE_MESSAGE="${2:?merge message is required}"
MERGE_TARGET="${3:?merge target is required}"

git rev-parse --verify "${BASE_REF}^{commit}" >/dev/null
git rev-parse --verify "${MERGE_TARGET}^{commit}" >/dev/null

merge_clean=false
if git merge --no-ff --no-edit -m "$MERGE_MESSAGE" "$MERGE_TARGET"; then
  merge_clean=true
else
  if ! git rev-parse -q --verify MERGE_HEAD >/dev/null; then
    echo "error: merge failed before entering a resolvable conflict state" >&2
    exit 1
  fi
fi

# 始终在合并提交落地前恢复 fork 自管工作流。即使 Git merge 无冲突，
# 上游也可能只修改了 workflow 文件；若直接退出，GitHub App 会在 push
# 时因缺少 workflows 权限拒绝整个提交。
bash "$ROOT/.fork/restore-managed-workflows.sh" "$BASE_REF"

conflicts=()
while IFS= read -r conflict; do
  conflicts+=("$conflict")
done < <(git diff --name-only --diff-filter=U)
if [[ ${#conflicts[@]} -gt 0 ]]; then
  printf 'Remaining non-workflow conflicts:\n' >&2
  printf '  %s\n' "${conflicts[@]}" >&2
  exit 2
fi

if [[ "$merge_clean" == true ]]; then
  # clean merge 已经创建提交；恢复 workflow 后必须修订该提交，使其相对
  # fork 基线不再包含任何 workflow 树变化。
  if ! git diff --quiet HEAD -- .github/workflows || ! git diff --cached --quiet -- .github/workflows; then
    git add .github/workflows
    git commit --amend --no-edit
  fi
  echo "Merge completed after restoring fork-managed workflows."
else
  git commit --no-edit
  echo "Merge completed after restoring fork-managed workflows."
fi

if ! git diff --quiet "$BASE_REF" HEAD -- .github/workflows; then
  echo "error: merged commit still changes fork-managed workflows; refusing to push" >&2
  git diff --name-status "$BASE_REF" HEAD -- .github/workflows >&2 || true
  exit 1
fi

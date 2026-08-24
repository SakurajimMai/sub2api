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

if git merge --no-ff --no-edit -m "$MERGE_MESSAGE" "$MERGE_TARGET"; then
  echo "Merge completed without conflicts."
  exit 0
fi

if ! git rev-parse -q --verify MERGE_HEAD >/dev/null; then
  echo "error: merge failed before entering a resolvable conflict state" >&2
  exit 1
fi

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

git commit --no-edit
echo "Merge completed after restoring fork-managed workflows."

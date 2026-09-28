#!/usr/bin/env bash
# 合并上游出现冲突后，按确定性规则自动解决可机械处理的文件：
#   - backend/cmd/server/VERSION：采用上游版本（版本号以上游发布为准）
#   - backend/go.sum：取双方并集（每行独立校验，多余条目无害）
#   - ent / wire 生成文件（含 "Code generated ... DO NOT EDIT."）：先用 fork 版本占位，
#     之后由 regenerate-backend.sh 按合并后的源码重新生成。不能直接删除：
#     ent/schema 依赖生成的 ent/intercept 包，缺文件会导致生成器本身无法编译。
# 其余冲突保持未解决，交给 AI 或人工处理。
#
# 退出码：0 = 冲突已全部解决；2 = 仍有冲突（列表输出到 stderr）；1 = 执行错误。
# 需兼容 macOS Bash 3：不使用 mapfile/readarray 与关联数组。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

if ! git rev-parse -q --verify MERGE_HEAD >/dev/null; then
  echo "error: 当前不在合并状态，无冲突可解决" >&2
  exit 1
fi

GENERATED_MARKER='^// Code generated .* DO NOT EDIT\.$'

has_stage() {
  git cat-file -e ":$1:$2" 2>/dev/null
}

stage_is_generated() {
  local header
  has_stage "$1" "$2" || return 1
  header="$(git show ":$1:$2" | sed -n '1,5p')"
  grep -Eq "$GENERATED_MARKER" <<<"$header"
}

is_regenerable() {
  case "$1" in
    backend/ent/schema/*) return 1 ;;
    backend/cmd/server/wire_gen.go | backend/ent/*) ;;
    *) return 1 ;;
  esac
  stage_is_generated 2 "$1" || stage_is_generated 3 "$1"
}

union_merge() {
  local path="$1" tmp
  tmp="$(mktemp -d)"
  git show ":2:$path" >"$tmp/ours"
  git show ":3:$path" >"$tmp/theirs"
  git show ":1:$path" >"$tmp/base" 2>/dev/null || : >"$tmp/base"
  git merge-file -p --union "$tmp/ours" "$tmp/base" "$tmp/theirs" >"$path"
  rm -rf "$tmp"
}

conflicts="$(git diff --name-only --diff-filter=U)"
remaining=""

while IFS= read -r path; do
  [[ -n "$path" ]] || continue
  rule=""
  if [[ "$path" == "backend/cmd/server/VERSION" ]] && has_stage 3 "$path"; then
    git checkout --theirs -- "$path"
    rule="采用上游版本"
  elif [[ "$path" == "backend/go.sum" ]] && has_stage 2 "$path" && has_stage 3 "$path"; then
    union_merge "$path"
    rule="取双方并集"
  elif is_regenerable "$path"; then
    if has_stage 2 "$path"; then
      git checkout --ours -- "$path"
    else
      git checkout --theirs -- "$path"
    fi
    rule="生成文件，稍后重新生成"
  fi

  if [[ -n "$rule" ]]; then
    git add -- "$path"
    echo "auto-resolved: ${path} (${rule})"
  else
    remaining="${remaining}${path}"$'\n'
  fi
done <<<"$conflicts"

if [[ -n "$remaining" ]]; then
  echo "Remaining conflicts after auto-resolve:" >&2
  printf '%s' "$remaining" | sed 's/^/  /' >&2
  exit 2
fi
echo "All conflicts resolved by deterministic rules."

#!/usr/bin/env bash
# 自动合并上游：
#   git 合并（保留 fork 工作流）→ 规则化解决冲突 → AI 解决剩余冲突（可选）
#   → 重新生成代码并校验 → 校验失败时 AI 修复（可选）→ 生成最终合并提交
# 任何一步失败都会回到合并前的 HEAD，不留下半成品。
#
# 用法：merge-upstream-auto.sh <fork-base-ref> <merge-message> <merge-target>
#
# 退出码：
#   0  已生成通过校验的合并提交；HEAD 未变化表示已是最新
#   2  存在无法自动解决的冲突（未配置 DEEPSEEK_API_KEY，或 AI 未能解决）
#   3  合并后校验失败
#   1  其他错误
#
# 环境变量：DEEPSEEK_API_KEY（配置后启用 AI）、AI_MERGE_ROUNDS（校验失败后 AI 修复轮数，
# 默认 2）、VERIFY_LOG_DIR、PNPM，以及 ai-resolve-conflicts.sh 支持的变量。
# 在 GitHub Actions 中会把过程写入 $GITHUB_STEP_SUMMARY。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

BASE_REF="${1:?fork base ref is required}"
MERGE_MESSAGE="${2:?merge message is required}"
MERGE_TARGET="${3:?merge target is required}"

# 密钥不导出给子进程（校验会执行上游代码），只在调用 AI 时显式传入
AI_KEY="${DEEPSEEK_API_KEY:-}"
unset DEEPSEEK_API_KEY
AI_ROUNDS="${AI_MERGE_ROUNDS:-2}"
SUMMARY="${GITHUB_STEP_SUMMARY:-/dev/null}"
export VERIFY_LOG_DIR="${VERIFY_LOG_DIR:-$(mktemp -d)}"
PROTECTED_PATHS=(.github/workflows .fork)

if ! git diff --quiet || ! git diff --cached --quiet; then
  echo "error: 工作区有未提交的修改，拒绝自动合并" >&2
  exit 1
fi

start_head="$(git rev-parse HEAD)"
ai_changed=""

summary() {
  printf '%s\n' "$@" >>"$SUMMARY"
}

summary_block() {
  summary '```'
  cat >>"$SUMMARY"
  summary '```'
}

fail() {
  local code="$1" message="$2"
  echo "error: ${message}" >&2
  summary "" "**自动合并未完成：** ${message}"
  if git rev-parse -q --verify MERGE_HEAD >/dev/null; then
    git merge --abort || true
  fi
  git reset -q --hard "$start_head"
  exit "$code"
}

in_merge() {
  git rev-parse -q --verify MERGE_HEAD >/dev/null
}

guard_protected_paths() {
  if ! git diff --quiet "$BASE_REF" -- "${PROTECTED_PATHS[@]}"; then
    git diff --name-status "$BASE_REF" -- "${PROTECTED_PATHS[@]}" >&2 || true
    fail 1 "合并结果改动了受保护路径（.github/workflows 或 .fork）"
  fi
}

run_ai() {
  local mode="$1" log="${2:-}" status changed
  echo "==> AI (${mode})"
  set +e
  DEEPSEEK_API_KEY="$AI_KEY" bash .fork/ai-resolve-conflicts.sh \
    "$mode" "$BASE_REF" "$MERGE_TARGET" "$log"
  status=$?
  set -e
  [[ $status -eq 0 ]] || fail 2 "AI 处理失败（${mode}，退出码 ${status}）"

  guard_protected_paths
  changed="$( (git diff --name-only; git ls-files --others --exclude-standard) | sort -u)"
  ai_changed="$(printf '%s\n%s\n' "$ai_changed" "$changed" | sed '/^$/d' | sort -u)"

  # AI 改过的手写 Go 文件统一 gofmt，生成文件稍后会重新生成
  local path header
  while IFS= read -r path; do
    [[ "$path" == *.go && -f "$path" ]] || continue
    header="$(sed -n '1,5p' "$path")"
    grep -Eq '^// Code generated .* DO NOT EDIT\.$' <<<"$header" && continue
    gofmt -w "$path"
  done <<<"$changed"
}

stage_ai_resolution() {
  local conflicts="$1" path leftover=""
  while IFS= read -r path; do
    [[ -n "$path" ]] || continue
    if [[ -e "$path" ]]; then
      if grep -nE '^(<<<<<<<|>>>>>>>)( |$)' "$path" >/dev/null; then
        leftover="${leftover} ${path}"
        continue
      fi
      git add -- "$path"
    else
      git rm -q --cached --ignore-unmatch -- "$path"
    fi
  done <<<"$conflicts"
  if [[ -n "$leftover" ]]; then
    fail 2 "AI 未能解决以下文件的冲突：${leftover}"
  fi
}

summary "## 上游自动合并：${MERGE_TARGET}"

# 1. git 合并（fork 自管 workflow 冲突在此自动恢复）；diff3 冲突标记便于 AI 看到共同祖先
set +e
GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=merge.conflictStyle GIT_CONFIG_VALUE_0=diff3 \
  bash .fork/merge-upstream-preserving-workflows.sh "$BASE_REF" "$MERGE_MESSAGE" "$MERGE_TARGET"
merge_status=$?
set -e
if [[ $merge_status -ne 0 && $merge_status -ne 2 ]]; then
  fail 1 "git merge 执行失败（退出码 ${merge_status}）"
fi
if [[ $merge_status -eq 0 && "$(git rev-parse HEAD)" == "$start_head" ]]; then
  echo "Already up to date with ${MERGE_TARGET}; nothing to merge."
  summary "已是最新，无需合并。"
  exit 0
fi

# 2. 规则化解决冲突，剩余的交给 AI
if [[ $merge_status -eq 2 ]]; then
  set +e
  bash .fork/auto-resolve-conflicts.sh
  resolve_status=$?
  set -e
  if [[ $resolve_status -ne 0 && $resolve_status -ne 2 ]]; then
    fail 1 "规则化冲突解决执行失败"
  fi

  if [[ $resolve_status -eq 2 ]]; then
    conflicts="$(git diff --name-only --diff-filter=U)"
    summary "" "### 规则无法解决的冲突"
    printf '%s\n' "$conflicts" | sed 's/^/- `/; s/$/`/' >>"$SUMMARY"
    if [[ -z "$AI_KEY" ]]; then
      fail 2 "存在需要人工处理的冲突，且未配置 DEEPSEEK_API_KEY。请在本地合并 ${MERGE_TARGET} 并解决上述文件后推送 main。"
    fi
    run_ai conflicts
    stage_ai_resolution "$conflicts"
  fi
fi

# 3. 重新生成代码并校验；失败时交给 AI 修复（包括无文本冲突的语义冲突）
round=0
while true; do
  log="${VERIFY_LOG_DIR}/verify-round-${round}.txt"
  echo "==> verify (round ${round})"
  set +e
  bash .fork/verify-merge.sh 2>&1 | tee "$log"
  verify_status=${PIPESTATUS[0]}
  set -e
  if [[ $verify_status -eq 0 ]]; then
    break
  fi
  if [[ -z "$AI_KEY" || $round -ge $AI_ROUNDS ]]; then
    summary "" "### 校验失败"
    tail -n 80 "$log" | summary_block
    fail 3 "合并后校验未通过，未推送任何改动"
  fi
  round=$((round + 1))
  git add -A
  run_ai fix "$log"
done

# 4. 生成最终合并提交
git add -A
guard_protected_paths

message_file="$(mktemp)"
printf '%s\n' "$MERGE_MESSAGE" >"$message_file"
if [[ -n "$ai_changed" ]]; then
  {
    echo
    echo "AI-assisted resolution (${AI_MERGE_MODEL:-deepseek-v4-pro} via Claude Code), verified by .fork/verify-merge.sh:"
    printf '%s\n' "$ai_changed" | sed 's/^/- /'
  } >>"$message_file"
fi

if in_merge; then
  git -c core.hooksPath=/dev/null commit -q -F "$message_file"
elif ! git diff --cached --quiet || [[ -n "$ai_changed" ]]; then
  git -c core.hooksPath=/dev/null commit -q --amend -F "$message_file"
fi
rm -f "$message_file"

summary "" "### 合并完成" "" "- 合并提交：\`$(git rev-parse --short HEAD)\`"
if [[ -n "$ai_changed" ]]; then
  summary "- AI 修改的文件（已通过校验）："
  printf '%s\n' "$ai_changed" | sed 's/^/  - `/; s/$/`/' >>"$SUMMARY"
fi
echo "Merged ${MERGE_TARGET} and passed verification."

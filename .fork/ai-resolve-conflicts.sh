#!/usr/bin/env bash
# 用 Claude Code CLI + DeepSeek（Anthropic 兼容接口）处理确定性规则解决不了的合并问题。
#
# 用法：
#   ai-resolve-conflicts.sh conflicts <base-ref> <merge-target>          解决剩余冲突文件
#   ai-resolve-conflicts.sh fix <base-ref> <merge-target> <verify-log>   修复合并后的编译/测试失败
#
# 环境变量：
#   DEEPSEEK_API_KEY     必填
#   AI_MERGE_MODEL       默认 deepseek-v4-pro
#   AI_MERGE_FAST_MODEL  默认 deepseek-flash（Claude Code 的后台小任务）
#   AI_BASE_URL          默认 https://api.deepseek.com/anthropic
#   AI_TIMEOUT           默认 50m
#   CLAUDE_BIN           可选，指定现成的 claude 可执行文件（默认临时 npm 安装）
#
# 安全边界（上游代码是不可信输入，可能包含提示注入）：
#   - 密钥只注入 claude 进程。AI 只能运行仓库外两个只读包装脚本，它们先用 env -i
#     清空环境变量，再执行只读 git 命令或 verify-merge.sh。
#   - --bare / --setting-sources user / --strict-mcp-config：不加载仓库里的 hooks、
#     CLAUDE.md、项目级权限和 MCP 配置。
#   - 读写仅限仓库目录，禁止改 .github/、.fork/、.git/；调用方结束后会再次校验。

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

MODE="${1:?mode (conflicts|fix) is required}"
BASE_REF="${2:?base ref is required}"
MERGE_TARGET="${3:?merge target is required}"
VERIFY_LOG="${4:-}"

API_KEY="${DEEPSEEK_API_KEY:?DEEPSEEK_API_KEY is required}"
unset DEEPSEEK_API_KEY
MODEL="${AI_MERGE_MODEL:-deepseek-v4-pro}"
FAST_MODEL="${AI_MERGE_FAST_MODEL:-deepseek-flash}"
BASE_URL="${AI_BASE_URL:-https://api.deepseek.com/anthropic}"
TIMEOUT="${AI_TIMEOUT:-50m}"

WORK="$(mktemp -d "${RUNNER_TEMP:-/tmp}/fork-ai.XXXXXX")"
TOOLS="$WORK/tools"
mkdir -p "$TOOLS"

if [[ -n "${CLAUDE_BIN:-}" ]]; then
  CLAUDE="$CLAUDE_BIN"
else
  npm install --prefix "$WORK/cli" --no-audit --no-fund --loglevel=error \
    @anthropic-ai/claude-code >/dev/null
  CLAUDE="$WORK/cli/node_modules/.bin/claude"
fi
echo "claude: $("$CLAUDE" --version 2>/dev/null || echo unknown), model: ${MODEL}"

# 包装脚本只继承构建所需的环境变量，密钥与 token 一律不传
env_args=""
for name in HOME PATH LANG LC_ALL TMPDIR GOPATH GOCACHE GOMODCACHE GOROOT GOTOOLCHAIN \
  GOFLAGS GOPROXY GOSUMDB PNPM PNPM_HOME CI; do
  if [[ -n "${!name:-}" ]]; then
    env_args="${env_args} $(printf '%q' "${name}=${!name}")"
  fi
done
quoted_root="$(printf '%q' "$ROOT")"

cat >"$TOOLS/git-ro" <<EOF
#!/usr/bin/env bash
case "\${1:-}" in
  diff|log|show|status|merge-base|ls-files|blame|rev-parse) ;;
  *) echo "git-ro: only diff/log/show/status/merge-base/ls-files/blame/rev-parse are allowed" >&2; exit 2 ;;
esac
cd ${quoted_root}
exec env -i${env_args} git --no-pager -c core.hooksPath=/dev/null "\$@"
EOF

cat >"$TOOLS/verify" <<EOF
#!/usr/bin/env bash
case "\${1:-}" in
  ""|--quick) ;;
  *) echo "verify: only --quick is supported" >&2; exit 2 ;;
esac
cd ${quoted_root}
exec env -i${env_args} bash .fork/verify-merge.sh "\$@"
EOF

chmod 555 "$TOOLS/git-ro" "$TOOLS/verify"
chmod 555 "$TOOLS"

git_ro="$TOOLS/git-ro"
verify="$TOOLS/verify"

{
  cat <<EOF
You are resolving an automated merge of the upstream project Wei-Shaw/sub2api into the
fork SakurajimMai/sub2api. The working directory is the fork repository with the merge
in progress.

- Fork side (HEAD): ${BASE_REF}
- Upstream side (MERGE_HEAD): ${MERGE_TARGET}

Goal: a merge result that keeps every upstream change AND every fork-only feature, and
that passes the project checks.

EOF

  if [[ "$MODE" == "conflicts" ]]; then
    echo "These files still contain unresolved conflicts:"
    git diff --name-only --diff-filter=U | sed 's/^/- /'
    cat <<'EOF'

For a modify/delete conflict, either keep the file with the correct content or delete it.
EOF
  else
    cat <<'EOF'
Git merged without remaining textual conflicts, but verification failed. This is usually
a semantic conflict: e.g. upstream changed a function signature, struct or interface that
fork-only code still uses the old way. Verification output:

```
EOF
    tail -n 200 "${VERIFY_LOG:?verify log is required for fix mode}"
    echo '```'
  fi

  cat <<EOF

Tools:
- Read / Glob / Grep / Edit / Write inside the repository.
- \`${git_ro} <diff|log|show|status|merge-base|ls-files|blame|rev-parse> ...\` — read-only git. Useful:
  - \`${git_ro} log --oneline -p HEAD..MERGE_HEAD -- <file>\` — upstream changes to a file
  - \`${git_ro} log --oneline -p MERGE_HEAD..HEAD -- <file>\` — fork-only changes to a file
  - \`${git_ro} show :1:<file>\` / \`:2:<file>\` / \`:3:<file>\` — base / fork / upstream version of a conflicted file
- \`${verify} --quick\` — regenerates ent/wire code, builds and vets the backend, typechecks the frontend.
- \`${verify}\` — full check: additionally backend unit tests, frontend lint and critical tests.

Rules:
1. Conflict markers are diff3 style: \`<<<<<<< HEAD\` (fork) / \`|||||||\` (merge base) /
   \`=======\` / \`>>>>>>>\` (upstream). Remove every marker.
2. Take upstream's code for upstream's changes and re-apply the fork's additions on top.
   When both sides add fields, parameters, imports, routes, providers, i18n keys, etc.,
   keep both. Leave upstream lines untouched where possible and put fork-only additions
   in separate lines/blocks, so future upstream merges conflict less.
3. When upstream changed an API that fork-only code uses, update the fork code to the new
   upstream API. Never revert an upstream change just to make fork code compile.
4. Never edit generated files by hand ("// Code generated ... DO NOT EDIT.": backend/ent/**
   except backend/ent/schema/**, and backend/cmd/server/wire_gen.go). The verify tool
   regenerates them from their sources (ent schema, wire.go and provider sets).
5. Do not touch .github/, .fork/ or .git/. Do not add dependencies.
6. Run \`${verify} --quick\` until it passes, then run \`${verify}\` once. Fix every failure
   caused by the merge, including in files that had no textual conflict.
7. If you cannot resolve a conflict with confidence, leave its conflict markers in place
   (the automation then stops for a human) and explain why.

Finish with a short summary: for each file you changed, how and why.
EOF
} >"$WORK/prompt.md"

set +e
env -u GH_TOKEN -u GITHUB_TOKEN \
  ANTHROPIC_BASE_URL="$BASE_URL" \
  ANTHROPIC_API_KEY="$API_KEY" \
  ANTHROPIC_MODEL="$MODEL" \
  ANTHROPIC_DEFAULT_OPUS_MODEL="$MODEL" \
  ANTHROPIC_DEFAULT_SONNET_MODEL="$MODEL" \
  ANTHROPIC_DEFAULT_HAIKU_MODEL="$FAST_MODEL" \
  CLAUDE_CODE_SUBAGENT_MODEL="$FAST_MODEL" \
  API_TIMEOUT_MS=600000 \
  CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1 \
  DISABLE_AUTOUPDATER=1 \
  timeout "$TIMEOUT" "$CLAUDE" -p \
    --bare --setting-sources user --strict-mcp-config --no-session-persistence \
    --model "$MODEL" --permission-mode dontAsk \
    --allowedTools "Read(./**)" "Glob" "Grep" "Edit(./**)" "Write(./**)" "TodoWrite" \
      "Bash(${git_ro}:*)" "Bash(${verify}:*)" \
    --disallowedTools "Edit(.github/**)" "Edit(.fork/**)" "Edit(.git/**)" \
      "Write(.github/**)" "Write(.fork/**)" "Write(.git/**)" \
      "WebFetch" "WebSearch" "Task" \
    <"$WORK/prompt.md"
status=$?
set -e

chmod -R u+w "$WORK" 2>/dev/null || true
rm -rf "$WORK"

if [[ $status -ne 0 ]]; then
  echo "error: claude exited with status ${status}" >&2
fi
exit "$status"

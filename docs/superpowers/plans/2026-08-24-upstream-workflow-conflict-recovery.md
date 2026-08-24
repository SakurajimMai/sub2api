# Upstream Workflow Conflict Recovery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 让 Mirror/Sync 在仅有 fork 自管 workflow 冲突时自动完成合并，并在真实业务冲突时输出诊断后安全失败，不再创建空分支或 PR。

**Architecture:** 新增一个只负责“开始合并、恢复 `.github/workflows/**`、判断剩余冲突、完成合并提交”的 Bash 辅助脚本。Mirror 和 Sync 复用该脚本；Sync 在脚本报告剩余冲突后继续保留既有的 VERSION-only 自动解决特例。最终合并提交继续由现有恢复脚本和 overlay 修订，确保推送历史不含上游 workflow 变更。

**Tech Stack:** Bash、Git、GitHub Actions YAML、PowerShell/WSL Bash 测试入口、GitHub CLI。

---

## 文件结构

- 创建 `.fork/merge-upstream-preserving-workflows.sh`：统一处理上游合并及 fork workflow 冲突。
- 修改 `.fork/tests/protect-workflows-test.sh`：增加冲突合并、业务冲突和 YAML 契约测试。
- 修改 `.github/workflows/mirror-upstream-release.yml`：调用统一脚本，删除空分支和 PR fallback。
- 修改 `.github/workflows/sync-upstream.yml`：调用统一脚本，保留 VERSION-only 自动解决，删除空冲突分支。

### Task 1: 用失败测试定义合并辅助脚本契约

**Files:**
- Modify: `.fork/tests/protect-workflows-test.sh`
- Test: `.fork/tests/protect-workflows-test.sh`

- [ ] **Step 1: 增加静态 workflow 契约断言**

在 `require_workflow_guard` 中要求两个 workflow 调用新脚本，并在文件尾增加禁止旧 fallback 的断言：

```bash
grep -Fq './.fork/merge-upstream-preserving-workflows.sh' "$ROOT/$workflow" \
  || fail "$workflow 未使用统一的 workflow 冲突恢复脚本"

if grep -Fq 'gh pr create' "$ROOT/.github/workflows/mirror-upstream-release.yml"; then
  fail "mirror workflow 不应再创建无内容冲突 PR"
fi
if grep -Fq 'git commit --allow-empty' "$ROOT/.github/workflows/sync-upstream.yml"; then
  fail "sync workflow 不应再创建空冲突分支"
fi
```

- [ ] **Step 2: 增加 workflow 内容冲突自动恢复用例**

在临时 Git 仓库中让 fork 和 upstream 修改同一 workflow 行，同时让 upstream 修改 `app.txt`。复制两个 `.fork` 脚本后执行：

```bash
bash ./.fork/merge-upstream-preserving-workflows.sh \
  "$base" "test: merge workflow conflict" upstream
```

断言 `git diff --name-only --diff-filter=U` 为空、workflow 与 `$base` 一致、`app.txt` 保留上游内容、`HEAD` 有两个父提交。

- [ ] **Step 3: 增加真实业务冲突不被吞掉用例**

构造 `app.txt` 同行冲突并执行辅助脚本，捕获退出码：

```bash
set +e
bash ./.fork/merge-upstream-preserving-workflows.sh \
  "$base" "test: keep business conflict" upstream
status=$?
set -e
[[ "$status" -eq 2 ]] || fail "业务冲突应返回退出码 2，实际为 $status"
git diff --name-only --diff-filter=U | grep -Fxq 'app.txt' \
  || fail "业务冲突文件必须保持未解决"
git diff --name-only --diff-filter=U | grep -Fq '.github/workflows/' \
  && fail "fork workflow 冲突应已解决"
git merge --abort
```

- [ ] **Step 4: 运行测试确认按预期失败**

Run: `bash .fork/tests/protect-workflows-test.sh`

Expected: FAIL，提示缺少 `.fork/merge-upstream-preserving-workflows.sh` 或 workflow 尚未调用该脚本。

### Task 2: 实现可复用的 workflow 冲突恢复脚本

**Files:**
- Create: `.fork/merge-upstream-preserving-workflows.sh`
- Modify: `.fork/tests/protect-workflows-test.sh`
- Test: `.fork/tests/protect-workflows-test.sh`

- [ ] **Step 1: 实现参数和前置校验**

脚本接受 `BASE_REF`、`MERGE_MESSAGE`、`MERGE_TARGET`：

```bash
#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

BASE_REF="${1:?fork base ref is required}"
MERGE_MESSAGE="${2:?merge message is required}"
MERGE_TARGET="${3:?merge target is required}"

git rev-parse --verify "${BASE_REF}^{commit}" >/dev/null
git rev-parse --verify "${MERGE_TARGET}^{commit}" >/dev/null
```

- [ ] **Step 2: 实现 clean merge 与 workflow 冲突恢复**

```bash
if git merge --no-ff --no-edit -m "$MERGE_MESSAGE" "$MERGE_TARGET"; then
  echo "Merge completed without conflicts."
  exit 0
fi

if ! git rev-parse -q --verify MERGE_HEAD >/dev/null; then
  echo "error: merge failed before entering a resolvable conflict state" >&2
  exit 1
fi

"$ROOT/.fork/restore-managed-workflows.sh" "$BASE_REF"
mapfile -t conflicts < <(git diff --name-only --diff-filter=U)
if [[ ${#conflicts[@]} -gt 0 ]]; then
  printf 'Remaining non-workflow conflicts:\n' >&2
  printf '  %s\n' "${conflicts[@]}" >&2
  exit 2
fi

git commit --no-edit
echo "Merge completed after restoring fork-managed workflows."
```

- [ ] **Step 3: 赋予可执行位并运行聚焦测试**

Run:

```bash
chmod +x .fork/merge-upstream-preserving-workflows.sh
bash .fork/tests/protect-workflows-test.sh
```

Expected: 静态 workflow 契约仍失败，但新增脚本的 synthetic merge 测试通过。

### Task 3: 接入 Mirror 和 Sync workflow

**Files:**
- Modify: `.github/workflows/mirror-upstream-release.yml`
- Modify: `.github/workflows/sync-upstream.yml`
- Test: `.fork/tests/protect-workflows-test.sh`

- [ ] **Step 1: 修改 Mirror 合并路径**

把内联 `git merge` 和冲突 PR 分支替换为：

```bash
chmod +x .fork/merge-upstream-preserving-workflows.sh \
  .fork/restore-managed-workflows.sh .fork/apply-overlay.sh

set +e
./.fork/merge-upstream-preserving-workflows.sh \
  origin/main \
  "chore(sync): merge upstream ${TAG} for mirrored release" \
  "refs/tags/upstream-mirror/${TAG}"
merge_status=$?
set -e

if [[ $merge_status -ne 0 ]]; then
  mapfile -t conflicts < <(git diff --name-only --diff-filter=U || true)
  {
    echo "## Mirror blocked by non-workflow conflicts"
    echo
    echo "Upstream: ${TAG} @ ${UP_COMMIT}"
    printf -- '- `%s`\n' "${conflicts[@]:-unknown}"
  } >> "$GITHUB_STEP_SUMMARY"
  git merge --abort || true
  exit "$merge_status"
fi
```

同时删除 `pull-requests: write`、`GH_TOKEN`、冲突分支 push、`gh pr list/edit/create`。

- [ ] **Step 2: 修改 Sync 合并路径并保留 VERSION 特例**

调用相同脚本。返回 `2` 后重新读取剩余冲突；若唯一冲突为 `backend/cmd/server/VERSION`，执行 `git checkout --theirs`、`git add`、`git commit --no-edit`。否则写入 `GITHUB_STEP_SUMMARY`，设置 `has_conflicts=true`，abort 后退出该 step，让下一步显式失败。

- [ ] **Step 3: 更新失败步骤文本**

Sync 的失败步骤只报告真实剩余文件，不再引用 `conflict_branch`：

```bash
echo "::error::Upstream merge has non-workflow conflicts: ${{ steps.merge.outputs.conflict_files }}"
echo "See the job summary for local resolution commands."
exit 1
```

- [ ] **Step 4: 运行完整脚本测试**

Run: `bash .fork/tests/protect-workflows-test.sh`

Expected: `PASS: 上游 workflow 变更被隔离，业务代码变更被保留`，退出码 0。

### Task 4: 用真实 v0.1.180 标签复现并验证

**Files:**
- Test: `.fork/tests/protect-workflows-test.sh`
- Verify: `.github/workflows/mirror-upstream-release.yml`

- [ ] **Step 1: 在临时克隆中模拟真实 Mirror 合并**

Run:

```powershell
$tmp = Join-Path ([System.IO.Path]::GetTempPath()) ("sub2api-mirror-" + [guid]::NewGuid())
git clone --no-hardlinks . $tmp
git -C $tmp config user.name "Mirror test"
git -C $tmp config user.email "mirror-test@example.invalid"
git -C $tmp update-ref refs/tags/upstream-mirror/v0.1.180 (git rev-parse refs/tags/upstream-mirror/v0.1.180)
bash -lc "cd '$($tmp -replace '\\','/')' && ./.fork/merge-upstream-preserving-workflows.sh HEAD 'test: mirror v0.1.180' refs/tags/upstream-mirror/v0.1.180"
```

Expected: 合并成功；`git -C $tmp diff --name-only --diff-filter=U` 为空；workflow 目录相对临时克隆合并前基线无差异；合并提交有两个父提交。

- [ ] **Step 2: 运行静态校验**

Run:

```powershell
git diff --check
bash .fork/tests/protect-workflows-test.sh
git diff -- .github/workflows .fork
```

Expected: 无 whitespace 错误，测试退出码 0，差异只包含计划文件。

- [ ] **Step 3: 提交 Actions 修复**

```bash
git add .fork/merge-upstream-preserving-workflows.sh .fork/tests/protect-workflows-test.sh \
  .github/workflows/mirror-upstream-release.yml .github/workflows/sync-upstream.yml
git commit -m "fix(ci): recover fork workflow merge conflicts"
```

### Task 5: 推送并完成远端运行验证

**Files:**
- Verify only

- [ ] **Step 1: 推送 main**

Run: `git push origin main`

Expected: 推送成功，远端 `main` 包含设计和 Actions 修复提交。

- [ ] **Step 2: 从新提交发起 Mirror v0.1.180**

Run:

```bash
gh workflow run mirror-upstream-release.yml --repo SakurajimMai/sub2api --ref main \
  -f tag=v0.1.180 -f dry_run=false
```

通过 `gh run list` 找到新 run，并用 `gh run watch <run-id> --exit-status` 等待。

Expected: Mirror 成功合并 v0.1.180、推送 tag 并 dispatch Release；日志包含 `Merge completed after restoring fork-managed workflows.`，不出现 `createPullRequest` 权限错误。

- [ ] **Step 3: 同步本地并发起全新 Sync 运行**

Run:

```bash
git fetch origin main --prune
git pull --ff-only origin main
gh workflow run sync-upstream.yml --repo SakurajimMai/sub2api --ref main -f dry_run=false
```

Expected: Sync 新 run 成功或明确报告已经同步；不得重跑旧 run。

- [ ] **Step 4: 核对远端状态**

Run:

```bash
git fetch origin main --prune
git rev-list --left-right --count HEAD...origin/main
gh run list --repo SakurajimMai/sub2api --limit 8 \
  --json databaseId,workflowName,status,conclusion,headSha,url
```

Expected: 本地与远端为 `0 0`；新 Mirror 和 Sync 运行均有可追溯成功结果。

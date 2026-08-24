# 上游同步 Workflow 冲突恢复设计

## 背景

本 fork 把 `.github/workflows/**` 作为自主管理边界。自动同步上游时，合并完成后会从 `origin/main` 恢复整个 workflow 目录，再修订合并提交，避免 `GITHUB_TOKEN` 推送包含 workflow 变更的历史。

v0.1.180 同时修改了三个 fork workflow，Git 在合并阶段先产生内容冲突。现有恢复脚本只在 clean merge 后运行，因此工作流直接中止并创建空冲突分支；随后 `gh pr create` 又因 GitHub App 没有创建 PR 的权限而失败。

## 目标

- 上游变更只与 fork 自管 workflow 冲突时，自动保留 fork 版本并继续合并。
- 同一逻辑同时用于 `Mirror Upstream Release` 和 `Sync Upstream`。
- 存在业务文件冲突时停止，不自动选择任一方，不制造空提交或无内容 PR。
- 自动推送的提交历史不包含上游 workflow 变更。
- 给人工处理者输出明确的剩余冲突文件和复现命令。

## 方案

新增可测试的 `.fork/merge-upstream-preserving-workflows.sh`，统一封装两个 workflow 的合并行为：

1. 使用 `git merge --no-ff --no-edit` 开始合并。
2. clean merge 时直接返回成功，后续仍执行现有 workflow 恢复和 overlay。
3. merge 返回冲突时，保持未完成合并状态，不立即 `git merge --abort`。
4. 调用 `.fork/restore-managed-workflows.sh origin/main`，把 workflow 目录的索引和工作树恢复为 fork 基线，从而解决该目录内的内容、删除/修改和新增冲突。
5. 检查 `git diff --name-only --diff-filter=U`。
6. 若已无未解决文件，执行 `git commit --no-edit` 完成双父合并并返回成功。
7. 若仍有非 workflow 冲突，打印文件列表并返回专用非零状态；调用方随后 abort，写入 GitHub Step Summary 并让任务失败。

成功合并后继续执行现有流程：恢复 fork workflow、应用品牌 overlay、暂存所有变更，并在需要时 `git commit --amend --no-edit`。这确保最终合并提交保留双父历史，但其树中的 `.github/workflows/**` 与合并前 `origin/main` 完全一致。

## PR 权限处理

删除自动创建 `mirror/<tag>-conflicts` 空分支和 `gh pr create` 流程，同时移除不再需要的 `pull-requests: write` 权限。

Git 无法把未解决索引推送到远端，当前空分支也不包含可在 PR 页面解决的冲突，因此该 PR fallback 没有实际修复价值。遇到真实业务冲突时，任务摘要提供：

- 上游 tag 或 commit。
- 所有剩余冲突文件。
- 本地 fetch、merge、解决冲突、应用 overlay、推送和触发 release 的命令。

任务保持失败状态，确保冲突不会被误认为已同步成功。

## 测试

扩展 `.fork/tests/protect-workflows-test.sh`，覆盖：

- clean merge 中上游修改、删除和新增 workflow，最终仍保留 fork 基线。
- workflow 同一行内容冲突，辅助脚本自动解决并生成双父合并提交。
- workflow 冲突与业务文件 clean 变更并存，业务变更保留。
- workflow 冲突与业务文件冲突并存，只解决 workflow，辅助脚本返回失败并报告业务冲突。
- 最终提交相对 fork 基线在 `.github/workflows/**` 无差异。
- mirror 与 sync 两个 YAML 都调用统一辅助脚本，且不再调用 `gh pr create`。

## 验收标准

- 使用 v0.1.180 对应提交复现时，三个 workflow 冲突被自动保留为 fork 版本，合并继续完成。
- `main` 推送不再触发 GitHub App workflow 权限拒绝。
- 不再出现 `createPullRequest: Resource not accessible by integration`。
- 非 workflow 冲突仍安全失败，并在任务摘要中给出可操作诊断。
- 新提交推送后，必须从 `main` 发起全新的 Mirror 和 Sync 运行验证，不能重跑旧 workflow run。

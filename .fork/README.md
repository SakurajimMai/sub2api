# Fork maintenance (SakurajimMai/sub2api)

This fork tracks upstream [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) while keeping local customizations.

## What gets protected

| Kind | How it survives upstream sync |
|------|-------------------------------|
| **Product features** (e.g. custom menu open modes) | Normal git history on `main`. Merge brings upstream commits; your commits stay. |
| **Branding / deploy identity** | Re-applied after every sync by [apply-overlay.sh](./apply-overlay.sh) from [overlay.conf](./overlay.conf). |
| **GitHub Actions 工作流** | 合并后由 [restore-managed-workflows.sh](./restore-managed-workflows.sh) 恢复为 fork 基线，再修订原 merge commit。 |

`.github/workflows/**` 属于 fork 自主管理边界。自动同步不会直接引入上游工作流变更；此类变更必须人工审查后单独合并。这样既避免内置 `GITHUB_TOKEN` 因缺少 `Workflows` 权限而拒绝推送，也避免未经审查的上游 Actions 获得 fork Secrets 的执行机会。

Branding currently re-stamps:

- Docker Hub image: `sakurajiamai/sub2api` (was `weishaw/sub2api`)
- GitHub owner/repo links & install sources: `SakurajimMai/sub2api`
- In-app update check (`backend/internal/service/update_service.go` `githubRepo`) → fork releases only
- Paths listed in `OVERLAY_PATHS` inside `overlay.conf`
- Star History badges are left on **upstream** metrics (by design)

> **Important:** UI “立即更新” downloads GitHub Release assets from **this fork**. If `githubRepo` still pointed at Wei-Shaw, an update would install upstream binaries and wipe fork customizations.

Secrets such as `DOCKERHUB_USERNAME` / `DOCKERHUB_TOKEN` live in GitHub Actions secrets and are **not** part of this overlay.

## Automatic code sync (main)

Workflow: [`.github/workflows/sync-upstream.yml`](../.github/workflows/sync-upstream.yml)

| Trigger | Behavior |
|---------|----------|
| Daily schedule (03:15 UTC) | If fork is behind `upstream/main`, **merge + overlay + push `main`** |
| Manual **Run workflow** | Same; optional dry-run |

Merges that pass the verify gate update `main` directly (no PR — avoids GITHUB_TOKEN createPullRequest limits). Fork-managed workflows are restored and branding is re-applied before the merge commit is amended and pushed. If a conflict cannot be resolved automatically or the gate fails, the job fails, lists the files in the job summary and leaves `main` untouched.

## Auto-merge pipeline

Both Sync Upstream and Mirror Upstream Release call [merge-upstream-auto.sh](./merge-upstream-auto.sh):

1. `git merge` via [merge-upstream-preserving-workflows.sh](./merge-upstream-preserving-workflows.sh) (fork workflows restored)
2. [auto-resolve-conflicts.sh](./auto-resolve-conflicts.sh) resolves mechanical conflicts:
   - `backend/cmd/server/VERSION` → upstream version
   - `backend/go.sum` → union of both sides
   - generated `backend/ent/**` (not `ent/schema`) and `backend/cmd/server/wire_gen.go` → regenerated
3. Remaining conflicts → [ai-resolve-conflicts.sh](./ai-resolve-conflicts.sh) (DeepSeek via Claude Code CLI), only when the `DEEPSEEK_API_KEY` secret is set
4. [verify-merge.sh](./verify-merge.sh) gate: regenerate ent/wire, `go build`, `go vet` (unit + integration tags), backend unit tests, frontend lint/typecheck/critical vitest. This also catches *semantic* conflicts git cannot see (e.g. upstream changed a signature that fork-only code still calls the old way). If it fails and AI is enabled, the failure log goes to the AI for up to `AI_MERGE_ROUNDS` (default 2) fix rounds.
5. Only a merge that passes the gate is committed; anything else resets to the pre-merge HEAD. AI-touched files are listed in the merge commit message and the job summary.

Exit codes: `0` merged (or already up to date), `2` conflicts need a human, `3` gate failed, `1` other error.

### AI resolution (DeepSeek)

| Setting | Where | Value |
|---------|-------|-------|
| `DEEPSEEK_API_KEY` | Settings → Secrets and variables → Actions → **Secrets** | DeepSeek API key. Unset = no AI (rules + gate only) |
| `AI_MERGE_MODEL` | Settings → Secrets and variables → Actions → **Variables** (optional) | Default `deepseek-v4-pro`; e.g. `deepseek-flash` is cheaper |

AI-resolved merges go straight to `main` and are released like any other merge, gated only by `verify-merge.sh`. Review the "AI-assisted resolution" section of such merge commits after the fact.

Upstream code is untrusted input to the AI (prompt injection), so the AI runs sandboxed:

- The key is given to the `claude` process only — never exported to the verify gate, which executes upstream code — and the step that holds it has no GitHub token (checkout uses `persist-credentials: false`; only the final push step gets `GITHUB_TOKEN`).
- `--bare --setting-sources user --strict-mcp-config`: no hooks, CLAUDE.md, project permissions or MCP servers from the repo; `--permission-mode dontAsk`: anything not allow-listed is denied.
- Read/Edit/Write only inside the repo, never `.github/`, `.fork/`, `.git/` (re-checked after every AI run); no web access.
- The only commands the AI can run are two read-only wrappers outside the repo (`git` read subcommands and `verify-merge.sh`), both started with `env -i`.

`.fork/tests/protect-workflows-test.sh` fails CI if any of these guards is removed.

Resolve a blocked merge locally:

```bash
git fetch upstream main
./.fork/merge-upstream-auto.sh origin/main "chore(sync): merge upstream/main" upstream/main
# exit 2 → the merge was rolled back; redo it by hand:
git merge upstream/main              # stops with conflicts
./.fork/restore-managed-workflows.sh origin/main
./.fork/auto-resolve-conflicts.sh    # prints the files left for you; fix and `git add` them
./.fork/verify-merge.sh              # PNPM="npx -y pnpm@9" if local pnpm is not v9
./.fork/apply-overlay.sh
git add -A && git commit --no-edit
```

To keep future merges conflict-free, put fork-only additions in separate lines/blocks (e.g. a separate gofmt section in a struct) instead of editing upstream lines.

## Automatic release mirror (tags + images)

Workflow: [`.github/workflows/mirror-upstream-release.yml`](../.github/workflows/mirror-upstream-release.yml)

| Trigger | Behavior |
|---------|----------|
| Every 2 hours | If upstream has a new `vX.Y.Z` tag missing on this fork → merge that release into `main`, overlay branding, create the **same tag**, dispatch **Release** |
| Manual **Run workflow** | Optional specific tag; optional dry-run |

Pipeline:

```
upstream tag v0.1.171
    → merge into main + apply-overlay
    → git tag v0.1.171 + push
    → workflow_dispatch Release
    → sakurajiamai/sub2api:0.1.171 + :latest
    → ghcr.io/sakurajimmai/sub2api:0.1.171 + :latest
```

Notes:

- Only stable tags matching `vMAJOR.MINOR.PATCH` (no `-rc` / `-beta`).
- One missing tag per run (newest first).
- `GITHUB_TOKEN` tag pushes do not auto-trigger other workflows, so Release is started via `workflow_dispatch`.
- Uses the same [auto-merge pipeline](#auto-merge-pipeline); unresolvable conflicts or a failed gate stop before tagging. If the release was merged by hand, re-running the workflow only tags and releases.
- Shares a concurrency lock with Sync Upstream (`fork-main-mutation`).
- After every Mirror / Sync / CI push, [cleanup-workflow-runs.sh](./cleanup-workflow-runs.sh) keeps only the newest **10 runs per workflow** so the Actions tab does not accumulate hundreds of scheduled no-op runs.

## Manual commands

```bash
# Remotes (once)
git remote add upstream https://github.com/Wei-Shaw/sub2api.git   # if missing
git fetch upstream main

# Preview how far behind you are
git rev-list --count HEAD..upstream/main

# Merge upstream into a branch
git checkout main
git pull origin main
git checkout -b sync/upstream-manual
git merge upstream/main

# Re-apply branding (always run after merge)
./.fork/apply-overlay.sh

# Optional: fail CI/local if branding drifted
./.fork/apply-overlay.sh --check
```

On Windows, run the shell scripts from **Git Bash** or WSL (not plain PowerShell).

## Adding new fork-only changes

1. **Code/features** — commit them on `main` as usual. Prefer isolated commits so merges are easier to read.
2. **More branding paths** — edit `OVERLAY_PATHS` in [overlay.conf](./overlay.conf) and extend [apply-overlay.sh](./apply-overlay.sh) if you need special cases.
3. **Never** put tokens or passwords in `.fork/`.

## Release images

With Actions secrets:

- `DOCKERHUB_USERNAME` = `sakurajiamai`
- `DOCKERHUB_TOKEN` = Docker Hub access token

Sources of releases:

1. **Auto** — Mirror Upstream Release when Wei-Shaw publishes `v*`
2. **Manual tag** — `git tag -a vX.Y.Z && git push origin vX.Y.Z`
3. **Manual Actions** — Release → Run workflow with a tag

Published images:

- `sakurajiamai/sub2api:latest` (and version tags)
- `ghcr.io/sakurajimmai/sub2api:latest` (owner lowercased)

# Fork maintenance (SakurajimMai/sub2api)

This fork tracks upstream [Wei-Shaw/sub2api](https://github.com/Wei-Shaw/sub2api) while keeping local customizations.

## What gets protected

| Kind | How it survives upstream sync |
|------|-------------------------------|
| **Product features** (e.g. custom menu open modes) | Normal git history on `main`. Merge brings upstream commits; your commits stay. |
| **Branding / deploy identity** | Re-applied after every sync by [apply-overlay.sh](./apply-overlay.sh) from [overlay.conf](./overlay.conf). |

Branding currently re-stamps:

- Docker Hub image: `sakurajiamai/sub2api` (was `weishaw/sub2api`)
- GitHub owner/repo links & install sources: `SakurajimMai/sub2api`
- Paths listed in `OVERLAY_PATHS` inside `overlay.conf`
- Star History badges are left on **upstream** metrics (by design)

Secrets such as `DOCKERHUB_USERNAME` / `DOCKERHUB_TOKEN` live in GitHub Actions secrets and are **not** part of this overlay.

## Automatic code sync (main)

Workflow: [`.github/workflows/sync-upstream.yml`](../.github/workflows/sync-upstream.yml)

| Trigger | Behavior |
|---------|----------|
| Daily schedule (03:15 UTC) | If fork is behind `upstream/main`, **merge + overlay + push `main`** |
| Manual **Run workflow** | Same; optional dry-run |

Clean merges update `main` directly (no PR — avoids GITHUB_TOKEN createPullRequest limits). Branding is re-applied after every merge. On conflicts, branch `sync/upstream-<sha>-conflicts` is pushed and the job fails for manual fix.

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
- Merge conflicts open a `[CONFLICTS]` PR instead of tagging.
- Shares a concurrency lock with Sync Upstream (`fork-main-mutation`).

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

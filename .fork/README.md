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

## Automatic sync

Workflow: [`.github/workflows/sync-upstream.yml`](../.github/workflows/sync-upstream.yml)

| Trigger | Behavior |
|---------|----------|
| Daily schedule (03:15 UTC) | If fork is behind `upstream/main`, merge + overlay + open PR |
| Manual **Run workflow** | Same; optional dry-run |

The workflow **does not push straight to `main`**. It opens a PR (`sync/upstream-<sha>`) so you can review.

If git reports conflicts, the workflow opens a **placeholder PR** titled `[CONFLICTS] ...` (works even when GitHub Issues are disabled). Resolve locally, run the overlay again, push a real merge branch, then merge.

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

tagging `v*` runs the existing [release](../.github/workflows/release.yml) workflow and publishes:

- `sakurajiamai/sub2api:latest` (and version tags)
- `ghcr.io/sakurajimmai/sub2api:latest` (owner lowercased)

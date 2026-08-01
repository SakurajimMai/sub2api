#!/usr/bin/env bash
# Re-apply fork branding after merging upstream.
# Idempotent: safe to run multiple times.
#
# Usage:
#   ./.fork/apply-overlay.sh
#   ./.fork/apply-overlay.sh --check   # exit 1 if branding would change

set -euo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"

CONFIG="${ROOT}/.fork/overlay.conf"
if [[ ! -f "$CONFIG" ]]; then
  echo "error: missing ${CONFIG}" >&2
  exit 1
fi

# shellcheck disable=SC1090
source "$CONFIG"

: "${UPSTREAM_GITHUB_OWNER:?}"
: "${UPSTREAM_GITHUB_REPO:?}"
: "${UPSTREAM_DOCKERHUB_USER:?}"
: "${FORK_GITHUB_OWNER:?}"
: "${FORK_GITHUB_REPO:?}"
: "${FORK_DOCKERHUB_USER:?}"
: "${OVERLAY_PATHS:?}"
RESTORE_UPSTREAM_STAR_HISTORY="${RESTORE_UPSTREAM_STAR_HISTORY:-true}"

CHECK_ONLY=false
if [[ "${1:-}" == "--check" ]]; then
  CHECK_ONLY=true
fi

to_lower() {
  echo "$1" | tr '[:upper:]' '[:lower:]'
}

UP_OWNER_LOWER="$(to_lower "$UPSTREAM_GITHUB_OWNER")"
FORK_OWNER_LOWER="$(to_lower "$FORK_GITHUB_OWNER")"

# Collect text-like targets; skip fixtures/binaries.
mapfile -t TARGET_FILES < <(
  while IFS= read -r entry; do
    entry="$(echo "$entry" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')"
    [[ -z "$entry" || "$entry" == \#* ]] && continue
    if [[ -f "$entry" ]]; then
      printf '%s\n' "$entry"
    elif [[ -d "$entry" ]]; then
      find "$entry" -type f \
        ! -path '*/tests/fixtures/*' \
        ! -path '*/node_modules/*' \
        ! -path '*/.git/*' \
        \( \
          -name '*.md' -o -name '*.yml' -o -name '*.yaml' -o -name '*.sh' \
          -o -name '*.example' -o -name '*.toml' -o -name '*.json' \
          -o -name '*.service' -o -name '*.tmpl' -o -name 'Dockerfile*' \
          -o -name '.env*' -o -name 'Caddyfile' -o -name 'Makefile' \
          -o -name '*.vue' -o -name '*.ts' -o -name '*.go' -o -name '*.txt' \
        \)
    else
      echo "warn: overlay path not found: $entry" >&2
    fi
  done <<< "$OVERLAY_PATHS" | sort -u
)

if [[ ${#TARGET_FILES[@]} -eq 0 ]]; then
  echo "error: no overlay target files found" >&2
  exit 1
fi

# Transform file contents on stdout (does not touch the original).
transform() {
  local file="$1"
  # Branding only for this app repo — never rewrite Wei-Shaw/model-price-repo.
  local cmd=(
    sed
    -e "s|${UPSTREAM_DOCKERHUB_USER}/${UPSTREAM_GITHUB_REPO}|${FORK_DOCKERHUB_USER}/${FORK_GITHUB_REPO}|g"
    -e "s|${UPSTREAM_GITHUB_OWNER}/${UPSTREAM_GITHUB_REPO}|${FORK_GITHUB_OWNER}/${FORK_GITHUB_REPO}|g"
    -e "s|github.com/${UP_OWNER_LOWER}/${UPSTREAM_GITHUB_REPO}|github.com/${FORK_GITHUB_OWNER}/${FORK_GITHUB_REPO}|g"
    -e "s|raw.githubusercontent.com/${UP_OWNER_LOWER}/${UPSTREAM_GITHUB_REPO}|raw.githubusercontent.com/${FORK_GITHUB_OWNER}/${FORK_GITHUB_REPO}|g"
  )

  if [[ "$RESTORE_UPSTREAM_STAR_HISTORY" == "true" ]]; then
    cmd+=(
      -e "s|star-history.com/#${FORK_GITHUB_OWNER}/${FORK_GITHUB_REPO}|star-history.com/#${UPSTREAM_GITHUB_OWNER}/${UPSTREAM_GITHUB_REPO}|g"
      -e "s|star-history.com/svg?repos=${FORK_GITHUB_OWNER}/${FORK_GITHUB_REPO}|star-history.com/svg?repos=${UPSTREAM_GITHUB_OWNER}/${UPSTREAM_GITHUB_REPO}|g"
      -e "s|api.star-history.com/svg?repos=${FORK_GITHUB_OWNER}/${FORK_GITHUB_REPO}|api.star-history.com/svg?repos=${UPSTREAM_GITHUB_OWNER}/${UPSTREAM_GITHUB_REPO}|g"
      -e "s|star-history.com/#${FORK_OWNER_LOWER}/${FORK_GITHUB_REPO}|star-history.com/#${UPSTREAM_GITHUB_OWNER}/${UPSTREAM_GITHUB_REPO}|g"
      -e "s|api.star-history.com/svg?repos=${FORK_OWNER_LOWER}/${FORK_GITHUB_REPO}|api.star-history.com/svg?repos=${UPSTREAM_GITHUB_OWNER}/${UPSTREAM_GITHUB_REPO}|g"
    )
  fi

  case "$file" in
    frontend/src/components/common/VersionBadge.vue)
      cmd+=(
        -e "s|const GITHUB_REPO = '${UPSTREAM_GITHUB_OWNER}/${UPSTREAM_GITHUB_REPO}'|const GITHUB_REPO = '${FORK_GITHUB_OWNER}/${FORK_GITHUB_REPO}'|g"
        -e "s|const GITHUB_REPO = \"${UPSTREAM_GITHUB_OWNER}/${UPSTREAM_GITHUB_REPO}\"|const GITHUB_REPO = \"${FORK_GITHUB_OWNER}/${FORK_GITHUB_REPO}\"|g"
        -e "s|const DOCKER_IMAGE = '${UPSTREAM_DOCKERHUB_USER}/${UPSTREAM_GITHUB_REPO}'|const DOCKER_IMAGE = '${FORK_DOCKERHUB_USER}/${FORK_GITHUB_REPO}'|g"
        -e "s|const DOCKER_IMAGE = \"${UPSTREAM_DOCKERHUB_USER}/${UPSTREAM_GITHUB_REPO}\"|const DOCKER_IMAGE = \"${FORK_DOCKERHUB_USER}/${FORK_GITHUB_REPO}\"|g"
      )
      ;;
    deploy/install.sh)
      cmd+=(
        -e "s|GITHUB_REPO=\"${UPSTREAM_GITHUB_OWNER}/${UPSTREAM_GITHUB_REPO}\"|GITHUB_REPO=\"${FORK_GITHUB_OWNER}/${FORK_GITHUB_REPO}\"|g"
      )
      ;;
  esac

  "${cmd[@]}" "$file"
}

# Compare content ignoring pure CRLF/LF differences for "changed?" decisions.
content_key() {
  tr -d '\r' <"$1" | cksum
}

apply_file() {
  local file="$1"
  if [[ ! -s "$file" ]]; then
    return 0
  fi
  if ! grep -Iq . "$file" 2>/dev/null; then
    return 0
  fi

  local tmp
  tmp="$(mktemp)"
  transform "$file" >"$tmp"

  if [[ "$(content_key "$file")" == "$(content_key "$tmp")" ]]; then
    rm -f "$tmp"
    return 0
  fi

  if $CHECK_ONLY; then
    rm -f "$tmp"
    echo "drift: $file"
    return 2
  fi

  # Preserve original line endings if file was CRLF.
  if grep -q $'\r' "$file" 2>/dev/null; then
    # Convert tmp LF -> CRLF for Windows-checked-out files
    local tmp2
    tmp2="$(mktemp)"
    sed 's/$/\r/' "$tmp" >"$tmp2"
    mv "$tmp2" "$file"
    rm -f "$tmp"
  else
    mv "$tmp" "$file"
  fi
  echo "updated: $file"
  return 1
}

drift=0
changed=0
for f in "${TARGET_FILES[@]}"; do
  set +e
  apply_file "$f"
  rc=$?
  set -e
  if [[ $rc -eq 2 ]]; then
    drift=1
  elif [[ $rc -eq 1 ]]; then
    changed=$((changed + 1))
  fi
done

if $CHECK_ONLY; then
  if [[ "$drift" -ne 0 ]]; then
    echo "check failed: branding overlay would change the files above" >&2
    echo "run: ./.fork/apply-overlay.sh" >&2
    exit 1
  fi
  echo "check ok: fork branding is consistent with .fork/overlay.conf"
  exit 0
fi

echo ""
echo "fork overlay applied (files changed this run: ${changed})"
echo "  docker:  ${FORK_DOCKERHUB_USER}/${FORK_GITHUB_REPO}"
echo "  github:  ${FORK_GITHUB_OWNER}/${FORK_GITHUB_REPO}"

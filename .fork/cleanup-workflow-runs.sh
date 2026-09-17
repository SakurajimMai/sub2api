#!/usr/bin/env bash
# Keep only the newest KEEP_COUNT GitHub Actions runs per workflow.
# Older completed runs are deleted. In-progress/queued runs and the current
# GITHUB_RUN_ID are never deleted.
#
# Usage:
#   ./.fork/cleanup-workflow-runs.sh
#   ./.fork/cleanup-workflow-runs.sh --dry-run
#   ./.fork/cleanup-workflow-runs.sh --select-from-json KEEP [CURRENT_RUN_ID] < runs.json
#
# Env:
#   GITHUB_REPOSITORY  owner/repo (required unless --select-from-json)
#   KEEP_COUNT         runs to retain per workflow (default 10)
#   GH_TOKEN           token with actions: write (Actions provides this)

set -euo pipefail

KEEP_COUNT="${KEEP_COUNT:-10}"
DRY_RUN=false

select_ids_to_delete() {
  local keep="$1"
  local current="${2:-}"
  python3 -c '
import json
import sys

keep = int(sys.argv[1])
current = sys.argv[2]
raw = sys.stdin.read().strip()
runs = json.loads(raw or "[]")
if not isinstance(runs, list):
    raise SystemExit("expected a JSON array of workflow runs")

protected = set()
for run in runs[:keep]:
    protected.add(str(run["databaseId"]))
if current:
    protected.add(str(current))

skip_status = {
    "in_progress",
    "queued",
    "pending",
    "waiting",
    "requested",
    "waiting_for_review",
    "action_required",
}

for run in runs:
    rid = str(run["databaseId"])
    status = (run.get("status") or "").lower()
    if rid in protected:
        continue
    if status in skip_status:
        continue
    print(rid)
' "$keep" "$current"
}

if [[ "${1:-}" == "--select-from-json" ]]; then
  select_ids_to_delete "${2:?keep count required}" "${3:-}"
  exit 0
fi

if [[ "${1:-}" == "--dry-run" ]]; then
  DRY_RUN=true
  shift
fi

if [[ "${1:-}" == "--help" || "${1:-}" == "-h" ]]; then
  sed -n '2,16p' "$0"
  exit 0
fi

REPO="${GITHUB_REPOSITORY:?GITHUB_REPOSITORY is required}"
CURRENT_RUN="${GITHUB_RUN_ID:-}"

if ! [[ "$KEEP_COUNT" =~ ^[1-9][0-9]*$ ]]; then
  echo "error: KEEP_COUNT must be a positive integer (got ${KEEP_COUNT})" >&2
  exit 1
fi

echo "Cleaning workflow runs for ${REPO} (keep ${KEEP_COUNT} per workflow)"
if [[ -n "$CURRENT_RUN" ]]; then
  echo "Protecting current run ${CURRENT_RUN}"
fi

workflows="$(
  gh api --paginate "repos/${REPO}/actions/workflows" \
    --jq '.workflows[] | select(.path != null and .path != "") | .path' \
    | sort -u
)"

if [[ -z "$workflows" ]]; then
  echo "No workflows found."
  exit 0
fi

deleted=0
failed=0

delete_run() {
  local id="$1"
  local attempt=0
  local err
  if $DRY_RUN; then
    echo "dry-run: would delete ${id}"
    deleted=$((deleted + 1))
    return 0
  fi
  while :; do
    if err="$(gh api --method DELETE "repos/${REPO}/actions/runs/${id}" 2>&1)"; then
      echo "deleted ${id}"
      deleted=$((deleted + 1))
      return 0
    fi
    attempt=$((attempt + 1))
    if [[ "$err" == *"404"* ]]; then
      echo "already gone ${id}"
      return 0
    fi
    if [[ $attempt -ge 5 ]]; then
      echo "warn: failed to delete ${id}: ${err}" >&2
      failed=$((failed + 1))
      return 1
    fi
    sleep $((attempt * 2))
  done
}

while IFS= read -r path; do
  [[ -z "$path" ]] && continue
  workflow="${path##*/}"
  echo "--- ${workflow} ---"
  runs_json="$(
    gh run list --repo "$REPO" --workflow "$workflow" --limit 1000 \
      --json databaseId,status,createdAt
  )"
  ids="$(printf '%s' "$runs_json" | select_ids_to_delete "$KEEP_COUNT" "$CURRENT_RUN")"
  if [[ -z "$ids" ]]; then
    echo "nothing to delete"
    continue
  fi
  while IFS= read -r id; do
    [[ -z "$id" ]] && continue
    delete_run "$id" || true
  done <<< "$ids"
done <<< "$workflows"

echo ""
echo "cleanup finished: deleted=${deleted} failed=${failed} dry_run=${DRY_RUN}"
if [[ "$failed" -gt 0 ]]; then
  echo "warn: ${failed} run(s) could not be deleted" >&2
fi

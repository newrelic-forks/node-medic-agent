#!/usr/bin/env bash
# AC-15 runbook lint (research R-7).
#
# Runs the seven `grep -E` patterns documented in research.md against the
# canonical runbook at $1 (default prompts/runbook.md). Exits 0 only if all
# seven match; exits non-zero with a per-missing-clause error otherwise.
#
# Run by:
#   1. CI (.github/workflows/nodemedic-agent-ci.yml: runbook-lint job).
#   2. The Helm pre-install / pre-upgrade Job (T025) that mounts the
#      rendered image's /app/prompts/runbook.md and invokes this script.
#
# Both call sites use the same script — single source of truth.

set -euo pipefail

RUNBOOK="${1:-prompts/runbook.md}"

if [[ ! -r "$RUNBOOK" ]]; then
  echo "ERROR: runbook file not readable: $RUNBOOK" >&2
  exit 2
fi

# Order matters only for human-readable failure output. Each entry is:
#   "<clause description>|<grep pattern>"
# Patterns use extended regex (-E). Case-insensitive (-i) so prose drift
# doesn't break the build for cosmetic capitalization.
checks=(
  "1. AWS dispatch heading|^[[:space:]]*##[[:space:]]+AWS:"
  "2. Azure dispatch heading|^[[:space:]]*##[[:space:]]+Azure:"
  "3. Evidence calibration phrase|confidence below 0.7|lower the confidence"
  "4. Recommendation rubric (NoAction)|recommendation\\.action[[:space:]]*=[[:space:]]*\"?NoAction\"?"
  "5. Forbidden credential paths|\\\$SSH_KEY_PATH"
  "6. NRQL account_id=1 instruction|account_id[[:space:]]*=[[:space:]]*1"
  "7. emit_report calling discipline|call emit_report exactly once"
  "8. ContainerRuntimeUnhealthy decision tree|^[[:space:]]*##[[:space:]]+ContainerRuntimeUnhealthy"
  "9. KubeletUnhealthy decision tree|^[[:space:]]*##[[:space:]]+KubeletUnhealthy"
)

missing=0
for entry in "${checks[@]}"; do
  IFS="|" read -r label rest <<< "$entry"
  # `rest` may contain pipe-separated alternatives; treat as a single ERE.
  pattern="$rest"
  if ! grep -qiE "$pattern" "$RUNBOOK"; then
    echo "MISSING: $label  (pattern: $pattern)" >&2
    missing=$((missing+1))
  else
    echo "ok:      $label"
  fi
done

if (( missing > 0 )); then
  echo "" >&2
  echo "FAIL: runbook $RUNBOOK is missing $missing required clause(s) (AC-15)." >&2
  exit 1
fi

echo ""
echo "PASS: runbook $RUNBOOK contains all 9 AC-15 anchor clauses."

#!/usr/bin/env bash
# Run the whole spike end to end. Each step is re-runnable on its own.
#   ./run.sh              all steps
#   ./run.sh 01 03        only those steps
#   ANTHROPIC_API_KEY=... ./run.sh   also runs a real headless Claude Code turn in step 03
set -euo pipefail
cd "$(dirname "$0")"
: > results.md
steps=("$@"); [[ ${#steps[@]} -eq 0 ]] && steps=(00 01 02 03 04)
for s in "${steps[@]}"; do
  f=$(ls ${s}-*.sh 2>/dev/null | head -1) || true
  [[ -n "${f}" ]] || { echo "no step ${s}"; exit 1; }
  bash "${f}"
done
printf '\n\033[1m== Results ==\033[0m\n'; cat results.md

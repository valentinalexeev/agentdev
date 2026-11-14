#!/usr/bin/env bash
# Wait for the rolling agent rollout (#176) and report it. Exit 0 when every agent shows status.rollout.phase Current
# for the operator's current version; exit 1 at once when the rollout stopped (an agent Failed), with the agent and
# the reason; exit 2 on timeout. Read-only: kubectl get only.
#   scripts/rollout-wait.sh [--namespace coding-agents] [--timeout-minutes 120] [--context kind-agentdev]
set -euo pipefail
ns=coding-agents timeout=120 ctx=()
while (( $# )); do
  case $1 in
    --namespace) ns=$2; shift 2 ;;
    --timeout-minutes) timeout=$2; shift 2 ;;
    --context) ctx=(--context "$2"); shift 2 ;;
    *) echo "unknown argument $1" >&2; exit 64 ;;
  esac
done
[[ "${timeout}" =~ ^[0-9]{1,5}$ ]] || { echo "--timeout-minutes must be a number" >&2; exit 64; }
deadline=$(( $(date +%s) + timeout * 60 ))
# a new operator reconciles every agent at its start and writes the new version into each status; until then the
# agents still show the old version as Current. Give it that time before "all Current" counts.
settle_until=$(( $(date +%s) + ${ROLLOUT_SETTLE_SECONDS:-45} ))
last=""
while :; do
  rows=$(kubectl ${ctx[@]+"${ctx[@]}"} -n "${ns}" get agents.agentdev.dev -o json | jq -r '.items[] |
    [.metadata.name, (.status.rollout.version // "-"), (.status.rollout.phase // "-"), (.status.rollout.reason // "")] | @tsv')
  [[ -z "${rows}" ]] && { echo "no agents"; exit 0; }
  summary=$(awk -F'\t' '{n[$3]++} END {for (p in n) printf "%s=%d ", p, n[p]}' <<<"${rows}")
  [[ "${summary}" != "${last}" ]] && { echo "$(date -u +%H:%M:%S) rollout: ${summary}"; last=${summary}; }
  if failed=$(awk -F'\t' '$3=="Failed"' <<<"${rows}") && [[ -n "${failed}" ]]; then
    echo "rollout STOPPED:"; awk -F'\t' '$3=="Failed"{printf "  agent %s failed on version %s: %s\n", $1, $2, $4}' <<<"${rows}"
    awk -F'\t' '$3!="Current" && $3!="Failed"{printf "  agent %s stays on its previous version (%s)\n", $1, $3}' <<<"${rows}"
    exit 1
  fi
  versions=$(cut -f2 <<<"${rows}" | sort -u | wc -l)
  if (( versions == 1 && $(date +%s) >= settle_until )) && ! awk -F'\t' '$3!="Current"{bad=1} END{exit !bad}' <<<"${rows}"; then
    echo "rollout done: all agents on version $(head -1 <<<"${rows}" | cut -f2)"; exit 0
  fi
  (( $(date +%s) < deadline )) || { echo "rollout not finished after ${timeout} min:"; awk -F'\t' '$3!="Current"{printf "  %s: %s %s\n", $1, $3, $4}' <<<"${rows}"; exit 2; }
  sleep 15
done

#!/usr/bin/env bash
# Start a GKE instance of agentdev that scripts/cluster-stop.sh stopped (#313). It restores exactly what stop recorded.
# Usage:
#   ZONE=<zone> [PROJECT=<id>] [CLUSTER=agentdev] scripts/cluster-start.sh
# Steps:
#   1. Read kube-system/agentdev-stopped-state (or STATE_FILE when the ConfigMap is not there).
#   2. Resize the system pool to the recorded node count (minimum 1), then restore its autoscaling.
#   3. Restore the replicas: other workloads first, then the webhook router, the operator last.
#   4. Delete the ConfigMap. Wait for the rollouts (TIMEOUT, default 600s).
set -euo pipefail
: "${ZONE:?set ZONE, for example ZONE=europe-west4-a}"
CLUSTER="${CLUSTER:-agentdev}" STATE_FILE="${STATE_FILE:-./agentdev-stopped-state.env}" TIMEOUT="${TIMEOUT:-600s}"
STATE_NS=kube-system STATE_CM=agentdev-stopped-state
gc=(gcloud container --quiet); [[ -n "${PROJECT:-}" ]] && gc+=(--project "$PROJECT")

if ! state=$(kubectl -n "$STATE_NS" get configmap "$STATE_CM" -o jsonpath='{.data.state}' 2>/dev/null) || [[ -z "$state" ]]; then
  [[ -f "$STATE_FILE" ]] || { echo "ERROR: no $STATE_NS/$STATE_CM and no $STATE_FILE: nothing to start" >&2; exit 1; }
  state=$(cat "$STATE_FILE"); echo "using $STATE_FILE"
fi
read -r POOL as_enabled as_min as_max nodes < <(sed -n 's/^pool=//p' <<<"$state")

# 2. system pool
size=$(( nodes > 0 ? nodes : (as_min > 0 ? as_min : 1) ))
"${gc[@]}" clusters resize "$CLUSTER" --zone "$ZONE" --node-pool "$POOL" --num-nodes "$size"
if [[ "$as_enabled" == [Tt]rue ]]; then
  "${gc[@]}" clusters update "$CLUSTER" --zone "$ZONE" --node-pool "$POOL" \
    --enable-autoscaling --min-nodes "$as_min" --max-nodes "$as_max"
fi

# 3. replicas: rest, router, operator
rank() { case "$1" in agentdev-operator) echo 2;; webhook-router) echo 1;; *) echo 0;; esac; }
rows=$(grep -vE '^(#|pool=|$)' <<<"$state" | while read -r ns kind name rep; do
  echo "$(rank "$name") $ns $kind $name $rep"; done | sort -s -k1,1n)
while read -r _ ns kind name rep; do
  [[ -n "$name" ]] || continue
  echo "scale $ns $kind/$name to $rep"
  kubectl -n "$ns" scale "$kind/$name" --replicas="$rep"
done <<<"$rows"

# 4. clean up and wait
kubectl -n "$STATE_NS" delete configmap "$STATE_CM" --ignore-not-found
while read -r _ ns kind name rep; do
  [[ -n "$name" && "$rep" != 0 ]] || continue
  kubectl -n "$ns" rollout status "$kind/$name" --timeout="$TIMEOUT" || echo "WARNING: $ns $kind/$name not ready"
done <<<"$rows"
echo "started: pool $POOL and $(grep -c . <<<"$rows" || true) workloads restored"

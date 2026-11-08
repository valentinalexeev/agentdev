#!/usr/bin/env bash
# Stop a GKE instance of agentdev without deleting it (#313). Start it again with scripts/cluster-start.sh.
# Usage:
#   ZONE=<zone> [PROJECT=<id>] [CLUSTER=agentdev] scripts/cluster-stop.sh
# Steps:
#   1. Record the replicas of every Deployment and StatefulSet in NAMESPACES and the autoscaling of the system pool
#      in the ConfigMap kube-system/agentdev-stopped-state (and a local copy in STATE_FILE).
#   2. Scale to 0 in this order: the operator (it reconciles the agents), the webhook router (it wakes agents),
#      then all other workloads.
#   3. Disable autoscaling of the system pool and resize it to 0. Retry until the pool has 0 nodes.
#   4. Verify: no running pod in NAMESPACES, 0 nodes in the system pool.
# Billed while stopped: the GKE control plane, persistent disks (PVCs), static IPs, the state bucket, Secret Manager.
# PVCs, Secrets and ConfigMaps stay. The agent pool scales to 0 by itself when no agent runs.
# Env: NAMESPACES (default "agentdev-platform coding-agents agentdev-executors"; a missing namespace is skipped),
#      POOL (system), RETRIES (10), RETRY_SLEEP (30 s), STATE_FILE (./agentdev-stopped-state.env).
set -euo pipefail
: "${ZONE:?set ZONE, for example ZONE=europe-west4-a}"
CLUSTER="${CLUSTER:-agentdev}" POOL="${POOL:-system}"
NAMESPACES="${NAMESPACES:-agentdev-platform coding-agents agentdev-executors}"
RETRIES="${RETRIES:-10}" RETRY_SLEEP="${RETRY_SLEEP:-30}" STATE_FILE="${STATE_FILE:-./agentdev-stopped-state.env}"
STATE_NS=kube-system STATE_CM=agentdev-stopped-state
gc=(gcloud container --quiet); [[ -n "${PROJECT:-}" ]] && gc+=(--project "$PROJECT")
loc=(--cluster "$CLUSTER" --zone "$ZONE")

if kubectl -n "$STATE_NS" get configmap "$STATE_CM" >/dev/null 2>&1; then
  echo "ERROR: $STATE_NS/$STATE_CM exists: the instance is already stopped. Run scripts/cluster-start.sh first." >&2
  exit 1
fi

# 1. record
workloads=""
for ns in $NAMESPACES; do
  kubectl get namespace "$ns" >/dev/null 2>&1 || { echo "skip namespace $ns (not found)"; continue; }
  workloads+=$(kubectl -n "$ns" get deployments,statefulsets \
    -o jsonpath='{range .items[*]}{.metadata.namespace} {.kind} {.metadata.name} {.spec.replicas}{"\n"}{end}')
  workloads+=$'\n'
done
workloads=$(sed '/^$/d' <<<"$workloads")
pool=$("${gc[@]}" node-pools describe "$POOL" "${loc[@]}" \
  --format='value(autoscaling.enabled,autoscaling.minNodeCount,autoscaling.maxNodeCount)')
read -r as_enabled as_min as_max <<<"$pool"
nodes=$(kubectl get nodes -l "cloud.google.com/gke-nodepool=$POOL" -o name | grep -c . || true)
{ echo "# agentdev-stopped-state $(date -u +%Y-%m-%dT%H:%M:%SZ) cluster=$CLUSTER zone=$ZONE"
  echo "pool=${POOL} ${as_enabled:-False} ${as_min:-0} ${as_max:-0} ${nodes}"
  printf '%s\n' "$workloads"; } > "$STATE_FILE"
kubectl -n "$STATE_NS" create configmap "$STATE_CM" --from-file=state="$STATE_FILE"
echo "recorded $(grep -c . <<<"$workloads" || true) workloads and pool $POOL ($pool, $nodes nodes) in $STATE_NS/$STATE_CM and $STATE_FILE"

# 2. scale to 0: operator, router, then the rest
rank() { case "$3" in agentdev-operator) echo 0;; webhook-router) echo 1;; *) echo 2;; esac; }
while read -r ns kind name _; do
  [[ -n "$name" ]] || continue
  echo "$(rank "$ns" "$kind" "$name") $ns $kind $name"
done <<<"$workloads" | sort -s -k1,1n | while read -r _ ns kind name; do
  echo "scale $ns $kind/$name to 0"
  kubectl -n "$ns" scale "$kind/$name" --replicas=0
done

# 3. system pool to 0
"${gc[@]}" clusters update "$CLUSTER" --zone "$ZONE" --node-pool "$POOL" --no-enable-autoscaling
for i in $(seq 1 "$RETRIES"); do
  "${gc[@]}" clusters resize "$CLUSTER" --zone "$ZONE" --node-pool "$POOL" --num-nodes 0 || true
  n=$(kubectl get nodes -l "cloud.google.com/gke-nodepool=$POOL" -o name | grep -c . || true)
  [[ "$n" == 0 ]] && break
  echo "pool $POOL has $n nodes (try $i of $RETRIES); retry in ${RETRY_SLEEP}s"
  sleep "$RETRY_SLEEP"
done

# 4. verify
fail=0
[[ "${n:-1}" == 0 ]] || { echo "FAIL: pool $POOL still has $n nodes" >&2; fail=1; }
for ns in $NAMESPACES; do
  kubectl get namespace "$ns" >/dev/null 2>&1 || continue
  r=$(kubectl -n "$ns" get pods --field-selector=status.phase=Running -o name | grep -c . || true)
  [[ "$r" == 0 ]] || { echo "FAIL: $r running pods in $ns" >&2; fail=1; }
done
(( fail )) && exit 1
echo "stopped: pool $POOL has 0 nodes, no running pods. Start again with scripts/cluster-start.sh."

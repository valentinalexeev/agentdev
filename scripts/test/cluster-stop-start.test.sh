#!/usr/bin/env bash
# Tests for scripts/cluster-stop.sh and scripts/cluster-start.sh (#313) with a fake kubectl and gcloud on PATH.
# No cluster and no cloud: the fakes keep state in files and log every call.
#   scripts/test/cluster-stop-start.test.sh
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
fail() { echo "FAIL: $*"; echo "--- calls:"; cat "$work/calls"; exit 1; }
mkdir -p "$work/bin" "$work/s"; : > "$work/calls"
export FAKE="$work/s" PATH="$work/bin:$PATH"

cat > "$work/bin/kubectl" <<'EOF'
#!/usr/bin/env bash
# fake kubectl: workloads in $FAKE/w ("ns Kind name replicas"), nodes in $FAKE/nodes, ConfigMap in $FAKE/cm
echo "kubectl $*" >> "$FAKE/../calls"
ns=""; [[ "$1" == -n ]] && { ns=$2; shift 2; }
case "$1 $2" in
  "get namespace") grep -qx "$3" "$FAKE/namespaces" ;;
  "get configmap") [[ -f "$FAKE/cm" ]] || exit 1; [[ "$*" == *jsonpath* ]] && cat "$FAKE/cm"; exit 0 ;;
  "create configmap") all="$*"; cp "${all##*--from-file=state=}" "$FAKE/cm" ;;
  "delete configmap") rm -f "$FAKE/cm" ;;
  "get deployments,statefulsets") awk -v n="$ns" '$1==n' "$FAKE/w" ;;
  "get nodes") n=$(cat "$FAKE/nodes"); for ((i=0;i<n;i++)); do echo "node/n$i"; done ;;
  "get pods") awk -v n="$ns" '$1==n && $4>0 {print "pod/" $3}' "$FAKE/w" ;;
  "scale "*) kn=${2}; rep=${3#--replicas=}
    awk -v n="$ns" -v k="${kn%%/*}" -v m="${kn#*/}" -v r="$rep" '$1==n && $2==k && $3==m {$4=r} {print}' "$FAKE/w" > "$FAKE/w.new"
    mv "$FAKE/w.new" "$FAKE/w" ;;
  "rollout status") ;;
  *) echo "fake kubectl: unexpected $*" >&2; exit 3 ;;
esac
EOF
cat > "$work/bin/gcloud" <<'EOF'
#!/usr/bin/env bash
# fake gcloud: pool autoscaling in $FAKE/pool ("enabled min max"); resize to 0 fails $FAKE/resize_fail times
echo "gcloud $*" >> "$FAKE/../calls"
args=" $* "
case "$args" in
  *" node-pools describe "*) cat "$FAKE/pool" ;;
  *" --no-enable-autoscaling "*) read -r _ a b < "$FAKE/pool"; echo "False $a $b" > "$FAKE/pool" ;;
  *" --enable-autoscaling "*) mn=${args#* --min-nodes }; mx=${args#* --max-nodes }; echo "True ${mn%% *} ${mx%% *}" > "$FAKE/pool" ;;
  *" resize "*) n=${args#* --num-nodes }; n=${n%% *}
    f=$(cat "$FAKE/resize_fail" 2>/dev/null || echo 0)
    if (( n == 0 && f > 0 )); then echo $((f - 1)) > "$FAKE/resize_fail"; echo "ERROR: operation in progress" >&2; exit 1; fi
    echo "$n" > "$FAKE/nodes" ;;
  *) echo "fake gcloud: unexpected $*" >&2; exit 3 ;;
esac
EOF
chmod +x "$work/bin/"*

printf 'agentdev-platform\ncoding-agents\n' > "$FAKE/namespaces"   # agentdev-executors does not exist
cat > "$FAKE/w" <<'EOF'
agentdev-platform Deployment agentdev-api 2
agentdev-platform Deployment agentdev-operator 1
agentdev-platform StatefulSet postgres 1
coding-agents Deployment webhook-router 1
coding-agents StatefulSet agent-alice 0
coding-agents StatefulSet agent-bob 1
EOF
cp "$FAKE/w" "$work/w.before"
echo "True 1 2" > "$FAKE/pool"; echo 1 > "$FAKE/nodes"; echo 2 > "$FAKE/resize_fail"
export ZONE=z1 RETRY_SLEEP=0 RETRIES=5 STATE_FILE="$work/state.env"

# stop
"$root/scripts/cluster-stop.sh" > "$work/stop.out" 2>&1 || { cat "$work/stop.out"; fail "stop exit $?"; }
scales=$(grep -o 'scale [a-zA-Z]*/[a-z-]*' "$work/calls")
[[ $(head -1 <<<"$scales") == "scale Deployment/agentdev-operator" ]] || fail "operator not first: $scales"
[[ $(sed -n 2p <<<"$scales") == "scale Deployment/webhook-router" ]] || fail "router not second: $scales"
[[ $(grep -c . <<<"$scales") == 6 ]] || fail "want 6 scale calls: $scales"
echo "PASS stop order (operator, router, then the rest)"
auto=$(grep -n 'no-enable-autoscaling' "$work/calls" | cut -d: -f1); first=$(grep -n ' resize ' "$work/calls" | head -1 | cut -d: -f1)
(( auto < first )) || fail "autoscaling not disabled before resize"
[[ $(grep -c ' resize .*--num-nodes 0' "$work/calls") == 3 ]] || fail "want 3 resize tries (2 failures)"
[[ $(cat "$FAKE/nodes") == 0 ]] || fail "pool not 0"
echo "PASS system pool: autoscaling off, resize retried until 0 nodes"
grep -qx 'pool=system True 1 2 1' "$FAKE/cm" || fail "pool state not recorded: $(cat "$FAKE/cm")"
grep -qx 'coding-agents StatefulSet agent-bob 1' "$FAKE/cm" || fail "replicas not recorded"
[[ -z $(awk '$4!=0' "$FAKE/w") ]] || fail "workloads not at 0: $(cat "$FAKE/w")"
echo "PASS state recorded in the ConfigMap; all workloads at 0"

# a second stop is refused (the state would be lost)
rc=0; "$root/scripts/cluster-stop.sh" >/dev/null 2>&1 || rc=$?
[[ $rc == 1 ]] || fail "second stop exit $rc, want 1"
echo "PASS second stop refused"

# start
: > "$work/calls"
"$root/scripts/cluster-start.sh" > "$work/start.out" 2>&1 || { cat "$work/start.out"; fail "start exit $?"; }
diff <(sort "$work/w.before") <(sort "$FAKE/w") || fail "replicas not restored exactly"
[[ $(cat "$FAKE/pool") == "True 1 2" ]] || fail "autoscaling not restored: $(cat "$FAKE/pool")"
[[ $(cat "$FAKE/nodes") == 1 ]] || fail "pool not resized to 1"
scales=$(grep -o 'scale [a-zA-Z]*/[a-z-]*' "$work/calls")
[[ $(tail -1 <<<"$scales") == "scale Deployment/agentdev-operator" ]] || fail "operator not last: $scales"
[[ $(tail -2 <<<"$scales" | head -1) == "scale Deployment/webhook-router" ]] || fail "router not before operator: $scales"
p=$(grep -n ' resize ' "$work/calls" | cut -d: -f1); s=$(grep -n 'kubectl -n .* scale' "$work/calls" | head -1 | cut -d: -f1)
(( p < s )) || fail "pool must come back before the workloads"
[[ ! -f "$FAKE/cm" ]] || fail "ConfigMap not deleted"
echo "PASS start restores replicas, pool size and autoscaling exactly (operator last); ConfigMap deleted"

# start from the local file when the ConfigMap is gone; retries exhausted -> stop fails
rc=0; "$root/scripts/cluster-start.sh" >/dev/null 2>&1 || rc=$?
[[ $rc == 0 ]] || fail "start from STATE_FILE exit $rc"
echo 1 > "$FAKE/nodes"; echo 99 > "$FAKE/resize_fail"; rm -f "$STATE_FILE"
rc=0; RETRIES=2 "$root/scripts/cluster-stop.sh" > "$work/stop2.out" 2>&1 || rc=$?
[[ $rc == 1 ]] && grep -q 'FAIL: pool system still has 1 nodes' "$work/stop2.out" || fail "exhausted retries: exit $rc"
echo "PASS start from STATE_FILE; stop exits 1 when the pool does not reach 0"

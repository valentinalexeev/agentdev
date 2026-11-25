#!/usr/bin/env bash
# Tests for the GitOps scripts (#385): scripts/gitops-status.sh (fake kubectl, no cluster) and
# scripts/gitops-layout.py (a small render fixture). Run: scripts/test/gitops-test.sh
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
fail() { echo "FAIL: $*"; exit 1; }
pass() { echo "PASS: $*"; }

# ---- status adapter: a fake kubectl prints $work/state/<name>.json
mkdir -p "$work/state" "$work/bin"
cat > "$work/bin/kubectl" <<'K'
#!/usr/bin/env bash
# kubectl get <kind> <name> -n <ns> -o json
f="$FAKE_STATE/$3.json"; [ -f "$f" ] && cat "$f" || { echo "not found" >&2; exit 1; }
K
chmod +x "$work/bin/kubectl"
export KUBECTL="$work/bin/kubectl" FAKE_STATE="$work/state"
st="$root/scripts/gitops-status.sh"
obj() { # name generation observed ready-status reason revision [stalled]
  local stalled=""; [ "${7:-}" = stalled ] && stalled=',{"type":"Stalled","status":"True","reason":"BuildFailed","message":"bad"}'
  printf '{"metadata":{"generation":%s},"status":{"observedGeneration":%s,"lastAppliedRevision":"gitops@sha1:%s","conditions":[{"type":"Ready","status":"%s","reason":"%s","message":"m"}%s]}}' \
    "$2" "$3" "$6" "$4" "$5" "$stalled" > "$work/state/$1.json"
}
sha=0123456789abcdef0123456789abcdef01234567
obj agentdev-platform 2 2 True ReconciliationSucceeded "$sha"
obj agentdev-agents 1 1 True ReconciliationSucceeded "$sha"
out=$("$st" --timeout 0 --revision "$sha" kustomization/agentdev-platform kustomization/agentdev-agents)
[ "$(printf '%s\n' "$out" | grep -c '^ready ')" = 2 ] || fail "two ready units: $out"; pass "ready units"

obj agentdev-agents 1 1 True ReconciliationSucceeded fedcba9876543210fedcba9876543210fedcba98
"$st" --timeout 0 --revision "$sha" kustomization/agentdev-agents 2>/dev/null && fail "old revision is not ready"; pass "old revision waits"
obj agentdev-agents 3 2 True ReconciliationSucceeded "$sha"
"$st" --timeout 0 kustomization/agentdev-agents 2>/dev/null && fail "unobserved generation is not ready"; pass "generation"
obj agentdev-agents 1 1 False Progressing "$sha"
"$st" --timeout 0 kustomization/agentdev-agents 2>/dev/null && fail "Ready=False is not ready"; pass "Ready=False waits"
obj agentdev-agents 1 1 False BuildFailed "$sha" stalled
err=$("$st" --timeout 60 --interval 1 kustomization/agentdev-agents 2>&1) && fail "stalled must fail"
[[ "$err" == failed* ]] || fail "stalled fails at once: $err"; pass "stalled fails at once"
"$st" --timeout 0 kustomization/missing 2>/dev/null && fail "missing unit"; pass "missing unit waits"
obj reloader 1 1 True InstallSucceeded "$sha"
"$st" --timeout 0 helmrelease/reloader/reloader >/dev/null || fail "helmrelease unit"; pass "helmrelease unit"
rc=0; "$st" --engine argocd kustomization/x 2>/dev/null || rc=$?; [ "$rc" = 2 ] || fail "unknown engine exit 2"; pass "unknown engine"
rc=0; "$st" --timeout '1+1' kustomization/x 2>/dev/null || rc=$?; [ "$rc" = 2 ] || fail "timeout must be digits"; pass "numbers checked"
rc=0; "$st" --revision 'x;y' kustomization/x 2>/dev/null || rc=$?; [ "$rc" = 2 ] || fail "revision must be a sha"; pass "revision checked"

# ---- layout: split, merge of a duplicate, no StatefulSet replicas, Secret refused
lay() { "${PYTHON:-python3}" "$root/scripts/gitops-layout.py" "$@"; }
r="$work/render"; mkdir -p "$r"
cat > "$r/00-namespaces.yaml" <<'Y'
apiVersion: v1
kind: Namespace
metadata: {name: ex}
Y
cat > "$r/01-platform.yaml" <<'Y'
apiVersion: apps/v1
kind: StatefulSet
metadata: {name: db, namespace: ex}
spec: {replicas: 1, serviceName: db, selector: {matchLabels: {a: b}}, template: {metadata: {labels: {a: b}}, spec: {containers: [{name: c, image: i}]}}}
Y
cat > "$r/02-ns-again.yaml" <<'Y'
apiVersion: v1
kind: Namespace
metadata: {name: ex, labels: {agentdev/role: executors}}
Y
cat > "$r/03-agents.yaml" <<'Y'
apiVersion: agentdev.dev/v1alpha1
kind: Agent
metadata: {name: alice, namespace: ex}
spec: {forge: github}
Y
lay "$r" "$work/out" >/dev/null
[ -f "$work/out/agents/03-agents.yaml" ] && [ ! -f "$work/out/platform/03-agents.yaml" ] || fail "agents layer"
grep -q replicas "$work/out/platform/01-platform.yaml" && fail "StatefulSet replicas in Git"
[ ! -f "$work/out/platform/00-namespaces.yaml" ] && grep -q 'agentdev/role: executors' "$work/out/platform/02-ns-again.yaml" \
  || fail "duplicate Namespace merged into its last place"
[ "$(yq '.patches[0].patch | from_json | .[0].value | join(",")' "$work/out/reloader/kustomization.yaml")" = ex ] \
  || fail "reloader layer watches the rendered namespaces"
grep -q 'agentdev-executors' "$work/out/reloader/kustomization.yaml" && fail "no executors namespace without executors"
pass "layout"
if command -v kustomize >/dev/null; then
  [ "$(kustomize build "$work/out/platform" | grep -c '^kind:')" = 2 ] || fail "kustomize build platform"
  kustomize build "$work/out/agents" | grep -q '^kind: Agent' || fail "kustomize build agents"
  pass "kustomize build of the layers"
fi
lay "$r" "$work/out" >/dev/null 2>&1 && fail "non-empty out_dir"; pass "non-empty out_dir refused"
printf 'apiVersion: v1\nkind: Secret\nmetadata: {name: s}\n' > "$r/04-s.yaml"
lay "$r" "$work/out2" >/dev/null 2>&1 && fail "Secret refused"; [ ! -e "$work/out2" ] || fail "nothing written"
pass "Secret refused"
echo "all gitops tests passed"

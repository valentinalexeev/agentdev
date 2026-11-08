#!/usr/bin/env bash
# kind e2e for the scoped secrets store (#202 part 1). Needs a kind cluster with ./run.sh 13 done (and 12).
#   CLUSTER_NAME=t14 scripts/e2e-secrets.sh
# Checks: ProjectSecret -> ExternalSecret -> ps-<n> sync, rotation, CRD CEL, admission policies, deletion.
# The test value is random and never printed.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1
source ./config.env
k() { ${KUBECTL} "$@"; }
pass=0 fail=0
ok() { echo "  PASS $*"; pass=$((pass + 1)); }
no() { echo "  FAIL $*"; fail=$((fail + 1)); }
ns="${AGENTS_NS}" vns="${SECRETS_VALUES_NS}"
name=e2e-reg val1=$(openssl rand -hex 16) val2=$(openssl rand -hex 16)
cleanup() { k -n "$ns" delete projectsecret "$name" --ignore-not-found >/dev/null; k -n "$vns" delete secret "agentdev-ps-$name" --ignore-not-found >/dev/null; }
trap cleanup EXIT
cleanup

[[ "$(k -n "$ns" get secretstore agentdev-secrets -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" == True ]] \
  && ok "SecretStore ready" || no "SecretStore not ready"

k -n "$ns" apply -f - >/dev/null <<EOF
apiVersion: agentdev.dev/v1alpha1
kind: ProjectSecret
metadata: {name: $name}
spec: {scope: project, project: owner/repo, mode: broker, broker: {upstream: "https://registry.example.com"}}
EOF
for _ in $(seq 20); do k -n "$ns" get externalsecret "ps-$name" >/dev/null 2>&1 && break; sleep 1; done
k -n "$ns" get externalsecret "ps-$name" >/dev/null 2>&1 && ok "operator made ExternalSecret ps-$name" || no "no ExternalSecret"
sleep 3
[[ "$(k -n "$ns" get projectsecret "$name" -o jsonpath='{.status.phase}')" == pending ]] && ok "phase pending without a value" || no "phase not pending"

printf '%s' "$val1" | k -n "$vns" create secret generic "agentdev-ps-$name" --from-file=value=/dev/stdin >/dev/null
k -n "$ns" annotate externalsecret "ps-$name" force-sync="$(date +%s)" --overwrite >/dev/null
for _ in $(seq 60); do [[ "$(k -n "$ns" get projectsecret "$name" -o jsonpath='{.status.phase}')" == ready ]] && break; sleep 2; done
[[ "$(k -n "$ns" get projectsecret "$name" -o jsonpath='{.status.phase}')" == ready ]] && ok "phase ready after the value was set" || no "phase not ready"
[[ "$(k -n "$ns" get secret "ps-$name" -o jsonpath='{.data.value}' | base64 -d)" == "$val1" ]] && ok "ps-$name holds the value" || no "ps-$name value wrong"

printf '%s' "$val2" | k -n "$vns" create secret generic "agentdev-ps-$name" --from-file=value=/dev/stdin --dry-run=client -o yaml | k apply -f - >/dev/null
k -n "$ns" annotate externalsecret "ps-$name" force-sync="$(date +%s)-2" --overwrite >/dev/null
for _ in $(seq 60); do [[ "$(k -n "$ns" get secret "ps-$name" -o jsonpath='{.data.value}' | base64 -d)" == "$val2" ]] && break; sleep 2; done
[[ "$(k -n "$ns" get secret "ps-$name" -o jsonpath='{.data.value}' | base64 -d)" == "$val2" ]] && ok "rotation reached ps-$name" || no "rotation not synced"

# CRD CEL
out=$(k -n "$ns" apply -f - 2>&1 <<EOF
apiVersion: agentdev.dev/v1alpha1
kind: ProjectSecret
metadata: {name: e2e-env}
spec: {scope: agent, agent: a, mode: env, env: {name: ANTHROPIC_API_KEY, riskAccepted: true}}
EOF
) && no "reserved env name accepted" || { [[ "$out" == *"reserved variable name"* ]] && ok "CEL refuses a reserved env name" || no "unexpected: $out"; }
out=$(k -n "$ns" patch projectsecret "$name" --type=merge -p '{"spec":{"project":"other/repo"}}' 2>&1) \
  && no "project changed" || { [[ "$out" == *immutable* ]] && ok "CEL keeps the project immutable" || no "unexpected: $out"; }

# admission policies (as cluster admin: the rules hold for everyone except ESO)
out=$(k -n "$ns" apply -f - 2>&1 <<EOF
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata: {name: ps-steal}
spec:
  secretStoreRef: {name: agentdev-secrets, kind: SecretStore}
  target: {name: ps-steal, creationPolicy: Owner}
  data: [{secretKey: value, remoteRef: {key: agentdev-ps-$name}}]
EOF
) && { no "ExternalSecret reading another key accepted"; k -n "$ns" delete externalsecret ps-steal >/dev/null; } || ok "policy refuses an ExternalSecret for another key"
out=$(k -n "$ns" apply -f - 2>&1 <<EOF
apiVersion: external-secrets.io/v1
kind: ExternalSecret
metadata: {name: forge-copy}
spec:
  secretStoreRef: {name: agentdev-secrets, kind: SecretStore}
  target: {name: forge-copy, creationPolicy: Owner}
  data: [{secretKey: value, remoteRef: {key: anything}}]
EOF
) && { no "ExternalSecret with a free name accepted"; k -n "$ns" delete externalsecret forge-copy >/dev/null; } || ok "policy refuses an ExternalSecret not named ps-<n>"
out=$(k -n "$ns" create secret generic ps-fake --from-literal=value=x 2>&1) \
  && { no "ps-* Secret written by a person"; k -n "$ns" delete secret ps-fake >/dev/null; } || ok "policy reserves ps-* Secrets for ESO"
out=$(k -n "$ns" --as=system:serviceaccount:${PLATFORM_NS}:agentdev-operator apply -f - 2>&1 <<EOF
apiVersion: external-secrets.io/v1
kind: SecretStore
metadata: {name: evil}
spec: {provider: {kubernetes: {remoteNamespace: kube-system, server: {url: "https://kubernetes.default.svc"}, auth: {serviceAccount: {name: x}}}}}
EOF
) && { no "operator wrote a SecretStore"; k -n "$ns" delete secretstore evil >/dev/null; } || ok "agentdev service accounts cannot write a SecretStore"

# credential broker (#202 part 2): needs ./run.sh 13 with the credential proxy on
if k -n "$ns" get deploy credential-broker >/dev/null 2>&1; then
  k -n "$ns" apply -f - >/dev/null <<EOF
apiVersion: agentdev.dev/v1alpha1
kind: Agent
metadata: {name: e2e-brk}
spec: {repo: owner/repo, claudeSecret: claude-shared, paused: true}
---
apiVersion: agentdev.dev/v1alpha1
kind: ProjectSecret
metadata: {name: e2e-web}
spec: {scope: agent, agent: e2e-brk, mode: broker, broker: {upstream: "https://example.com"}}
EOF
  printf '%s' "$val1" | k -n "$vns" create secret generic agentdev-ps-e2e-web --from-file=value=/dev/stdin >/dev/null 2>&1
  signing=$(k -n "$ns" get secret claude-proxy-signing -o jsonpath='{.data.key}' | base64 -d)
  key_of() { printf '%s.%s' "$1" "$(printf 'agentdev-claude-proxy/v1:%s' "$1" | openssl dgst -sha256 -hmac "$signing" -r | cut -d' ' -f1)"; }
  good=$(key_of e2e-brk) other=$(key_of e2e-other)
  for _ in $(seq 60); do
    k -n "$ns" get configmap credential-broker-routes -o jsonpath='{.data.routes\.json}' 2>/dev/null | grep -q '"e2e-web".*"e2e-brk"' \
      && [[ "$(k -n "$ns" get projectsecret e2e-web -o jsonpath='{.status.phase}')" == ready ]] && break
    sleep 2
  done
  k -n "$ns" rollout status deploy/credential-broker --timeout=120s >/dev/null
  sleep 5   # the projected volume follows the new Secret
  burl="http://credential-broker.$ns.svc.cluster.local:8080/s/e2e-web/"
  codes=$(k -n "$ns" run e2e-brk-curl --rm -i --restart=Never --quiet --image=curlimages/curl:8.11.1 \
    --labels=app=coding-agent,agent=e2e-brk --env="G=$good" --env="O=$other" --command -- sh -c \
    "for a in \"Bearer \$G\" \"Bearer \$O\" 'Bearer e2e-brk.00'; do curl -s -o /dev/null -w '%{http_code} ' -H \"Authorization: \$a\" $burl; done" 2>/dev/null)
  read -r c_good c_other c_bad <<<"$codes"
  [[ "$c_good" == 200 ]] && ok "broker forwards with the agent's key (via https://example.com)" || no "broker forward: $c_good"
  [[ "$c_other" == 403 ]] && ok "broker refuses an agent outside the secret's scope" || no "out-of-scope agent: $c_other"
  [[ "$c_bad" == 401 ]] && ok "broker refuses a wrong key" || no "wrong key: $c_bad"
  for _ in $(seq 30); do
    k -n "$ns" get securityevents -o jsonpath='{range .items[*]}{.spec.source}/{.spec.rule} {end}' | grep -q 'credential-broker/broker-secret-denied' && break
    sleep 2
  done
  evs=$(k -n "$ns" get securityevents -o jsonpath='{range .items[*]}{.spec.source}/{.spec.rule} {end}')
  [[ "$evs" == *credential-broker/broker-use* && "$evs" == *credential-broker/broker-secret-denied* ]] \
    && ok "broker use and refusal recorded as SecurityEvents" || no "broker events missing: $evs"
  k -n "$ns" logs deploy/credential-broker 2>/dev/null | grep -qF "$val1" && no "value in the broker log" || ok "value not in the broker log"
  k -n "$ns" delete projectsecret e2e-web --ignore-not-found >/dev/null; k -n "$ns" delete agent e2e-brk --ignore-not-found >/dev/null
  k -n "$vns" delete secret agentdev-ps-e2e-web --ignore-not-found >/dev/null
  unset signing good other
else
  echo "  SKIP credential broker not deployed"
fi

# deletion
k -n "$ns" delete projectsecret "$name" >/dev/null
for _ in $(seq 30); do k -n "$ns" get secret "ps-$name" >/dev/null 2>&1 || break; sleep 2; done
k -n "$ns" get secret "ps-$name" >/dev/null 2>&1 && no "ps-$name kept after delete" || ok "delete removes ExternalSecret and ps-$name"

echo "secrets e2e: ${pass} PASS, ${fail} FAIL"
[[ $fail -eq 0 ]]

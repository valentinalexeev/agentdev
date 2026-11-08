#!/usr/bin/env bash
# kind e2e for project-scoped private npm registries (#248). Needs a kind cluster with ./run.sh 13 done (store and
# credential broker). Uses registry.npmjs.org as the "private" upstream (it accepts any token), so the check is the
# path, the scope and the absence of the credential, not the upstream's own auth.
#   CLUSTER_NAME=w17 scripts/e2e-private-registry.sh
# Checks: an agent of project A installs @<A scope> through the broker; it cannot use project B's registry (403);
# the npm config and the agent's env hold no registry credential.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1
source ./config.env
k() { ${KUBECTL} "$@"; }
pass=0 fail=0
ok() { echo "  PASS $*"; pass=$((pass + 1)); }
no() { echo "  FAIL $*"; fail=$((fail + 1)); }
ns="${AGENTS_NS}" vns="${SECRETS_VALUES_NS}"
tok_a="npm_$(openssl rand -hex 16)" tok_b="npm_$(openssl rand -hex 16)"
cleanup() {
  k -n "$ns" delete pod e2e-npm-a --ignore-not-found --wait=false >/dev/null
  k -n "$ns" delete projectsecret e2e-npm-a e2e-npm-b --ignore-not-found >/dev/null
  k -n "$ns" delete agent e2e-pra e2e-prb --ignore-not-found >/dev/null
  k -n "$vns" delete secret agentdev-ps-e2e-npm-a agentdev-ps-e2e-npm-b --ignore-not-found >/dev/null
}
trap cleanup EXIT
cleanup
k -n "$ns" get deploy credential-broker >/dev/null 2>&1 || { echo "  FAIL credential broker not deployed (./run.sh 13)"; exit 1; }

k -n "$ns" apply -f - >/dev/null <<EOF
apiVersion: agentdev.dev/v1alpha1
kind: Agent
metadata: {name: e2e-pra}
spec: {repo: project/a, claudeSecret: claude-shared, paused: true}
---
apiVersion: agentdev.dev/v1alpha1
kind: Agent
metadata: {name: e2e-prb}
spec: {repo: project/b, claudeSecret: claude-shared, paused: true}
---
apiVersion: agentdev.dev/v1alpha1
kind: ProjectSecret
metadata: {name: e2e-npm-a}
spec: {scope: project, project: project/a, mode: broker, broker: {upstream: "https://registry.npmjs.org", npmScope: "@types"}}
---
apiVersion: agentdev.dev/v1alpha1
kind: ProjectSecret
metadata: {name: e2e-npm-b}
spec: {scope: project, project: project/b, mode: broker, broker: {upstream: "https://registry.npmjs.org", npmScope: "@babel"}}
EOF
printf '%s' "$tok_a" | k -n "$vns" create secret generic agentdev-ps-e2e-npm-a --from-file=value=/dev/stdin >/dev/null
printf '%s' "$tok_b" | k -n "$vns" create secret generic agentdev-ps-e2e-npm-b --from-file=value=/dev/stdin >/dev/null

out=$(k -n "$ns" apply -f - 2>&1 <<EOF
apiVersion: agentdev.dev/v1alpha1
kind: ProjectSecret
metadata: {name: e2e-npm-bad}
spec: {scope: project, project: project/a, mode: broker, broker: {upstream: "https://registry.npmjs.org", npmScope: "acme"}}
EOF
) && { no "npmScope without @ accepted"; k -n "$ns" delete projectsecret e2e-npm-bad >/dev/null; } || ok "CRD refuses an npmScope without @"

for _ in $(seq 60); do
  npmrc_a=$(k -n "$ns" get configmap agentdev-guides -o jsonpath='{.data.e2e-pra\.npmrc}' 2>/dev/null)
  routes=$(k -n "$ns" get configmap credential-broker-routes -o jsonpath='{.data.routes\.json}' 2>/dev/null)
  [[ "$npmrc_a" == *"@types:registry="* && "$routes" == *'"e2e-npm-b"'* \
     && "$(k -n "$ns" get projectsecret e2e-npm-a -o jsonpath='{.status.phase}')" == ready ]] && break
  sleep 2
done
npmrc_b=$(k -n "$ns" get configmap agentdev-guides -o jsonpath='{.data.e2e-prb\.npmrc}')
[[ "$npmrc_a" == *"@types:registry=http://credential-broker.$ns.svc.cluster.local:8080/s/e2e-npm-a/"* && "$npmrc_a" != *@babel* ]] \
  && ok "agent of project A gets only A's scope in its npm config" || no "npm config of A: $npmrc_a"
[[ "$npmrc_b" == *"@babel:registry="* && "$npmrc_b" != *@types* ]] && ok "agent of project B gets only B's scope" || no "npm config of B: $npmrc_b"
[[ "$npmrc_a$npmrc_b" != *"$tok_a"* && "$npmrc_a$npmrc_b" != *"$tok_b"* && "$npmrc_a" == *'${AGENTDEV_BROKER_KEY}'* ]] \
  && ok "npm config holds no registry credential, only a reference to the agent key" || no "credential in the npm config"
sts_env=$(k -n "$ns" get sts agent-e2e-pra -o jsonpath='{range .spec.template.spec.containers[?(@.name=="agent")].env[*]}{.name}={.value}{" "}{end}')
items=$(k -n "$ns" get sts agent-e2e-pra -o jsonpath='{.spec.template.spec.volumes[?(@.name=="guide")].configMap.items[*].key}')
[[ "$sts_env" == *"NPM_CONFIG_GLOBALCONFIG=/etc/agentdev/guide/npmrc"* && "$items" == "e2e-pra e2e-pra.npmrc" ]] \
  && ok "agent StatefulSet mounts its own npm config only" || no "agent StatefulSet: env [$sts_env] items [$items]"

k -n "$ns" rollout status deploy/credential-broker --timeout=180s >/dev/null
sleep 5   # the projected values follow the new Secrets
# a pod shaped like agent A's container: same labels (NetworkPolicy), same npm config mount and env
signing=$(k -n "$ns" get secret claude-proxy-signing -o jsonpath='{.data.key}' | base64 -d)
key_a="e2e-pra.$(printf 'agentdev-claude-proxy/v1:%s' e2e-pra | openssl dgst -sha256 -hmac "$signing" -r | cut -d' ' -f1)"
unset signing
k -n "$ns" apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: e2e-npm-a, labels: {app: coding-agent, agent: e2e-pra}}
spec:
  restartPolicy: Never
  automountServiceAccountToken: false
  securityContext: {runAsNonRoot: true, runAsUser: 1000, seccompProfile: {type: RuntimeDefault}}
  containers:
  - name: agent
    image: node:22-alpine
    command: [sleep, "600"]
    env:
    - {name: AGENTDEV_BROKER_KEY, value: "$key_a"}
    - {name: NPM_CONFIG_GLOBALCONFIG, value: /etc/agentdev/guide/npmrc}
    - {name: HOME, value: /tmp}
    - {name: NO_PROXY, value: ".svc,.svc.cluster.local"}
    securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
    volumeMounts: [{name: guide, mountPath: /etc/agentdev/guide, readOnly: true}]
  volumes:
  - name: guide
    configMap: {name: agentdev-guides, items: [{key: e2e-pra, path: guide.md}, {key: e2e-pra.npmrc, path: npmrc}]}
EOF
k -n "$ns" wait --for=condition=Ready pod/e2e-npm-a --timeout=180s >/dev/null || no "test pod not ready"
ex() { k -n "$ns" exec e2e-npm-a -- sh -c "$1" 2>&1; }
inst=$(ex 'cd /tmp && npm init -y >/dev/null && npm install --no-audit --no-fund --ignore-scripts @types/node-fetch@2.6.11 --omit=dev >/dev/null 2>&1 && ls node_modules/@types && grep -o "credential-broker[^\"]*e2e-npm-a" package-lock.json | head -1')
[[ "$inst" == *node-fetch* && "$inst" == *"credential-broker"*"/s/e2e-npm-a"* ]] \
  && ok "agent of project A installs @types/* through its project's registry (broker)" || no "install of A's scope: $inst"
code=$(ex "wget -q -S -O /dev/null --header='Authorization: Bearer $key_a' http://credential-broker.$ns.svc.cluster.local:8080/s/e2e-npm-b/@babel%2fcore 2>&1 | grep -o 'HTTP/1.1 [0-9]*' | tail -1")
[[ "$code" == *403* ]] && ok "agent of project A cannot read project B's registry (403)" || no "A on B's registry: $code"
leak=$(ex "env; cat /etc/agentdev/guide/npmrc; cat /tmp/.npmrc 2>/dev/null; cat /proc/1/environ 2>/dev/null" | grep -cF -e "$tok_a" -e "$tok_b")
[[ "$leak" == 0 ]] && ok "no registry credential in the agent's env, files or /proc" || no "registry credential visible in the pod"
k -n "$ns" logs deploy/credential-broker 2>/dev/null | grep -qF -e "$tok_a" -e "$tok_b" && no "token in the broker log" || ok "token not in the broker log"
for _ in $(seq 30); do
  evs=$(k -n "$ns" get securityevents -o jsonpath='{range .items[*]}{.spec.agent}/{.spec.rule} {end}')
  [[ "$evs" == *e2e-pra/broker-secret-denied* ]] && break; sleep 2
done
[[ "$evs" == *e2e-pra/broker-use* && "$evs" == *e2e-pra/broker-secret-denied* ]] \
  && ok "use and cross-project refusal recorded as SecurityEvents" || no "events: $evs"

k -n "$ns" delete projectsecret e2e-npm-b >/dev/null
for _ in $(seq 30); do [[ -z "$(k -n "$ns" get configmap agentdev-guides -o jsonpath='{.data.e2e-prb\.npmrc}')" ]] && break; sleep 2; done
[[ -z "$(k -n "$ns" get configmap agentdev-guides -o jsonpath='{.data.e2e-prb\.npmrc}')" ]] \
  && ok "deleting the secret removes the scope from the npm config" || no "npm config of B kept"

echo "private registry e2e: ${pass} PASS, ${fail} FAIL"
[[ $fail -eq 0 ]]

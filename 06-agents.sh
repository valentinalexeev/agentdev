#!/usr/bin/env bash
# MVP agents: build the worker image, the router, and one Agent object per agent account (the operator, #47, turns
# it into a StatefulSet); Claude credentials as Secrets. Botboard (#26) and cluster-wide agent settings are the
# operator's (12-operator.sh).
#
# Claude credentials, in order of precedence per agent:
#   .secrets/<agent>.claude  file with lines like CLAUDE_CODE_OAUTH_TOKEN=... or ANTHROPIC_API_KEY=...
#   ANTHROPIC_API_KEY / CLAUDE_CODE_OAUTH_TOKEN from the environment (.env)  -> shared by all agents
source "$(dirname "$0")/lib.sh"
need envsubst; need jq

log "Building and loading ${WORKER_IMAGE}, ${FORGE_IMAGE}, ${ROUTER_IMAGE} and ${CLAUDE_PROXY_IMAGE}"
"${CONTAINER_ENGINE}" build -q -t "${WORKER_IMAGE}" "${SPIKE_DIR}/images/worker" >/dev/null
"${CONTAINER_ENGINE}" build -q -t "${FORGE_IMAGE}" "${SPIKE_DIR}/images/forge" >/dev/null
"${CONTAINER_ENGINE}" build -q -t "${ROUTER_IMAGE}" "${SPIKE_DIR}/images/router" >/dev/null
"${CONTAINER_ENGINE}" build -q -t "${CLAUDE_PROXY_IMAGE}" "${SPIKE_DIR}/images/credproxy" >/dev/null
kind load docker-image "${WORKER_IMAGE}" "${FORGE_IMAGE}" "${ROUTER_IMAGE}" "${CLAUDE_PROXY_IMAGE}" --name "${CLUSTER_NAME}" >/dev/null
ok "image loaded"

k create namespace "${AGENTS_NS}" --dry-run=client -o yaml | k apply -f - >/dev/null

shared=()
[[ -n "${ANTHROPIC_API_KEY:-}" ]]       && shared+=(--from-literal=ANTHROPIC_API_KEY="${ANTHROPIC_API_KEY}")
[[ -n "${CLAUDE_CODE_OAUTH_TOKEN:-}" ]] && shared+=(--from-literal=CLAUDE_CODE_OAUTH_TOKEN="${CLAUDE_CODE_OAUTH_TOKEN}")
if (( ${#shared[@]} )); then
  k -n "${AGENTS_NS}" create secret generic claude-shared "${shared[@]}" --dry-run=client -o yaml | k apply -f - >/dev/null
  ok "shared Claude credential stored (claude-shared)"
fi

# Cluster-wide webhook secrets, kept across runs (#3): one forge secret (Gitea -> router) and one
# registration token (agents -> router). Per-agent wake tokens (router -> agent) are below.
secret_file() { [[ -s "$1" ]] || { openssl rand -hex 32 > "$1"; chmod 600 "$1"; }; cat "$1"; }
GITEA_WEBHOOK_SECRET=$(secret_file "${SPIKE_DIR}/.secrets/router-gitea.secret")
FORGE_SECRETS="gitea=${GITEA_WEBHOOK_SECRET}"
gh_app="${SPIKE_DIR}/.secrets/github-app"
if [[ "${FORGE_KIND}" == github ]]; then   # #2: the App from scripts/github-app.sh convert
  [[ -s "${gh_app}/app-id" && -s "${gh_app}/private-key.pem" && -s "${gh_app}/webhook-secret" ]] \
    || die "FORGE_KIND=github: register the App first (scripts/github-app.sh, docs/admin.md)"
  [[ "$(cat "${gh_app}/app-id")" =~ ^[1-9][0-9]{0,15}$ ]] || die "${gh_app}/app-id is not a number"
  [[ -z "${GITHUB_INSTALLATION_ID}" || "${GITHUB_INSTALLATION_ID}" =~ ^[1-9][0-9]{0,15}$ ]] || die "GITHUB_INSTALLATION_ID is not a number"
  FORGE_SECRETS="github=$(cat "${gh_app}/webhook-secret")"
elif [[ "${FORGE_KIND}" != gitea ]]; then die "FORGE_KIND must be gitea or github"
fi
REGISTER_TOKEN=$(secret_file "${SPIKE_DIR}/.secrets/router-register.token")

log "Webhook router"
AGENTS_NS="${AGENTS_NS}" FORGE_SECRETS="${FORGE_SECRETS}" REGISTER_TOKEN="${REGISTER_TOKEN}" \
  ROUTER_IMAGE="${ROUTER_IMAGE}" IMAGE_PULL_POLICY=Never \
  envsubst '${AGENTS_NS} ${FORGE_SECRETS} ${REGISTER_TOKEN} ${ROUTER_IMAGE} ${IMAGE_PULL_POLICY}' \
  < "${SPIKE_DIR}/manifests/router.yaml.tmpl" | k apply -f - >/dev/null
k -n "${AGENTS_NS}" rollout restart deploy/webhook-router >/dev/null   # pick up a rebuilt local image
k -n "${AGENTS_NS}" rollout status deploy/webhook-router --timeout=180s >/dev/null
ok "router up: http://webhook-router.${AGENTS_NS}.svc.cluster.local:8080"

log "Claude credential proxy (#66)"
# the only pod with the Claude credentials; each agent gets a key (HMAC of its name) that is worth nothing elsewhere
PROXY_SIGNING=$(secret_file "${SPIKE_DIR}/.secrets/claude-proxy-signing.key")
printf '%s' "${PROXY_SIGNING}" | k -n "${AGENTS_NS}" create secret generic claude-proxy-signing --from-file=key=/dev/stdin \
  --dry-run=client -o yaml | k apply -f - >/dev/null
proxy_key() { printf 'agentdev-claude-proxy/v1:%s' "$1" | openssl dgst -sha256 -hmac "${PROXY_SIGNING}" -r | cut -d' ' -f1; }
[[ "${CLAUDE_BUDGET_TOKENS}" =~ ^[0-9]+$ ]] || die "CLAUDE_BUDGET_TOKENS must be a number of tokens (0 = no limit)"
# shellcheck disable=SC2086 # AGENTS is a space-separated list
AGENTS_NS="${AGENTS_NS}" PLATFORM_NS="${PLATFORM_NS}" CLAUDE_PROXY_IMAGE="${CLAUDE_PROXY_IMAGE}" IMAGE_PULL_POLICY=Never \
  CLAUDE_BUDGET_TOKENS="${CLAUDE_BUDGET_TOKENS}" CLAUDE_SECRET_SOURCES="$("${SPIKE_DIR}/scripts/claude-proxy-sources.sh" ${AGENTS})" \
  envsubst '${AGENTS_NS} ${PLATFORM_NS} ${CLAUDE_PROXY_IMAGE} ${IMAGE_PULL_POLICY} ${CLAUDE_BUDGET_TOKENS} ${CLAUDE_SECRET_SOURCES}' \
  < "${SPIKE_DIR}/manifests/claude-proxy.yaml.tmpl" | k apply -f - >/dev/null
k -n "${AGENTS_NS}" rollout restart deploy/claude-proxy >/dev/null   # pick up a rebuilt local image and new Secrets
k -n "${AGENTS_NS}" rollout status deploy/claude-proxy --timeout=120s >/dev/null || die "claude-proxy did not become ready"
ok "claude-proxy up: http://claude-proxy.${AGENTS_NS}.svc.cluster.local:8080"

log "Egress allow-list (#65): proxy and NetworkPolicy"
# with the credential proxy on, only claude-proxy reaches the Claude API (R64): drop it from the agents' list
agent_egress_hosts=$(echo "${EGRESS_HOSTS} ${EGRESS_EXTRA_HOSTS}" | xargs -n1 |
  { if [[ "${CLAUDE_PROXY}" == true ]]; then grep -vx 'api\.anthropic\.com'; else cat; fi; } | xargs)
# the forge hosts (GitHub) are for the forge sidecar only: the proxy wants its credential (manifests/egress.yaml.tmpl)
FORGE_PROXY_KEY=$(secret_file "${SPIKE_DIR}/.secrets/egress-forge-proxy.key")
forge_auth="^Basic $(printf 'forge:%s' "${FORGE_PROXY_KEY}" | base64 | tr -d '\n')\$"
printf '%s' "${FORGE_PROXY_KEY}" | k -n "${AGENTS_NS}" create secret generic egress-forge-proxy --from-file=password=/dev/stdin \
  --from-literal=auth="${forge_auth}" --dry-run=client -o yaml | k apply -f - >/dev/null
egress_render() {
  AGENTS_NS="${AGENTS_NS}" PLATFORM_NS="${PLATFORM_NS}" FORGE_NS="${FORGE_NS}" BOTBOARD_NS="${BOTBOARD_NS}" \
    EGRESS_HOSTS="${agent_egress_hosts}" FORGE_EGRESS_HOSTS="${FORGE_EGRESS_HOSTS}" EGRESS_PROXY_IMAGE="${EGRESS_PROXY_IMAGE}" \
    FORGE_PROXY_AUTH_SHA="$(printf '%s' "${forge_auth}" | openssl dgst -sha256 -r | cut -c1-16)" \
    envsubst '${AGENTS_NS} ${PLATFORM_NS} ${FORGE_NS} ${BOTBOARD_NS} ${EGRESS_HOSTS} ${FORGE_EGRESS_HOSTS} ${EGRESS_PROXY_IMAGE} ${FORGE_PROXY_AUTH_SHA}' \
    < "${SPIKE_DIR}/manifests/egress.yaml.tmpl"
}
egress_render | k apply -f - >/dev/null
AGENTS_NS="${AGENTS_NS}" envsubst '${AGENTS_NS}' < "${SPIKE_DIR}/manifests/egress-proxy-web.yaml.tmpl" | k apply -f - >/dev/null
k -n "${AGENTS_NS}" rollout status deploy/egress-proxy --timeout=180s >/dev/null
ok "egress-proxy up; agents may reach only: ${agent_egress_hosts} (forge sidecar also: ${FORGE_EGRESS_HOSTS})"

log "Agents"
# agents are Agent objects (#47, #57): the operator turns them into StatefulSets. Install it when missing; to
# pick up operator changes run ./run.sh 12 yourself.
if ! k -n "${PLATFORM_NS}" get deploy agentdev-operator >/dev/null 2>&1; then
  "${SPIKE_DIR}/12-operator.sh"
fi
for a in ${AGENTS}; do
  if [[ -s "${SPIKE_DIR}/.secrets/${a}.claude" ]]; then
    k -n "${AGENTS_NS}" create secret generic "claude-${a}" --from-env-file="${SPIKE_DIR}/.secrets/${a}.claude" \
      --dry-run=client -o yaml | k apply -f - >/dev/null
    cred="own Claude credential"; secret="claude-${a}"
  elif (( ${#shared[@]} )); then cred="shared Claude credential"; secret="claude-shared"
    # a per-agent secret left over from an earlier deploy must not linger
    k -n "${AGENTS_NS}" delete secret "claude-${a}" --ignore-not-found >/dev/null
  else die "${a} has no Claude credential: add .secrets/${a}.claude or set ANTHROPIC_API_KEY"
  fi
  [[ -n "${secret:-}" ]] || die "${a}: no Claude secret selected"
  # a blocked agent (#27, scripts/agent-block.sh) stays blocked and stopped across redeploys: the operator keeps it at 0
  blocked=$(k -n "${AGENTS_NS}" get statefulset "agent-${a}" -o jsonpath='{.metadata.annotations.agentdev/blocked}' 2>/dev/null || true)
  # the agent's tokens; the operator mounts them by name (agent-<name>)
  # GitHub (#2): the App's id and key instead of a forge token, in the same per-agent Secret (forge container only)
  if [[ "${FORGE_KIND}" == github ]]; then
    k -n "${AGENTS_NS}" create secret generic "agent-${a}" --from-file=GITHUB_APP_ID="${gh_app}/app-id" \
        --from-file=GITHUB_APP_PRIVATE_KEY="${gh_app}/private-key.pem" --from-literal=GITHUB_INSTALLATION_ID="${GITHUB_INSTALLATION_ID}" \
        --from-literal=WAKE_TOKEN="$(secret_file "${SPIKE_DIR}/.secrets/${a}.wake")" --from-literal=REGISTER_TOKEN="${REGISTER_TOKEN}" \
        --dry-run=client -o yaml \
      | k apply --server-side --force-conflicts --field-manager=agentdev -f - >/dev/null
  else
  printf 'FORGE_TOKEN=%s\nWAKE_TOKEN=%s\nREGISTER_TOKEN=%s\n' "$(cat "${SPIKE_DIR}/.secrets/${a}.token")" \
      "$(secret_file "${SPIKE_DIR}/.secrets/${a}.wake")" "${REGISTER_TOKEN}" \
    | k -n "${AGENTS_NS}" create secret generic "agent-${a}" --from-env-file=/dev/stdin --dry-run=client -o yaml \
    | k apply --server-side --force-conflicts --field-manager=agentdev -f - >/dev/null
  fi
  # the agent's key for the Claude credential proxy (#66); the agent container gets this, never the credential
  printf 'ANTHROPIC_AUTH_TOKEN=%s.%s\n' "${a}" "$(proxy_key "${a}")" \
    | k -n "${AGENTS_NS}" create secret generic "agent-${a}-proxy" --from-env-file=/dev/stdin --dry-run=client -o yaml \
    | k apply -f - >/dev/null
  # the agent itself (#47, #57): the operator renders its Service and StatefulSet; an existing one is adopted in place
  jq -n --arg n "${a}" --arg ns "${AGENTS_NS}" --arg repo "${REPO}" --arg claude "${secret}" --arg forge "${FORGE_KIND}" \
      --arg maxc "${MAX_CONCURRENT}" --arg s2z "${SCALE_TO_ZERO}" \
      '{apiVersion:"agentdev.dev/v1alpha1", kind:"Agent", metadata:{name:$n, namespace:$ns},
        spec:{repo:$repo, forge:$forge, claudeSecret:$claude, maxConcurrent:($maxc|tonumber),
              scaleToZero:($s2z|ascii_downcase|IN("true","1","yes"))}}' \
    | k apply --server-side --force-conflicts --field-manager=agentdev -f - >/dev/null
  if [[ "${blocked}" == true ]]; then
    warn "${a} is blocked: kept at 0 (scripts/agent-block.sh unblock ${a})"; continue
  fi
  # pick up a rebuilt image / changed secret (the operator owns the spec; a restart only replaces the pod)
  k -n "${AGENTS_NS}" wait --for=jsonpath='{.status.observedGeneration}'="$(k -n "${AGENTS_NS}" get agent "${a}" -o jsonpath='{.metadata.generation}')" \
    agent "${a}" --timeout=60s >/dev/null || die "operator did not reconcile agent ${a} (kubectl -n ${PLATFORM_NS} logs deploy/agentdev-operator)"
  k -n "${AGENTS_NS}" rollout restart statefulset "agent-${a}" >/dev/null
  ok "${a} deployed (${cred})"
done
for a in ${AGENTS}; do
  [[ "$(k -n "${AGENTS_NS}" get statefulset "agent-${a}" -o jsonpath='{.metadata.annotations.agentdev/blocked}' 2>/dev/null || true)" == true ]] && continue
  k -n "${AGENTS_NS}" rollout status statefulset "agent-${a}" --timeout=300s >/dev/null
done

if [[ "${FORGE_KIND}" == github ]]; then
  ok "GitHub: the App's webhook goes to its public_url (GKE); on kind the agents poll every ${POLL_SECONDS}s"
  echo "  Work:   label an issue in ${REPO} with agent:<name>, one of: ${AGENTS}"
  record "agents running under gVisor on GitHub: ${AGENTS}"
  exit 0
fi
log "Forge webhook (${REPO} -> router)"
# shellcheck source=lib-forge.sh
source "${SPIKE_DIR}/lib-forge.sh"
url="http://webhook-router.${AGENTS_NS}.svc.cluster.local:8080/"
# idempotent: one hook per repo pointing at the router; also removes per-agent hooks from earlier versions
forge_webhook "${REPO}" "${url}" "${GITEA_WEBHOOK_SECRET}" ".${AGENTS_NS}.svc.cluster.local"
ok "webhook -> ${url}"
# registrations are refreshed every 60 s; show who the router knows right now
sleep 5
k -n "${AGENTS_NS}" exec deploy/webhook-router -- node -e \
  "require('http').get('http://localhost:8080/status',r=>{let b='';r.on('data',c=>b+=c);r.on('end',()=>console.log('  router knows: '+JSON.parse(b).agents.map(a=>a.agent).join(', ')))})"
record "agents running under gVisor: ${AGENTS}"
k -n "${AGENTS_NS}" get pods -l app=coding-agent
echo
echo "  Forge:  ${FORGE_HOST_URL}  (login ${FORGE_ADMIN_USER} / ${FORGE_ADMIN_PASSWORD})"
echo "  Logs:   ${KUBECTL} -n ${AGENTS_NS} logs -f -l app=coding-agent --prefix"
echo "  Work:   create an issue in ${REPO} and assign it to one of: ${AGENTS}"

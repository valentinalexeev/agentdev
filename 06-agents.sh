#!/usr/bin/env bash
# MVP agents: build the worker image, one StatefulSet per agent account, Claude credentials as Secrets.
#
# Claude credentials, in order of precedence per agent:
#   .secrets/<agent>.claude  file with lines like CLAUDE_CODE_OAUTH_TOKEN=... or ANTHROPIC_API_KEY=...
#   ANTHROPIC_API_KEY / CLAUDE_CODE_OAUTH_TOKEN from the environment (.env)  -> shared by all agents
source "$(dirname "$0")/lib.sh"
need envsubst; need yq

log "Building and loading ${WORKER_IMAGE} and ${ROUTER_IMAGE}"
"${CONTAINER_ENGINE}" build -q -t "${WORKER_IMAGE}" "${SPIKE_DIR}/images/worker" >/dev/null
"${CONTAINER_ENGINE}" build -q -t "${ROUTER_IMAGE}" "${SPIKE_DIR}/images/router" >/dev/null
kind load docker-image "${WORKER_IMAGE}" "${ROUTER_IMAGE}" --name "${CLUSTER_NAME}" >/dev/null
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
REGISTER_TOKEN=$(secret_file "${SPIKE_DIR}/.secrets/router-register.token")

log "Webhook router"
AGENTS_NS="${AGENTS_NS}" FORGE_SECRETS="gitea=${GITEA_WEBHOOK_SECRET}" REGISTER_TOKEN="${REGISTER_TOKEN}" \
  ROUTER_IMAGE="${ROUTER_IMAGE}" IMAGE_PULL_POLICY=Never \
  envsubst '${AGENTS_NS} ${FORGE_SECRETS} ${REGISTER_TOKEN} ${ROUTER_IMAGE} ${IMAGE_PULL_POLICY}' \
  < "${SPIKE_DIR}/manifests/router.yaml.tmpl" | k apply -f - >/dev/null
k -n "${AGENTS_NS}" rollout restart deploy/webhook-router >/dev/null   # pick up a rebuilt local image
k -n "${AGENTS_NS}" rollout status deploy/webhook-router --timeout=180s >/dev/null
ok "router up: http://webhook-router.${AGENTS_NS}.svc.cluster.local:8080"

log "Agents"
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
  # a blocked agent (#27, scripts/agent-block.sh) stays blocked and stopped across redeploys
  blocked=$(k -n "${AGENTS_NS}" get statefulset "agent-${a}" -o jsonpath='{.metadata.annotations.agentdev/blocked}' 2>/dev/null || true)
  AGENT="${a}" FORGE_TOKEN="$(cat "${SPIKE_DIR}/.secrets/${a}.token")" AGENTS_NS="${AGENTS_NS}" \
    WAKE_TOKEN="$(secret_file "${SPIKE_DIR}/.secrets/${a}.wake")" REGISTER_TOKEN="${REGISTER_TOKEN}" \
    WORKER_IMAGE="${WORKER_IMAGE}" REPO="${REPO}" POLL_SECONDS="${POLL_SECONDS}" FORGE_ADMIN_USER="${FORGE_ADMIN_USER}" CLAUDE_SECRET="${secret}" \
    IMAGE_PULL_POLICY=Never FORGE_KIND=gitea FORGE_URL="http://gitea.${FORGE_NS}.svc.cluster.local:3000" MAX_CONCURRENT="${MAX_CONCURRENT}" \
    SCALE_TO_ZERO="${SCALE_TO_ZERO}" IDLE_SECONDS="${IDLE_SECONDS}" \
    envsubst < "${SPIKE_DIR}/manifests/agent.yaml.tmpl" \
    | if [[ "${blocked}" == true ]]; then   # one apply, already at 0 and still annotated: no pod flashes up
        yq 'select(.kind == "StatefulSet") |= (.spec.replicas = 0 | .metadata.annotations."agentdev/blocked" = "true")'
      else cat; fi \
    | k apply -f - >/dev/null
  if [[ "${blocked}" == true ]]; then
    warn "${a} is blocked: kept at 0 (scripts/agent-block.sh unblock ${a})"; continue
  fi
  # pick up a rebuilt image / changed secret
  k -n "${AGENTS_NS}" rollout restart statefulset "agent-${a}" >/dev/null
  ok "${a} deployed (${cred})"
done
for a in ${AGENTS}; do
  [[ "$(k -n "${AGENTS_NS}" get statefulset "agent-${a}" -o jsonpath='{.metadata.annotations.agentdev/blocked}' 2>/dev/null || true)" == true ]] && continue
  k -n "${AGENTS_NS}" rollout status statefulset "agent-${a}" --timeout=300s >/dev/null
done

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

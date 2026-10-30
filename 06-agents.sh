#!/usr/bin/env bash
# MVP agents: build the worker image, one StatefulSet per agent account, Claude credentials as Secrets.
#
# Claude credentials, in order of precedence per agent:
#   .secrets/<agent>.claude  file with lines like CLAUDE_CODE_OAUTH_TOKEN=... or ANTHROPIC_API_KEY=...
#   ANTHROPIC_API_KEY / CLAUDE_CODE_OAUTH_TOKEN from the environment (.env)  -> shared by all agents
source "$(dirname "$0")/lib.sh"
need envsubst

log "Building and loading ${WORKER_IMAGE}"
"${CONTAINER_ENGINE}" build -q -t "${WORKER_IMAGE}" "${SPIKE_DIR}/images/worker" >/dev/null
kind load docker-image "${WORKER_IMAGE}" --name "${CLUSTER_NAME}" >/dev/null
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
ROUTER_JS=$(sed 's/^/    /' "${SPIKE_DIR}/images/router/router.js")
ROUTER_HASH=$(shasum -a 256 "${SPIKE_DIR}/images/router/router.js" | cut -c1-12)
AGENTS_NS="${AGENTS_NS}" FORGE_SECRETS="gitea=${GITEA_WEBHOOK_SECRET}" REGISTER_TOKEN="${REGISTER_TOKEN}" \
  ROUTER_JS="${ROUTER_JS}" ROUTER_HASH="${ROUTER_HASH}" \
  envsubst '${AGENTS_NS} ${FORGE_SECRETS} ${REGISTER_TOKEN} ${ROUTER_JS} ${ROUTER_HASH}' \
  < "${SPIKE_DIR}/manifests/router.yaml.tmpl" | k apply -f - >/dev/null
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
  AGENT="${a}" FORGE_TOKEN="$(cat "${SPIKE_DIR}/.secrets/${a}.token")" AGENTS_NS="${AGENTS_NS}" \
    WAKE_TOKEN="$(secret_file "${SPIKE_DIR}/.secrets/${a}.wake")" REGISTER_TOKEN="${REGISTER_TOKEN}" \
    WORKER_IMAGE="${WORKER_IMAGE}" REPO="${REPO}" POLL_SECONDS="${POLL_SECONDS}" FORGE_ADMIN_USER="${FORGE_ADMIN_USER}" CLAUDE_SECRET="${secret}" \
    IMAGE_PULL_POLICY=Never FORGE_KIND=gitea FORGE_URL="http://gitea.${FORGE_NS}.svc.cluster.local:3000" \
    envsubst < "${SPIKE_DIR}/manifests/agent.yaml.tmpl" | k apply -f - >/dev/null
  # pick up a rebuilt image / changed secret
  k -n "${AGENTS_NS}" rollout restart statefulset "agent-${a}" >/dev/null
  ok "${a} deployed (${cred})"
done
for a in ${AGENTS}; do
  k -n "${AGENTS_NS}" rollout status statefulset "agent-${a}" --timeout=300s >/dev/null
done

log "Forge webhook (${REPO} -> router)"
ga() {  # ga METHOD PATH [body]  as the forge owner
  curl -fsS -u "${FORGE_ADMIN_USER}:${FORGE_ADMIN_PASSWORD}" -X "$1" -H 'Content-Type: application/json' \
    "${FORGE_HOST_URL}/api/v1$2" ${3:+-d "$3"}
}
url="http://webhook-router.${AGENTS_NS}.svc.cluster.local:8080/"
# idempotent: one hook per repo pointing at the router; also removes per-agent hooks from earlier versions
for id in $(ga GET "/repos/${REPO}/hooks" | jq -r --arg ns ".${AGENTS_NS}.svc.cluster.local" '.[] | select(.config.url | contains($ns)) | .id'); do
  ga DELETE "/repos/${REPO}/hooks/${id}" >/dev/null
done
ga POST "/repos/${REPO}/hooks" "$(jq -n --arg u "${url}" --arg s "${GITEA_WEBHOOK_SECRET}" \
  '{type:"gitea", active:true, events:["issues","issue_assign","issue_comment","pull_request","pull_request_assign","pull_request_comment","pull_request_review_approved","pull_request_review_rejected","pull_request_review_comment"],
    config:{url:$u, content_type:"json", secret:$s}}')" >/dev/null
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

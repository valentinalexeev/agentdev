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

log "Agents"
for a in ${AGENTS}; do
  if [[ -s "${SPIKE_DIR}/.secrets/${a}.claude" ]]; then
    k -n "${AGENTS_NS}" create secret generic "claude-${a}" --from-env-file="${SPIKE_DIR}/.secrets/${a}.claude" \
      --dry-run=client -o yaml | k apply -f - >/dev/null
    cred="own Claude credential"
  elif (( ${#shared[@]} )); then cred="shared Claude credential"
  else die "${a} has no Claude credential: add .secrets/${a}.claude or set ANTHROPIC_API_KEY"
  fi
  AGENT="${a}" FORGE_TOKEN="$(cat "${SPIKE_DIR}/.secrets/${a}.token")" AGENTS_NS="${AGENTS_NS}" \
    WORKER_IMAGE="${WORKER_IMAGE}" REPO="${REPO}" POLL_SECONDS="${POLL_SECONDS}" FORGE_ADMIN_USER="${FORGE_ADMIN_USER}" \
    envsubst < "${SPIKE_DIR}/manifests/agent.yaml.tmpl" | k apply -f - >/dev/null
  # pick up a rebuilt image / changed secret
  k -n "${AGENTS_NS}" rollout restart statefulset "agent-${a}" >/dev/null
  ok "${a} deployed (${cred})"
done
for a in ${AGENTS}; do
  k -n "${AGENTS_NS}" rollout status statefulset "agent-${a}" --timeout=300s >/dev/null
done
record "agents running under gVisor: ${AGENTS}"
k -n "${AGENTS_NS}" get pods -l app=coding-agent
echo
echo "  Forge:  ${FORGE_HOST_URL}  (login ${FORGE_ADMIN_USER} / ${FORGE_ADMIN_PASSWORD})"
echo "  Logs:   ${KUBECTL} -n ${AGENTS_NS} logs -f -l app=coding-agent --prefix"
echo "  Work:   create an issue in ${REPO} and assign it to one of: ${AGENTS}"

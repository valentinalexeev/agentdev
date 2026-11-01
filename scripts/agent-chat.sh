#!/usr/bin/env bash
# agent-chat: interactive Claude Code sessions in a project (#38).
#   scripts/agent-chat.sh start  <project.yaml>   start a session and attach to it
#   scripts/agent-chat.sh attach [session]        attach (again) from any terminal with cluster access; Ctrl-b d detaches
#   scripts/agent-chat.sh list                    running sessions
#   scripts/agent-chat.sh stop   <session>        end a session now
#
# A session is a short-lived Job (manifests/chat.yaml.tmpl) with a fresh clone of the project, under gVisor. It uses the
# project's chat account: read access plus issues and comments, no push. It ends after IDLE_SECONDS (default 1800)
# with nobody attached.
#
# Claude (#118): the session pod holds no Claude credential. Claude Code calls claude-proxy (ANTHROPIC_BASE_URL) with
# the proxy key of one agent of the project (CHAT_AGENT, default: the first agent in the spec), so budget and usage
# count for that agent. The pod falls under agent-egress like an agent pod (no direct path to api.anthropic.com).
# REMOTE_CONTROL=true (Claude app, #26) needs a full claude.ai login in the pod, which the proxy cannot give: it is
# refused.
source "$(dirname "$0")/../lib.sh"
# shellcheck disable=SC1091
[[ -f "${SPIKE_DIR}/.env" ]] && { set -a; source "${SPIKE_DIR}/.env"; set +a; }
need jq; need envsubst

attach() {
  local s=$1 pod=""
  # the Job's pod appears a moment after apply
  for _ in $(seq 1 60); do
    pod=$(k -n "${AGENTS_NS}" get pod -l "session=${s}" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
    [[ -n "${pod}" ]] && break; sleep 2
  done
  [[ -n "${pod}" ]] || die "no running session ${s}"
  k -n "${AGENTS_NS}" wait --for=condition=Ready "pod/${pod}" --timeout=180s >/dev/null || die "session ${s} did not start"
  # wait for tmux inside: the clone may take a moment
  local up=""
  for _ in $(seq 1 60); do k -n "${AGENTS_NS}" exec "${pod}" -- tmux has-session -t chat 2>/dev/null && { up=1; break; }; sleep 2; done
  [[ -n "${up}" ]] || die "session ${s} has no chat yet (see: ${KUBECTL} -n ${AGENTS_NS} logs ${pod})"
  ok "attaching to ${s} (detach: Ctrl-b d; the session keeps running until idle)"
  exec ${KUBECTL} -n "${AGENTS_NS}" exec -it "${pod}" -- tmux attach -t chat
}

sessions() { k -n "${AGENTS_NS}" get jobs -l app=agent-chat -o jsonpath='{range .items[?(@.status.active)]}{.metadata.name}{"\t"}{.metadata.labels.project}{"\t"}{.metadata.creationTimestamp}{"\n"}{end}'; }

case "${1:-}" in
  start)
    need yq
    spec=${2:?usage: $0 start <project.yaml>}
    project=$(yq -r '.project' "${spec}"); REPO=$(yq -r '.forge.repo' "${spec}")
    [[ "${project}" =~ ^[a-z0-9-]+$ && "${REPO}" =~ ^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$ ]] || die "spec: project (a-z0-9-) and forge.repo (owner/name) are required"
    gitea_cli() { k -n "${FORGE_NS}" exec deploy/gitea -- gitea "$@" 2>/dev/null; }
    # shellcheck source=../lib-forge.sh
    source "${SPIKE_DIR}/lib-forge.sh"
    chat="${project}-chat"; tf="${SPIKE_DIR}/.secrets/${chat}.token"
    if [[ ! -s "${tf}" ]]; then
      forge_mkuser "${chat}" "$(openssl rand -hex 12)Aa1!"
      ga PUT "/repos/${REPO}/collaborators/${chat}" '{"permission":"read"}' >/dev/null
      # read code, write issues and comments; no write:repository, so no push
      gitea_cli admin user generate-access-token --username "${chat}" --token-name "chat-$(date +%s)" \
        --scopes read:repository,write:issue,read:user --raw | tr -d '\r\n' > "${tf}"
      chmod 600 "${tf}"
    fi
    [[ -s "${tf}" ]] || die "no chat token for ${chat}"
    k -n "${AGENTS_NS}" create secret generic "chat-${project}" --from-file=FORGE_TOKEN="${tf}" \
      --dry-run=client -o yaml | k apply -f - >/dev/null
    [[ "${CLAUDE_PROXY:-true}" == true ]] || die "chat sessions need the Claude credential proxy (CLAUDE_PROXY=true, #118)"
    [[ "${REMOTE_CONTROL:-false}" == true ]] && die "REMOTE_CONTROL needs a claude.ai login in the session pod; sessions use claude-proxy (#118)"
    chat_agent="${CHAT_AGENT:-$(yq -r '.agents[0].name // ""' "${spec}")}"
    [[ "${chat_agent}" =~ ^[a-z0-9]([a-z0-9-]*[a-z0-9])?$ ]] || die "spec: no agent for the session (agents[0].name or CHAT_AGENT)"
    k -n "${AGENTS_NS}" get secret "agent-${chat_agent}-proxy" >/dev/null 2>&1 \
      || die "no proxy key agent-${chat_agent}-proxy (run ./run.sh 06 first)"
    claude_proxy_url="http://claude-proxy.${AGENTS_NS}.svc.cluster.local:8080"   # same URLs as the operator (12-operator.sh)
    egress_proxy="http://egress-proxy.${AGENTS_NS}.svc.cluster.local:3128"
    session="chat-${project}-$(date +%H%M%S)"
    board_url="${BOTBOARD_URL:-}"   # report the session to botboard (#26) when it is deployed
    [[ -z "${board_url}" ]] && k -n "${BOTBOARD_NS:-botboard}" get svc botboard >/dev/null 2>&1 && board_url="http://botboard.${BOTBOARD_NS:-botboard}.svc.cluster.local"
    BOTBOARD_URL="${board_url}" CHAT_URL="${CHAT_URL:-}" \
    SESSION="${session}" PROJECT="${project}" AGENTS_NS="${AGENTS_NS}" WORKER_IMAGE="${WORKER_IMAGE}" IMAGE_PULL_POLICY="${IMAGE_PULL_POLICY:-Never}" \
      FORGE_URL="http://gitea.${FORGE_NS}.svc.cluster.local:3000" CHAT_USER="${chat}" REPO="${REPO}" \
      IDLE_SECONDS="${IDLE_SECONDS:-1800}" REMOTE_CONTROL=false CHAT_AGENT="${chat_agent}" \
      CLAUDE_PROXY_URL="${claude_proxy_url}" EGRESS_PROXY="${egress_proxy}" \
      envsubst < "${SPIKE_DIR}/manifests/chat.yaml.tmpl" | k apply -f - >/dev/null
    ok "session ${session} started for ${REPO} (Claude through claude-proxy as agent ${chat_agent})"
    [[ -t 0 ]] && attach "${session}" || echo "  attach: $0 attach ${session}"
    ;;
  attach)
    s=${2:-$(sessions | tail -1 | cut -f1)}; [[ -n "${s}" ]] || die "no running session"
    attach "${s}" ;;
  list) sessions | column -t ;;
  stop) k -n "${AGENTS_NS}" delete job "${2:?usage: $0 stop <session>}" >/dev/null && ok "stopped ${2}" ;;
  *) sed -n '2,6p' "$0"; exit 2 ;;
esac

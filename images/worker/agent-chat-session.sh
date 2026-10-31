#!/usr/bin/env bash
# agent-chat-session: one interactive Claude Code session in a project (#38). Runs as the container command of a
# chat Job (manifests/chat.yaml.tmpl). Claude runs inside tmux, so people attach, detach and re-attach from any
# terminal with cluster access (scripts/agent-chat.sh attach). The session ends after IDLE_SECONDS with nobody
# attached; the Job then removes itself.
#
# Forge access is read + comment/issue only (a token without push rights): ideas become issues, and issues become
# code through the normal agent -> PR -> review path.
set -uo pipefail
: "${FORGE_URL:?}" "${FORGE_USER:?}" "${FORGE_TOKEN:?}" "${REPO:?}"
IDLE_SECONDS="${IDLE_SECONDS:-1800}"
WORK=/state/repo
log() { printf '%s [chat %s] %s\n' "$(date -u +%H:%M:%S)" "${REPO}" "$*"; }

host=${FORGE_URL#*://}
git clone -q "${FORGE_URL%%://*}://${FORGE_USER}:${FORGE_TOKEN}@${host}/${REPO}.git" "${WORK}" || { log "clone failed"; exit 1; }
git -C "${WORK}" remote set-url --push origin "no-push://interactive-session"   # belt and braces: the token can't push either

cat > /state/session-prompt.md <<EOF
You are an interactive agentdev session in project ${REPO} (forge: ${FORGE_URL}). A person is chatting with you to
explore ideas before they become tasks. You have a fresh clone in ${WORK}; you may read, run and change files locally.
You cannot push. To turn a result into work, file an issue on the forge with the forge API, for example:
  curl -fsS -X POST -H "Authorization: token \$FORGE_TOKEN" -H 'Content-Type: application/json' \\
    "\$FORGE_URL/api/v1/repos/${REPO}/issues" -d '{"title":"...","body":"..."}'
Use the Task format (What, Acceptance criterion, Open decisions) and leave assignment to the person.
EOF

rc=()
[[ "${REMOTE_CONTROL:-false}" == true ]] && rc=(--remote-control)   # Claude app / claude.ai/code (#26); needs a subscription login
# skip first-run onboarding and the folder-trust prompt: the session must be usable the moment someone attaches
mkdir -p "${CLAUDE_CONFIG_DIR:-${HOME}/.claude}"
jq -n --arg d "${WORK}" '{hasCompletedOnboarding: true, theme: "dark", projects: {($d): {hasTrustDialogAccepted: true}}}' \
  > "${CLAUDE_CONFIG_DIR:-${HOME}/.claude}/.claude.json"
cd "${WORK}" || exit 1
tmux new-session -d -s chat -x 200 -y 50 \
  "claude ${rc[*]} --append-system-prompt \"\$(cat /state/session-prompt.md)\"; echo 'claude exited; detach with Ctrl-b d'; sleep infinity"
log "session ready: attach with scripts/agent-chat.sh attach (idle timeout ${IDLE_SECONDS}s)"

# idle watchdog: count time with no attached client
idle=0
while tmux has-session -t chat 2>/dev/null; do
  if [[ -n "$(tmux list-clients -t chat 2>/dev/null)" ]]; then idle=0; else idle=$((idle + 10)); fi
  if (( idle >= IDLE_SECONDS )); then log "nobody attached for ${IDLE_SECONDS}s: ending the session"; break; fi
  sleep 10
done
tmux kill-server 2>/dev/null
exit 0

#!/usr/bin/env bash
# agent-block: force-stop an agent and keep it stopped (#27), or release it.
#   scripts/agent-block.sh block   <agent> [reason]   annotate agentdev/blocked=true and scale to 0
#   scripts/agent-block.sh unblock <agent>            remove the block and start it again
#   scripts/agent-block.sh list                       blocked agents
#
# While blocked, nothing starts the agent: the router skips it on events (scale to zero, #35) and 06-agents.sh keeps
# it at 0 replicas on redeploy. The UI toggle in #12 sets the same annotation.
source "$(dirname "$0")/../lib.sh"

sts() { echo "agent-${1:?agent name required}"; }
case "${1:-}" in
  block)
    s=$(sts "${2:-}"); reason=${3:-blocked by admin}
    k -n "${AGENTS_NS}" get statefulset "${s}" >/dev/null 2>&1 || die "no agent ${2}"
    k -n "${AGENTS_NS}" annotate statefulset "${s}" --overwrite agentdev/blocked=true \
      "agentdev/blocked-reason=${reason}" "agentdev/blocked-at=$(date -u +%Y-%m-%dT%H:%M:%SZ)" >/dev/null
    k -n "${AGENTS_NS}" scale statefulset "${s}" --replicas=0 >/dev/null
    ok "${2} blocked and stopped (${reason})" ;;
  unblock)
    s=$(sts "${2:-}")
    k -n "${AGENTS_NS}" get statefulset "${s}" >/dev/null 2>&1 || die "no agent ${2}"
    k -n "${AGENTS_NS}" annotate statefulset "${s}" agentdev/blocked- agentdev/blocked-reason- agentdev/blocked-at- >/dev/null
    k -n "${AGENTS_NS}" scale statefulset "${s}" --replicas=1 >/dev/null
    ok "${2} unblocked and started" ;;
  list)
    k -n "${AGENTS_NS}" get statefulset -l app=coding-agent \
      -o jsonpath='{range .items[?(@.metadata.annotations.agentdev/blocked=="true")]}{.metadata.name}{"\t"}{.metadata.annotations.agentdev/blocked-at}{"\t"}{.metadata.annotations.agentdev/blocked-reason}{"\n"}{end}' ;;
  *) sed -n '2,5p' "$0"; exit 2 ;;
esac

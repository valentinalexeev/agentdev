#!/usr/bin/env bash
# Spike check (#14): run claude in an agent pod against that agent's ContextForge virtual server.
#   spikes/contextforge/check-agent.sh <agent>     (reads token + server id from $SCRATCH/cf-<agent>.{tok,sid})
set -uo pipefail
a=$1
SCRATCH=${JCODE_SCRATCH_DIR:-$HOME/.jcode/scratch}
K="kubectl --context kind-agentdev -n coding-agents"
sid=$(cat "${SCRATCH}/cf-${a}.sid"); tok=$(cat "${SCRATCH}/cf-${a}.tok")

# MCP config in the agent's state volume; the token stays in an env var, the file only references it
$K exec -i "agent-${a}-0" -- sh -c 'mkdir -p /state/mcp && cat > /state/mcp/contextforge.json' <<EOF
{"mcpServers":{"project":{"type":"http","url":"http://contextforge.mcp-spike.svc.cluster.local:4444/servers/${sid}/mcp","headers":{"Authorization":"Bearer \${CF_TOKEN}"}}}}
EOF

prompt='List every MCP tool you have from the server named project, one tool name per line, nothing else. Then, if one of them contains secret_hint, call it and print its output; otherwise print NO_SECRET_TOOL.'
$K exec "agent-${a}-0" -- env CF_TOKEN="${tok}" claude -p "${prompt}" \
  --mcp-config /state/mcp/contextforge.json --strict-mcp-config --dangerously-skip-permissions \
  --output-format json --max-turns 6 | jq -r '.result'

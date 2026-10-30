# Coding-agent worker: Claude Code CLI + a polling loop over a forge (Gitea for the MVP).
FROM node:24-bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git ripgrep curl jq procps python3 \
 && rm -rf /var/lib/apt/lists/*

ARG CLAUDE_CODE_VERSION=2.1.312
RUN npm install -g @anthropic-ai/claude-code@${CLAUDE_CODE_VERSION} \
 && npm cache clean --force

COPY agent-loop.sh /usr/local/bin/agent-loop
COPY webhook-listener.js /usr/local/lib/agentdev/webhook-listener.js
RUN chmod +x /usr/local/bin/agent-loop \
 && mkdir -p /state /home/node/.claude && chown -R 1000:1000 /state /home/node/.claude

# The node image already has UID 1000 ("node"). /state and ~/.claude are PVC-backed.
# CLAUDE_CONFIG_DIR keeps .claude.json on the PVC too (by default it sits in $HOME and is lost on restart).
ENV HOME=/home/node IS_SANDBOX=1 CLAUDE_CONFIG_DIR=/home/node/.claude
WORKDIR /state
USER 1000
CMD ["agent-loop"]

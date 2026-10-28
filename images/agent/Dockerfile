# Minimal headless coding-agent image for the spike: Node + git + ripgrep + Claude Code.
# Multi-arch base, so it builds natively on Apple Silicon and x86.
FROM node:24-bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates git ripgrep curl procps \
 && rm -rf /var/lib/apt/lists/*

RUN npm install -g @anthropic-ai/claude-code \
 && npm cache clean --force

# The node image already has UID 1000 ("node"); state dirs are PVC-backed.
RUN mkdir -p /workspace /home/node/.claude && chown -R 1000:1000 /workspace /home/node/.claude
ENV HOME=/home/node IS_SANDBOX=1
WORKDIR /workspace
USER 1000

# Long-lived sandbox: the agent is driven with `kubectl exec ... claude -p`.
CMD ["sleep", "infinity"]

#!/usr/bin/env bash
# Prints the projected-volume sources of the Claude credential proxy (#66) as one line of JSON (YAML flow style):
# one optional Secret source per Claude Secret, each key under a directory with the Secret's name, as the proxy reads
# them (images/credproxy main.go credStore). Usage: claude-proxy-sources.sh <agent>...  (claude-shared is always first)
set -euo pipefail
re='^[a-z0-9]([-a-z0-9]{0,61}[a-z0-9])?$'
src() {
  printf '{"secret":{"name":"%s","optional":true,"items":[{"key":"CLAUDE_CODE_OAUTH_TOKEN","path":"%s/CLAUDE_CODE_OAUTH_TOKEN"},{"key":"ANTHROPIC_API_KEY","path":"%s/ANTHROPIC_API_KEY"}]}}' "$1" "$1" "$1"
}
out=$(src claude-shared)
for a in "$@"; do
  [[ "${a}" =~ ${re} ]] || { echo "bad agent name: ${a}" >&2; exit 1; }
  out+=",$(src "claude-${a}")"
done
printf '[%s]\n' "${out}"

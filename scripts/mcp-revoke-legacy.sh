#!/usr/bin/env bash
# mcp-revoke-legacy (#452, #453): revoke the ContextForge tokens minted before the McpProject controller stopped
# reading Secrets. Their ids live only in the annotation agentdev/mcp-token-id of the agent-<name>-mcp Secrets, which
# the operator can no longer read, so it never revokes them. The owner runs this once, with the owner's kubectl.
#
#   scripts/mcp-revoke-legacy.sh list   > legacy-mcp-token-ids.txt   # BEFORE the upgrade: record the old token ids
#   scripts/mcp-revoke-legacy.sh revoke   legacy-mcp-token-ids.txt   # AFTER the upgrade, once every McpProject is Ready
#   scripts/mcp-revoke-legacy.sh revoke                              # no file: read the ids from the Secrets now
#
# Order: the new operator re-mints every token on its first reconcile and overwrites the annotation with the new id,
# so the old ids must be recorded before the upgrade. Revoking after the upgrade keeps the agents' MCP access up until
# the new tokens are in place. An id that an McpProject status records (a token of the new operator) is never revoked.
#
# No secret value is printed: the token ids are not credentials, the admin password goes to curl on stdin and the admin
# bearer token is kept in a 0600 file removed on exit.
# Env: AGENTS_NS (coding-agents), PLATFORM_NS (agentdev-platform), CF_ADMIN_EMAIL (admin@agentdev.dev), KUBECTL.
set -euo pipefail
AGENTS_NS=${AGENTS_NS:-coding-agents}
PLATFORM_NS=${PLATFORM_NS:-agentdev-platform}
CF_ADMIN_EMAIL=${CF_ADMIN_EMAIL:-admin@agentdev.dev}
KUBECTL=${KUBECTL:-kubectl}
k() { ${KUBECTL} "$@"; }
ok()   { printf '  ok %s\n' "$*" >&2; }
warn() { printf '  ! %s\n' "$*" >&2; }
die()  { printf '  x %s\n' "$*" >&2; exit 1; }
for c in jq curl; do command -v "$c" >/dev/null 2>&1 || die "missing '$c'"; done

# ids from the Secret annotations, one per line, valid characters only
legacy_ids() {
  k -n "${AGENTS_NS}" get secret -l agentdev/mcp-project -o json \
    | jq -r '.items[] | select(.metadata.name | test("^agent-.+-mcp$")) | .metadata.annotations["agentdev/mcp-token-id"] // empty' \
    | grep -E '^[A-Za-z0-9._-]+$' | sort -u || true
}

mode=${1:-}
case "${mode}" in
  list) legacy_ids; exit 0 ;;
  revoke) ;;
  *) die "usage: $0 list | revoke [ids-file]" ;;
esac
if [[ -n "${2:-}" ]]; then
  ids=$(grep -E '^[A-Za-z0-9._-]+$' "$2" | sort -u || true)
else
  ids=$(legacy_ids)
fi
# never revoke a token the new operator recorded
current=$(k -n "${AGENTS_NS}" get mcpprojects.agentdev.dev -o json 2>/dev/null \
  | jq -r '.items[].status.tokens // [] | .[].tokenID // empty' | sort -u || true)
[[ -n "${ids}" ]] || { ok "no legacy token ids: nothing to revoke"; exit 0; }

tmp=$(mktemp -d); chmod 700 "${tmp}"; pf=""
trap '[[ -n "${pf}" ]] && { kill "${pf}" 2>/dev/null; wait "${pf}" 2>/dev/null || true; }; rm -rf "${tmp}"' EXIT
k -n "${PLATFORM_NS}" port-forward svc/contextforge :4444 >"${tmp}/pf.log" 2>&1 & pf=$!
port=""; for _ in $(seq 1 30); do port=$(sed -n 's/^Forwarding from 127.0.0.1:\([0-9]*\).*/\1/p' "${tmp}/pf.log" | head -1); [[ -n "${port}" ]] && break; sleep 1; done
[[ -n "${port}" ]] || die "port-forward to ContextForge failed"
B="http://127.0.0.1:${port}"
healthy=0; for _ in $(seq 1 30); do curl -fs "${B}/health" >/dev/null 2>&1 && { healthy=1; break; }; sleep 1; done
[[ ${healthy} == 1 ]] || die "ContextForge is not healthy"

# admin password: the same key the operator reads; it goes through pipes only, never argv or the terminal
k -n "${PLATFORM_NS}" get secret contextforge -o jsonpath='{.data.PLATFORM_ADMIN_PASSWORD}' | base64 -d \
  | jq -Rs --arg e "${CF_ADMIN_EMAIL}" '{email:$e, password:(.|rtrimstr("\n"))}' \
  | curl -fsS -X POST "${B}/auth/login" -H 'Content-Type: application/json' --data @- \
  | jq -r '.access_token // empty | "Authorization: Bearer " + .' >"${tmp}/hdr" || true
chmod 600 "${tmp}/hdr"
[[ -s "${tmp}/hdr" ]] || die "ContextForge admin login failed"

revoked=0 failed=0 skipped=0
while read -r id; do
  [[ -n "${id}" ]] || continue
  if grep -qxF "${id}" <<<"${current}"; then skipped=$((skipped + 1)); ok "${id}: in use by an McpProject, kept"; continue; fi
  code=$(curl -s -o /dev/null -w '%{http_code}' -X DELETE "${B}/tokens/${id}" -H @"${tmp}/hdr")
  case "${code}" in
    2??|404) revoked=$((revoked + 1)); ok "${id}: revoked (HTTP ${code})" ;;
    *) failed=$((failed + 1)); warn "${id}: HTTP ${code}" ;;
  esac
done <<<"${ids}"
printf 'revoked %d, kept %d, failed %d\n' "${revoked}" "${skipped}" "${failed}"
[[ ${failed} == 0 ]]

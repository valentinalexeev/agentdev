#!/usr/bin/env bash
# mcp-sync (#14, ADR 0001): make ContextForge match the `mcp:` section of a project spec.
#   scripts/mcp-sync.sh <project.yaml>
#
# Spec:
#   mcp:
#     servers:                       # MCP servers this project may use (registered as ContextForge gateways)
#       - {name: docs, url: http://docs-mcp.tools.svc.cluster.local:8080/mcp}
#
# Every agent of the project gets the project's tools (one project per agent, decided in #14). Per-agent subsets would
# need one virtual server per agent (a token is scoped to one server): not in v1.
#
# Result, idempotent:
#   - each server registered once (gateway <project>-<name>); its tools discovered by ContextForge
#   - one virtual server per project (project-<project>) with exactly those tools
#   - one API token per agent, scoped to that virtual server (server_id), stored as Secret agent-<agent>-mcp
#     {CF_URL, CF_TOKEN}. The agent's pod reads it (optional secretRef); a token never leaves the cluster.
# Separation is enforced by ContextForge: an agent's token is refused on any other server (verified in the ADR spike).
source "$(dirname "$0")/../lib.sh"
# shellcheck disable=SC1091
[[ -f "${SPIKE_DIR}/.env" ]] && { set -a; source "${SPIKE_DIR}/.env"; set +a; }
need yq; need jq; need curl

spec=${1:?usage: $0 <project.yaml>}
project=$(yq -r '.project' "${spec}")
[[ "${project}" =~ ^[a-z0-9-]+$ ]] || die "spec: project is required (a-z0-9-)"
has_mcp=1; [[ "$(yq -r '.mcp.servers // [] | length' "${spec}")" != 0 ]] || has_mcp=0
if ! k -n "${PLATFORM_NS}" get svc contextforge >/dev/null 2>&1; then
  [[ ${has_mcp} == 0 ]] && { ok "no mcp.servers in ${spec}: nothing to do"; exit 0; }
  die "${spec} has mcp: but ContextForge is not deployed: run ./run.sh 08 first"
fi
CF_IN_CLUSTER="http://contextforge.${PLATFORM_NS}.svc.cluster.local:4444"

# reach the admin API through a short-lived port-forward (no public endpoint)
pflog=$(mktemp)
k -n "${PLATFORM_NS}" port-forward svc/contextforge :4444 >"${pflog}" 2>&1 & pf=$!   # kubectl picks a free port
trap 'kill ${pf} 2>/dev/null; rm -f "${pflog}"' EXIT
port=""; for _ in $(seq 1 30); do port=$(sed -n 's/^Forwarding from 127.0.0.1:\([0-9]*\).*/\1/p' "${pflog}" | head -1); [[ -n "${port}" ]] && break; sleep 1; done
[[ -n "${port}" ]] || die "port-forward to ContextForge failed: $(cat "${pflog}")"
B="http://127.0.0.1:${port}"
healthy=0; for _ in $(seq 1 30); do curl -fs "${B}/health" >/dev/null 2>&1 && { healthy=1; break; }; sleep 1; done
[[ ${healthy} == 1 ]] || die "ContextForge is not healthy (kubectl -n ${PLATFORM_NS} logs deploy/contextforge)"
# secrets go through stdin, never argv (visible in ps)
T=$(jq -n --arg e "${CF_ADMIN_EMAIL}" --rawfile p "${SPIKE_DIR}/.secrets/platform/cf-admin" '{email:$e, password:("Aa1!" + ($p|rtrimstr("\n")))}' \
    | curl -fsS -X POST "${B}/auth/login" -H 'Content-Type: application/json' --data @- | jq -r .access_token)
[[ -n "${T}" && "${T}" != null ]] || die "ContextForge admin login failed"
hdr=$(mktemp); chmod 600 "${hdr}"; printf 'Authorization: Bearer %s\n' "${T}" >"${hdr}"
trap 'kill ${pf} 2>/dev/null; rm -f "${pflog}" "${hdr}"' EXIT
cf() { if [[ -n "${3:-}" ]]; then printf '%s' "$3" | curl -fsS -X "$1" "${B}$2" -H @"${hdr}" -H 'Content-Type: application/json' --data @-
       else curl -fsS -X "$1" "${B}$2" -H @"${hdr}"; fi; }
# revoke a token minted by this script (its ContextForge id is kept as an annotation on the agent's Secret)
revoke() { local id; id=$(k -n "${AGENTS_NS}" get secret "$1" -o jsonpath='{.metadata.annotations.agentdev/mcp-token-id}' 2>/dev/null)
  [[ -z "${id}" ]] || cf DELETE "/tokens/${id}" >/dev/null 2>&1 || warn "could not revoke token ${id}"; }

# cleanup: Secrets of this project whose agent left the spec, or every one if the spec dropped mcp:
keep=$([[ ${has_mcp} == 1 ]] && yq -r '.agents[].name' "${spec}" | sed 's/^/agent-/;s/$/-mcp/')
for s in $(k -n "${AGENTS_NS}" get secret -l "agentdev/mcp-project=${project}" -o name 2>/dev/null | sed 's|^secret/||'); do
  grep -qx "${s}" <<<"${keep}" && continue
  revoke "${s}"; k -n "${AGENTS_NS}" delete secret "${s}" >/dev/null; ok "${s}: removed, token revoked"
done
[[ ${has_mcp} == 1 ]] || { ok "no mcp.servers in ${spec}: nothing more to do"; exit 0; }

log "1. MCP servers for ${project}"
gateways=$(cf GET /gateways)
while IFS=$'\t' read -r name url; do
  gname="${project}-${name}"
  if jq -e --arg n "${gname}" '.[] | select(.name == $n)' <<<"${gateways}" >/dev/null; then
    ok "${gname}: registered"
  else
    cf POST /gateways "$(jq -n --arg n "${gname}" --arg u "${url}" '{name:$n, url:$u, transport:"STREAMABLEHTTP"}')" >/dev/null \
      || die "could not register ${gname} (${url}); it must be reachable from ContextForge (cluster Service range only)"
    ok "${gname}: registered ${url}"
  fi
done < <(yq -r '.mcp.servers[] | [.name, .url] | @tsv' "${spec}")

log "2. Virtual server project-${project}"
prefixes=$(yq -r '.mcp.servers[].name' "${spec}" | sed "s/^/${project}-/" | jq -R . | jq -sc .)
# ContextForge names gateway tools "<gateway>-<tool>"; the project's tools are those of its gateways
tools=$(cf GET "/tools?limit=0" | jq -c --argjson p "${prefixes}" \
  '[.[] | select(.gatewaySlug as $g | $p | index($g)) | {id, name}]')
[[ "$(jq length <<<"${tools}")" -gt 0 ]] || die "no tools discovered for ${project}: check the servers are up"
servers=$(cf GET /servers)
sid=$(jq -r --arg n "project-${project}" '.[] | select(.name == $n) | .id' <<<"${servers}" | head -1)
body=$(jq -n --arg n "project-${project}" --argjson t "$(jq '[.[].id]' <<<"${tools}")" \
  '{name:$n, description:"agentdev project (mcp-sync)", associated_tools:$t, visibility:"private"}')
if [[ -z "${sid}" ]]; then
  sid=$(cf POST /servers "{\"server\":${body}}" 2>/dev/null | jq -r .id) || sid=""
  [[ -n "${sid}" && "${sid}" != null ]] || sid=$(cf POST /servers "${body}" | jq -r .id)
else
  cf PUT "/servers/${sid}" "${body}" >/dev/null
fi
[[ -n "${sid}" && "${sid}" != null ]] || die "could not create the virtual server"
ok "project-${project} (${sid}): $(jq -r '[.[].name] | join(", ")' <<<"${tools}")"

log "3. Agent tokens (scoped to project-${project})"
for a in $(yq -r '.agents[].name' "${spec}"); do
  s="agent-${a}-mcp"
  ann=$(k -n "${AGENTS_NS}" get secret "${s}" -o json 2>/dev/null | jq -c '.metadata.annotations // {}') || ann=""
  renew_after=$(jq -r '."agentdev/mcp-renew-after" // 0' <<<"${ann:-null}")
  if [[ -n "${ann}" && "$(jq -r '."agentdev/mcp-server" // ""' <<<"${ann}")" == "${sid}" && $(date +%s) -lt ${renew_after} ]]; then
    ok "${a}: token present"; continue
  fi
  # ContextForge 1.1 rejects a token request without an expiry: a year, re-minted by any sync in the last 30 days
  resp=$(cf POST /tokens "$(jq -n --arg n "agent-${a}-${project}-$(date +%Y%m%d%H%M%S)" --arg s "${sid}" \
        '{name:$n, description:"agentdev agent token (mcp-sync)", expires_in_days:365, scope:{server_id:$s, permissions:[]}}')")
  tok=$(jq -r '.access_token // .token // empty' <<<"${resp}"); tid=$(jq -r '.token.id // .id // empty' <<<"${resp}")
  [[ -n "${tok}" && "${tok}" != null ]] || die "could not mint a token for ${a}"
  [[ -n "${ann}" ]] && revoke "${s}"   # the token it replaces (other server, or close to expiry)
  printf 'CF_URL=%s\nCF_TOKEN=%s\n' "${CF_IN_CLUSTER}/servers/${sid}/mcp" "${tok}" \
    | k -n "${AGENTS_NS}" create secret generic "${s}" --from-env-file=/dev/stdin --dry-run=client -o yaml \
    | k label --local -f - "agentdev/mcp-project=${project}" -o yaml \
    | k annotate --local -f - "agentdev/mcp-server=${sid}" "agentdev/mcp-token-id=${tid}" \
        "agentdev/mcp-renew-after=$(( $(date +%s) + 335*86400 ))" -o yaml \
    | k apply --server-side --force-conflicts --field-manager=mcp-sync -f - >/dev/null   # client-side apply would copy the token into an annotation
  ok "${a}: token scoped to project-${project}, Secret ${s}"
done
record "MCP for project ${project}: server project-${project}, $(jq length <<<"${tools}") tools"

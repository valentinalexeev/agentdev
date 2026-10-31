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
[[ "$(yq -r '.mcp.servers // [] | length' "${spec}")" != 0 ]] || { ok "no mcp.servers in ${spec}: nothing to do"; exit 0; }
k -n "${PLATFORM_NS}" get svc contextforge >/dev/null 2>&1 || die "ContextForge not deployed (./run.sh 08)"
CF_IN_CLUSTER="http://contextforge.${PLATFORM_NS}.svc.cluster.local:4444"

# reach the admin API through a short-lived port-forward (no public endpoint)
port=$(( 20000 + RANDOM % 10000 ))
k -n "${PLATFORM_NS}" port-forward svc/contextforge "${port}:4444" >/dev/null 2>&1 & pf=$!
trap 'kill ${pf} 2>/dev/null' EXIT
B="http://127.0.0.1:${port}"
for _ in $(seq 1 30); do curl -fs "${B}/health" >/dev/null 2>&1 && break; sleep 1; done
admin_pw="Aa1!$(cat "${SPIKE_DIR}/.secrets/platform/cf-admin")"
T=$(curl -fsS -X POST "${B}/auth/login" -H 'Content-Type: application/json' \
      -d "$(jq -n --arg e "${CF_ADMIN_EMAIL}" --arg p "${admin_pw}" '{email:$e, password:$p}')" | jq -r .access_token)
[[ -n "${T}" && "${T}" != null ]] || die "ContextForge admin login failed"
cf() { curl -fsS -X "$1" "${B}$2" -H "Authorization: Bearer ${T}" -H 'Content-Type: application/json' ${3:+-d "$3"}; }

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
  if k -n "${AGENTS_NS}" get secret "agent-${a}-mcp" >/dev/null 2>&1 \
     && [[ "$(k -n "${AGENTS_NS}" get secret "agent-${a}-mcp" -o jsonpath='{.metadata.annotations.agentdev/mcp-server}')" == "${sid}" ]]; then
    ok "${a}: token present"; continue
  fi
  # ContextForge 1.1 rejects a token request without an expiry; agents get a year. Rotate: delete the Secret, re-run.
  tok=$(cf POST /tokens "$(jq -n --arg n "agent-${a}-${project}-$(date +%Y%m%d%H%M)" --arg s "${sid}" \
        '{name:$n, description:"agentdev agent token (mcp-sync)", expires_in_days:365, scope:{server_id:$s, permissions:[]}}')" \
        | jq -r '.access_token // .token')
  [[ -n "${tok}" && "${tok}" != null ]] || die "could not mint a token for ${a}"
  k -n "${AGENTS_NS}" create secret generic "agent-${a}-mcp" \
    --from-literal=CF_URL="${CF_IN_CLUSTER}/servers/${sid}/mcp" --from-literal=CF_TOKEN="${tok}" \
    --dry-run=client -o yaml | k annotate --local -f - "agentdev/mcp-server=${sid}" -o yaml | k apply -f - >/dev/null
  ok "${a}: token scoped to project-${project}, Secret agent-${a}-mcp"
done
record "MCP for project ${project}: server project-${project}, $(jq length <<<"${tools}") tools"

#!/usr/bin/env bash
# MVP forge: Gitea in the cluster, an owner account (you), one account per agent, a seeded repo.
source "$(dirname "$0")/lib.sh"
need jq

log "Gitea"
# only these are substituted (webhook allow-list); other $ signs in the manifest stay literal
AGENTS_NS="${AGENTS_NS}" SERVICE_CIDR="${SERVICE_CIDR}" envsubst '${AGENTS_NS} ${SERVICE_CIDR}' \
  < "${SPIKE_DIR}/manifests/gitea.yaml" | k apply -f - >/dev/null
k -n "${FORGE_NS}" rollout status deploy/gitea --timeout=300s >/dev/null
wait_for 120 "Gitea answers on ${FORGE_HOST_URL}" curl -fsS "${FORGE_HOST_URL}/api/healthz"

gitea_cli() { k -n "${FORGE_NS}" exec deploy/gitea -- gitea "$@" 2>/dev/null; }
# capture first: `grep -q` closing the pipe early trips pipefail
user_exists() { local l; l=$(gitea_cli admin user list) || return 1; awk '{print $2}' <<<"${l}" | grep -qx "$1"; }
mkuser() {  # mkuser <name> <password> [--admin]
  user_exists "$1" && return 0
  gitea_cli admin user create --username "$1" --password "$2" --email "$1@agents.local" \
    --must-change-password=false ${3:-} >/dev/null
}
mkuser "${FORGE_ADMIN_USER}" "${FORGE_ADMIN_PASSWORD}" --admin
ok "owner account: ${FORGE_ADMIN_USER} / ${FORGE_ADMIN_PASSWORD}"

ga() {  # ga METHOD PATH [body]  as the owner (basic auth)
  curl -fsS -u "${FORGE_ADMIN_USER}:${FORGE_ADMIN_PASSWORD}" -X "$1" -H 'Content-Type: application/json' \
    "${FORGE_HOST_URL}/api/v1$2" ${3:+-d "$3"}
}

repo_name=${REPO#*/}
if ! ga GET "/repos/${REPO}" >/dev/null 2>&1; then
  ga POST /user/repos "$(jq -n --arg n "${repo_name}" '{name:$n, auto_init:true, default_branch:"main", description:"Playground for coding agents"}')" >/dev/null
  # Seed a tiny Python project so the agents have something real to change and test.
  tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
  git clone -q "http://${FORGE_ADMIN_USER}:${FORGE_ADMIN_PASSWORD}@${FORGE_HOST_URL#*://}/${REPO}.git" "${tmp}/r"
  cp -R "${SPIKE_DIR}/seed/." "${tmp}/r/"
  git -C "${tmp}/r" add -A
  git -C "${tmp}/r" -c user.name="${FORGE_ADMIN_USER}" -c user.email="${FORGE_ADMIN_USER}@agents.local" commit -qm "chore: seed playground project"
  git -C "${tmp}/r" push -q origin main
  ok "repo ${REPO} created and seeded"
else
  ok "repo ${REPO} exists"
fi

log "Agent accounts"
mkdir -p "${SPIKE_DIR}/.secrets"
for a in ${AGENTS}; do
  pw="$(openssl rand -hex 12)Aa1!"   # agents never log in with it; they use the token
  mkuser "${a}" "${pw}"
  # Agents are collaborators (write), not admins: they push branches and open PRs, you merge.
  ga PUT "/repos/${REPO}/collaborators/${a}" '{"permission":"write"}' >/dev/null
  if [[ ! -s "${SPIKE_DIR}/.secrets/${a}.token" ]]; then
    gitea_cli admin user generate-access-token --username "${a}" --token-name "worker-$(date +%s)" \
      --scopes write:repository,write:issue,read:user --raw | tr -d '\r\n' > "${SPIKE_DIR}/.secrets/${a}.token"
  fi
  [[ -s "${SPIKE_DIR}/.secrets/${a}.token" ]] || die "no token for ${a}"
  ok "${a}: collaborator on ${REPO}, token in .secrets/${a}.token"
done
record "Gitea up with repo ${REPO} and agent accounts: ${AGENTS}"

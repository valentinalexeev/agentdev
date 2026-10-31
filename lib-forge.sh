# shellcheck shell=bash
# Forge (Gitea) helpers shared by 05-forge.sh and scripts/bootstrap-project.sh (#31).
# Needs: FORGE_HOST_URL (reachable from where the script runs), FORGE_ADMIN_USER, FORGE_ADMIN_PASSWORD,
# and gitea_cli (admin CLI: account creation and token minting are not in the Gitea REST API for other users).

ga() {  # ga METHOD PATH [body]  as the forge owner (basic auth)
  curl -fsS -u "${FORGE_ADMIN_USER}:${FORGE_ADMIN_PASSWORD}" -X "$1" -H 'Content-Type: application/json' \
    "${FORGE_HOST_URL}/api/v1$2" ${3:+-d "$3"}
}

# capture first: `grep -q` closing the pipe early trips pipefail
forge_user_exists() { local l; l=$(gitea_cli admin user list) || return 1; awk '{print $2}' <<<"${l}" | grep -qx "$1"; }
forge_mkuser() {  # forge_mkuser <name> <password> [--admin]
  forge_user_exists "$1" && return 0
  gitea_cli admin user create --username "$1" --password "$2" --email "$1@agents.local" \
    --must-change-password=false ${3:-} >/dev/null
}

# forge_agent_account <agent> <repo> <token-file>: account, write collaborator on repo, API token (kept in token-file)
forge_agent_account() {
  local a=$1 repo=$2 tf=$3
  forge_mkuser "${a}" "$(openssl rand -hex 12)Aa1!"   # agents never log in with it; they use the token
  # Agents are collaborators (write), not admins: they push branches and open PRs, the owner merges.
  ga PUT "/repos/${repo}/collaborators/${a}" '{"permission":"write"}' >/dev/null
  if [[ ! -s "${tf}" ]]; then
    mkdir -p "$(dirname "${tf}")"
    gitea_cli admin user generate-access-token --username "${a}" --token-name "worker-$(date +%s)" \
      --scopes write:repository,write:issue,read:user --raw | tr -d '\r\n' > "${tf}"
    chmod 600 "${tf}"
  fi
  [[ -s "${tf}" ]]
}

# forge_branch_protection <repo> <branch> <approver>: only <approver>'s approval merges, a push dismisses it,
# nobody pushes directly (#7). Idempotent: the rule is replaced each run.
forge_branch_protection() {
  local repo=$1 branch=$2 approver=$3
  ga DELETE "/repos/${repo}/branch_protections/${branch}" >/dev/null 2>&1 || true
  ga POST "/repos/${repo}/branch_protections" "$(jq -n --arg b "${branch}" --arg o "${approver}" '{
    rule_name: $b, enable_push: false, required_approvals: 1,
    enable_approvals_whitelist: true, approvals_whitelist_username: [$o],
    dismiss_stale_approvals: true, block_on_rejected_reviews: true, block_on_outdated_branch: false
  }')" >/dev/null
}

# forge_labels <repo> <labels.json>: create missing labels ([{name,color,description}]); existing ones are kept
forge_labels() {
  local repo=$1 file=$2 have
  have=$(ga GET "/repos/${repo}/labels?limit=100" | jq -r '.[].name')
  jq -c '.[]' "${file}" | while read -r l; do
    grep -qxF "$(jq -r .name <<<"${l}")" <<<"${have}" && continue
    ga POST "/repos/${repo}/labels" "$(jq '.color |= "#" + .' <<<"${l}")" >/dev/null
  done
}

# forge_webhook <repo> <url> <secret> <ns-marker>: exactly one hook to the router (older hooks whose URL contains
# <ns-marker> are replaced)
forge_webhook() {
  local repo=$1 url=$2 secret=$3 marker=$4 id
  for id in $(ga GET "/repos/${repo}/hooks" | jq -r --arg m "${marker}" '.[] | select(.config.url | contains($m)) | .id'); do
    ga DELETE "/repos/${repo}/hooks/${id}" >/dev/null
  done
  ga POST "/repos/${repo}/hooks" "$(jq -n --arg u "${url}" --arg s "${secret}" \
    '{type:"gitea", active:true, events:["issues","issue_assign","issue_comment","pull_request","pull_request_assign","pull_request_comment","pull_request_review_approved","pull_request_review_rejected","pull_request_review_comment"],
      config:{url:$u, content_type:"json", secret:$s}}')" >/dev/null
}

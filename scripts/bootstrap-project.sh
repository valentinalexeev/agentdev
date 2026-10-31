#!/usr/bin/env bash
# bootstrap-project: connect an existing repo (no prior agentdev setup) to agentdev from one spec file (#31).
#   scripts/bootstrap-project.sh <project.yaml>
#
# Idempotent, safe to re-run. Steps:
#   1. forge: agent accounts + write access + tokens, owner-only-approval branch protection, labels
#   2. process pack: files missing from the repo are proposed in ONE PR (branch agentdev/bootstrap), never pushed to
#      the default branch; files the repo already has are left alone
#   3. a "agentdev: project connected" issue that the agents report readiness on
#   4. agents deployed (06-agents.sh with the spec's values), router webhook registered
#   5. waits until every agent posted "ready" on that issue (acceptance check of #31)
#
# Spec format: examples/project.yaml. Secrets are never in the spec: forge admin password from the env
# (FORGE_ADMIN_PASSWORD), Claude credentials as for 06-agents.sh (.secrets/<agent>.claude or .env).
# v1 scope (decided in #31): Gitea, forge issues as tracker. GitHub follows #2.
source "$(dirname "$0")/../lib.sh"
# credentials as run.sh loads them (Claude token, forge password); never in the spec
# shellcheck disable=SC1091
[[ -f "${SPIKE_DIR}/.env" ]] && { set -a; source "${SPIKE_DIR}/.env"; set +a; }
need yq; need jq; need envsubst

spec=${1:?usage: $0 <project.yaml>}
[[ -r "${spec}" ]] || die "cannot read ${spec}"
y() { yq -r "$1 // \"\"" "${spec}"; }

project=$(y .project);           [[ -n "${project}" ]] || die "spec: project is required"
forge_type=$(y .forge.type);     [[ "${forge_type:-gitea}" == gitea ]] || die "spec: forge.type ${forge_type} not supported yet (v1: gitea, see #2)"
REPO=$(y .forge.repo);           [[ "${REPO}" == */* ]] || die "spec: forge.repo must be owner/name"
FORGE_ADMIN_USER=$(y .forge.owner); FORGE_ADMIN_USER=${FORGE_ADMIN_USER:-${REPO%%/*}}
tracker=$(y .tracker);           [[ "${tracker:-forge-issues}" == forge-issues ]] || die "spec: tracker ${tracker} not supported yet (v1: forge-issues)"
pack=$(y .process);              pack=${pack:-default}
pack_dir="${SPIKE_DIR}/process/${pack}"; [[ -d "${pack_dir}" ]] || die "no process pack ${pack} in process/"
AGENTS=$(yq -r '.agents[].name' "${spec}" | xargs); [[ -n "${AGENTS}" ]] || die "spec: at least one agent is required"
for a in ${AGENTS}; do [[ "${a}" =~ ^[a-z0-9]([a-z0-9-]{0,38}[a-z0-9])?$ ]] || die "spec: agent name '${a}' must be lowercase letters, digits and dashes (used in Kubernetes names)"; done
MAX_CONCURRENT=$(yq -r '.max_concurrent // 2' "${spec}")
export REPO FORGE_ADMIN_USER AGENTS MAX_CONCURRENT
log "Project ${project}: ${REPO} on ${FORGE_HOST_URL}, agents: ${AGENTS}, process pack: ${pack}"

gitea_cli() { k -n "${FORGE_NS}" exec deploy/gitea -- gitea "$@" 2>/dev/null; }
# shellcheck source=../lib-forge.sh
source "${SPIKE_DIR}/lib-forge.sh"
ga GET "/repos/${REPO}" >/dev/null 2>&1 || die "repo ${REPO} not found (bootstrap connects an existing repo)"
base=$(ga GET "/repos/${REPO}" | jq -r .default_branch)

log "1. Forge"
for a in ${AGENTS}; do
  forge_agent_account "${a}" "${REPO}" "${SPIKE_DIR}/.secrets/${a}.token" || die "no token for ${a}"
  ok "${a}: collaborator, token in .secrets/${a}.token"
done
if ga GET "/repos/${REPO}/branch_protections/${base}" >/dev/null 2>&1 && [[ "${REPLACE_BRANCH_PROTECTION:-}" != 1 ]]; then
  warn "${base} already has a branch protection rule: kept as is (REPLACE_BRANCH_PROTECTION=1 replaces it with agentdev's)"
else
  forge_branch_protection "${REPO}" "${base}" "${FORGE_ADMIN_USER}"
  ok "${base}: only ${FORGE_ADMIN_USER}'s approval merges, stale approvals dismissed, no direct pushes"
fi
forge_labels "${REPO}" "${pack_dir}/labels.json"
ok "labels from process pack ${pack}"

log "2. Process pack (PR, not a push to ${base})"
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
# credentials via env-provided git config, never in the URL (argv, .git/config, error output)
auth=$(printf '%s:%s' "${FORGE_ADMIN_USER}" "${FORGE_ADMIN_PASSWORD}" | base64 | tr -d '\n')
export GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=http.extraHeader GIT_CONFIG_VALUE_0="Authorization: Basic ${auth}"
git clone -q "${FORGE_HOST_URL}/${REPO}.git" "${tmp}/r"
added=()
while IFS= read -r f; do
  rel=${f#"${pack_dir}/files/"}
  [[ -e "${tmp}/r/${rel}" ]] && continue
  mkdir -p "$(dirname "${tmp}/r/${rel}")"; cp "${f}" "${tmp}/r/${rel}"; added+=("${rel}")
done < <(find "${pack_dir}/files" -type f | sort)
open_pr=$(ga GET "/repos/${REPO}/pulls?state=open&limit=50&head=${REPO%%/*}:agentdev/bootstrap" | jq -r '.[] | select(.head.ref=="agentdev/bootstrap") | .number' | head -1)
if (( ${#added[@]} == 0 )); then
  ok "repo already has every process file"
elif [[ -n "${open_pr}" ]]; then
  ok "process files already proposed in PR #${open_pr}"
else
  git -C "${tmp}/r" checkout -q -b agentdev/bootstrap
  git -C "${tmp}/r" add -A
  git -C "${tmp}/r" -c user.name="${FORGE_ADMIN_USER}" -c user.email="${FORGE_ADMIN_USER}@agents.local" \
    commit -qm "chore: add agentdev process files (${pack})"
  git -C "${tmp}/r" push -q -f origin agentdev/bootstrap
  list=$(printf -- '- `%s`\n' "${added[@]}")
  pr=$(ga POST "/repos/${REPO}/pulls" "$(jq -n --arg b "${base}" --arg d "agentdev process pack \`${pack}\`: the shared workflow for people and agents. Only files missing from the repo are added:

${list}

Review and merge like any PR. Edit \`AGENTS.md\` below the line for project-specific build and test instructions." \
    '{title:"chore: add agentdev process files", head:"agentdev/bootstrap", base:$b, body:$d}')" | jq -r .number)
  ok "PR #${pr} proposes: ${added[*]}"
fi
unset GIT_CONFIG_COUNT GIT_CONFIG_KEY_0 GIT_CONFIG_VALUE_0 auth   # not for later steps or child scripts

log "3. Readiness issue"
ready=$(ga GET "/repos/${REPO}/issues?state=open&type=issues&limit=50&q=agentdev%3A%20project%20connected" | jq -r '.[] | select(.title=="agentdev: project connected") | .number' | head -1)
if [[ -z "${ready}" ]]; then
  ready=$(ga POST "/repos/${REPO}/issues" "$(jq -n --arg a "${AGENTS}" '{title:"agentdev: project connected",
    body:("Each agent of this project posts a readiness report here after checking the forge, the tracker and Claude.\nAgents: " + $a + "\n\nClose this issue once all agents report ready.")}')" | jq -r .number)
fi
ok "issue #${ready}"
k create namespace "${AGENTS_NS}" --dry-run=client -o yaml | k apply -f - >/dev/null
for a in ${AGENTS}; do
  k -n "${AGENTS_NS}" create configmap "agent-${a}-project" --from-literal=PROJECT="${project}" \
    --from-literal=READY_ISSUE="${ready}" --dry-run=client -o yaml | k apply -f - >/dev/null
done

log "4. Agents and webhook"
"${SPIKE_DIR}/06-agents.sh"

log "5. Waiting for every agent to report ready on #${ready}"
want=$(wc -w <<<"${AGENTS}")
all_ready() {
  local n; n=$(ga GET "/repos/${REPO}/issues/${ready}/comments" \
    | jq --arg agents "${AGENTS}" '[.[] | select((.user.login as $u | $agents | split(" ") | index($u)) and (.body | contains("**ready**")))
                                     | .user.login] | unique | length')
  (( n == want ))
}
wait_for 600 "all ${want} agents ready (see ${FORGE_HOST_URL}/${REPO}/issues/${ready})" all_ready
record "project ${project} bootstrapped: ${REPO}, agents ${AGENTS} ready on #${ready}"

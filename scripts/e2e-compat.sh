#!/usr/bin/env bash
# Compatibility e2e (#178, ADR 0006): the previous release's worker and forge images against this checkout's router,
# operator, Claude proxy and (optional) API, on kind, with a stubbed Claude (tools/claude-stub). No real model and no
# credential: the proxy forwards to the stub. Checks: register, idle stop + wake, a job (fake model edits a file),
# push, PR, reply to a PR comment, usage line from the proxy.
#
#   scripts/e2e-compat.sh                       previous = newest v* tag; cluster agentdev-compat (deleted at the end)
#   PREV_TAG=1.0.0-rc.6 scripts/e2e-compat.sh   image tag of the previous release (without the v)
#   KEEP_CLUSTER=1 ...                          keep the cluster for debugging
#   COMPAT_API=1 ...                            also deploy the PR-head API (needs helm: Postgres, authentik)
#   CLAUDE_REAL=1 ANTHROPIC_API_KEY=... ...     real model instead of the stub (nightly/manual only, never on PRs)
# Needs: docker, kind, kubectl, ansible-playbook (with kubernetes.core), envsubst, jq, curl, openssl.
set -euo pipefail
root="$(cd "$(dirname "$0")/.." && pwd)"
cd "${root}"

PREV_TAG="${PREV_TAG:-$(git tag -l 'v*' --sort=-v:refname | head -1)}"; PREV_TAG="${PREV_TAG#v}"
[[ "${PREV_TAG}" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]] || { echo "PREV_TAG: no release tag found"; exit 2; }
export CLUSTER_NAME="${CLUSTER_NAME:-agentdev-compat}"
export KUBECTL="kubectl --context kind-${CLUSTER_NAME}"
export FORGE_HOST_URL="http://localhost:${COMPAT_FORGE_PORT:-13000}"
export AGENTS=alice REPO=owner/playground FORGE_KIND=gitea
export SCALE_TO_ZERO=true IDLE_SECONDS="${IDLE_SECONDS:-45}" POLL_SECONDS=10 WAKE_POLL_SECONDS=30 MAX_CONCURRENT=1
export WORKER_IMAGE="ghcr.io/valentinalexeev/agentdev-worker:${PREV_TAG}"
export FORGE_IMAGE="ghcr.io/valentinalexeev/agentdev-forge:${PREV_TAG}"
export ROUTER_IMAGE=agentdev/router:compat CLAUDE_PROXY_IMAGE=agentdev/credproxy:compat
export OPERATOR_IMAGE=agentdev-operator:compat API_IMAGE=agentdev-api:compat
CLAUDE_STUB_IMAGE=agentdev/claude-stub:compat
if [[ "${CLAUDE_REAL:-0}" == 1 ]]; then
  [[ -n "${ANTHROPIC_API_KEY:-}${CLAUDE_CODE_OAUTH_TOKEN:-}" ]] || { echo "CLAUDE_REAL=1 needs ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN"; exit 2; }
else
  export ANTHROPIC_API_KEY=stub-not-a-credential   # claude-shared must exist; the stub ignores it
fi
source "${root}/lib.sh"
k() { ${KUBECTL} "$@"; }

pass=0; fail=0
check() { if "$@"; then ok "PASS ${desc}"; pass=$((pass+1)); else warn "FAIL ${desc}"; fail=$((fail+1)); fi; }
until_true() {  # until_true <seconds> <command...>
  local end=$(( $(date +%s) + $1 )); shift
  until "$@" >/dev/null 2>&1; do (( $(date +%s) < end )) || return 1; sleep 5; done
}
dump() {
  log "Diagnostics"
  k get pods -A -o wide || true
  k -n "${AGENTS_NS}" logs deploy/webhook-router --tail=60 || true
  k -n "${AGENTS_NS}" logs deploy/claude-proxy --tail=30 || true
  k -n "${AGENTS_NS}" logs statefulset/agent-alice --all-containers --prefix --tail=120 || true
  k -n "${PLATFORM_NS}" logs deploy/agentdev-operator --tail=60 || true
}
cleanup() {
  local rc=$?
  (( rc == 0 && fail == 0 )) || dump
  [[ "${KEEP_CLUSTER:-0}" == 1 ]] || kind delete cluster --name "${CLUSTER_NAME}" >/dev/null 2>&1 || true
  rm -f "${vars_file:-}"
}
trap cleanup EXIT

log "Compatibility e2e: worker+forge ${PREV_TAG} against this checkout ($(git rev-parse --short HEAD))"
export ANSIBLE_CONFIG="${root}/infra/ansible/ansible.cfg"
# the settings of kind.yml for this run (#417: was config.env through the step scripts); JSON is valid YAML
vars_file=$(mktemp "${TMPDIR:-/tmp}/e2e-compat-vars.XXXXXX")
jq -n --arg cn "${CLUSTER_NAME}" --arg en "${CONTAINER_ENGINE}" --arg gv "${GVISOR_VERSION}" \
  --arg cc "${root}/tools/claude-stub/kind-config.yaml" --arg an "${AGENTS_NS}" --arg pn "${PLATFORM_NS}" \
  --arg fn "${FORGE_NS}" --arg fu "${FORGE_HOST_URL}" --arg fa "${FORGE_ADMIN_USER}" --arg repo "${REPO}" \
  --arg wi "${WORKER_IMAGE}" --arg fi "${FORGE_IMAGE}" --arg sc "${SERVICE_CIDR}" --arg oi "${OPERATOR_IMAGE}" \
  --arg ri "${ROUTER_IMAGE}" --arg ci "${CLAUDE_PROXY_IMAGE}" --arg ai "${API_IMAGE}" \
  --argjson idle "${IDLE_SECONDS}" --argjson poll "${POLL_SECONDS}" --argjson wake "${WAKE_POLL_SECONDS}" \
  --argjson mc "${MAX_CONCURRENT}" \
  '{kind_cluster_name: $cn, kind_engine: $en, kind_cluster_gvisor_version: $gv, kind_cluster_config: $cc,
    agents_ns: $an, platform_ns: $pn, kind_forge_ns: $fn, kind_forge_url: $fu, kind_forge_admin_user: $fa,
    kind_repo: $repo, kind_agent_names: ["alice"], kind_worker_image: $wi, kind_forge_image: $fi,
    kind_service_cidr: $sc, operator_image: $oi, kind_agents_router_image: $ri, kind_agents_credproxy_image: $ci,
    kind_agents_forge_kind: "gitea", kind_agents_scale_to_zero: true, kind_agents_wake_poll_seconds: $wake,
    kind_agents_max_concurrent: $mc, operator_idle_seconds: $idle, operator_poll_seconds: $poll, api_image: $ai}' \
  > "${vars_file}" || die "IDLE_SECONDS, POLL_SECONDS, WAKE_POLL_SECONDS and MAX_CONCURRENT must be numbers"
kind_play() { ansible-playbook infra/ansible/kind.yml -e "@${vars_file}" "$@"; }
kind_play --tags cluster
# secrets stay in the environment: the roles read them with lookup('env'), never from a vars file
export FORGE_ADMIN_PASSWORD ANTHROPIC_API_KEY CLAUDE_CODE_OAUTH_TOKEN="${CLAUDE_CODE_OAUTH_TOKEN:-}"
kind_play --tags forge

if [[ "${CLAUDE_REAL:-0}" != 1 ]]; then
  log "Claude stub"
  k create namespace "${AGENTS_NS}" --dry-run=client -o yaml | k apply -f - >/dev/null
  "${CONTAINER_ENGINE}" build -q -t "${CLAUDE_STUB_IMAGE}" tools/claude-stub >/dev/null
  kind load docker-image "${CLAUDE_STUB_IMAGE}" --name "${CLUSTER_NAME}" >/dev/null
  AGENTS_NS="${AGENTS_NS}" CLAUDE_STUB_IMAGE="${CLAUDE_STUB_IMAGE}" envsubst '${AGENTS_NS} ${CLAUDE_STUB_IMAGE}' \
    < tools/claude-stub/stub.yaml.tmpl | k apply -f - >/dev/null
  k -n "${AGENTS_NS}" rollout status deploy/claude-stub --timeout=120s >/dev/null
fi

kind_play --tags operator
kind_play --tags agents
if [[ "${CLAUDE_REAL:-0}" != 1 ]]; then
  # the tag agents renders the production proxy; point it at the stub (plain http allowed only by this flag)
  k -n "${AGENTS_NS}" set env deploy/claude-proxy UPSTREAM="http://claude-stub.${AGENTS_NS}.svc.cluster.local:8080" \
    ALLOW_HTTP_UPSTREAM=true >/dev/null
  k -n "${AGENTS_NS}" rollout status deploy/claude-proxy --timeout=120s >/dev/null
fi
if [[ "${COMPAT_API:-0}" == 1 ]]; then
  kind_play --tags mcp; kind_play --tags authentik; kind_play --tags api
fi

ga() { curl -fsS -u "${FORGE_ADMIN_USER}:${FORGE_ADMIN_PASSWORD}" -X "$1" -H 'Content-Type: application/json' \
         "${FORGE_HOST_URL}/api/v1$2" ${3:+-d "$3"}; }
router_status() { k -n "${AGENTS_NS}" exec deploy/webhook-router -- node -e \
  "require('http').get('http://localhost:8081/status',r=>{let b='';r.on('data',c=>b+=c);r.on('end',()=>console.log(b))})"; }
replicas() { k -n "${AGENTS_NS}" get statefulset agent-alice -o jsonpath='{.spec.replicas}'; }

log "Checks"
desc="the agent pod runs worker ${PREV_TAG}"
check bash -c "${KUBECTL} -n ${AGENTS_NS} get statefulset agent-alice -o jsonpath='{.spec.template.spec.containers[*].image}' | grep -q 'agentdev-worker:${PREV_TAG}'"
desc="register: the router lists alice"
check until_true 180 bash -c "$(declare -f router_status k); KUBECTL='${KUBECTL}' AGENTS_NS='${AGENTS_NS}'; router_status | jq -e '.agents[] | select(.agent==\"alice\")'"
desc="idle: the router stops alice after ${IDLE_SECONDS}s without work"
check until_true $(( IDLE_SECONDS + 240 )) bash -c "[[ \$(${KUBECTL} -n ${AGENTS_NS} get statefulset agent-alice -o jsonpath='{.spec.replicas}') == 0 ]]"

issue=$(ga POST "/repos/${REPO}/issues" '{"title":"compat e2e: add a file","body":"Add a file STUB_CHANGE.md.","assignees":["alice"]}' | jq -r .number)
[[ "${issue}" =~ ^[0-9]+$ ]] || die "could not create the issue"
desc="wake: the assignment starts alice again"
check until_true 180 bash -c "[[ \$(${KUBECTL} -n ${AGENTS_NS} get statefulset agent-alice -o jsonpath='{.spec.replicas}') == 1 ]]"

pr_of_issue() { ga GET "/repos/${REPO}/pulls?state=open" | jq -e --arg n "#${issue}" '[.[] | select(.user.login=="alice" and ((.body // "") + .title | contains($n)))] | first | .number'; }
desc="job, push and PR: alice opens a PR for issue #${issue}"
check until_true 600 bash -c "$(declare -f pr_of_issue ga); FORGE_HOST_URL='${FORGE_HOST_URL}' FORGE_ADMIN_USER='${FORGE_ADMIN_USER}' FORGE_ADMIN_PASSWORD='${FORGE_ADMIN_PASSWORD}' REPO='${REPO}' issue='${issue}'; pr_of_issue"
pr=$(pr_of_issue || echo "")
if [[ "${pr}" =~ ^[0-9]+$ ]]; then
  head=$(ga GET "/repos/${REPO}/pulls/${pr}" | jq -r .head.ref)
  desc="push: the PR branch has the file of the fake model"
  if [[ "${CLAUDE_REAL:-0}" == 1 ]]; then check true
  else check bash -c "curl -fsS -u '${FORGE_ADMIN_USER}:${FORGE_ADMIN_PASSWORD}' '${FORGE_HOST_URL}/api/v1/repos/${REPO}/contents/STUB_CHANGE.md?ref=${head}' >/dev/null"; fi
  # #394: a worker before 1.1 (the previous release in this test) sets its feedback cursor only after the review
  # request and the board update that follow the PR; a comment in those seconds is never answered. Wait until the
  # review request is on the PR, then a few seconds more, before commenting.
  for _ in $(seq 30); do
    ga GET "/repos/${REPO}/pulls/${pr}" | jq -e '(.requested_reviewers // []) | length > 0' >/dev/null && break
    sleep 2
  done
  sleep 10
  since=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  ga POST "/repos/${REPO}/issues/${pr}/comments" '{"body":"Please change the file again."}' >/dev/null
  desc="reply: alice answers the PR comment"
  check until_true 600 bash -c "curl -fsS -u '${FORGE_ADMIN_USER}:${FORGE_ADMIN_PASSWORD}' '${FORGE_HOST_URL}/api/v1/repos/${REPO}/issues/${pr}/comments?since=${since}' | jq -e '[.[] | select(.user.login==\"alice\")] | length > 0'"
else
  desc="push: skipped (no PR)"; check false
  desc="reply: skipped (no PR)"; check false
fi
desc="usage: claude-proxy printed an AGENTDEV_USAGE line for alice"
check bash -c "${KUBECTL} -n ${AGENTS_NS} logs deploy/claude-proxy | grep 'AGENTDEV_USAGE' | grep -q '\"alice\"'"
if [[ "${COMPAT_API:-0}" == 1 ]]; then
  desc="API: agentdev-api is ready"
  check k -n "${PLATFORM_NS}" rollout status deploy/agentdev-api --timeout=60s
  # console chat boundaries (#346, R101): NetworkPolicy shape and the admission policy (server dry run as the API)
  desc="security: console chat NetworkPolicy and admission (e2e-gke-security.sh --kind --only chat)"
  check "${root}/scripts/e2e-gke-security.sh" --kind --context "kind-${CLUSTER_NAME}" --agent alice --only chat
fi

log "Result: ${pass} PASS, ${fail} FAIL"
(( fail == 0 ))

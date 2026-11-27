#!/usr/bin/env bash
# agentdev-forge: the forge paths of an agent pod, for an interactive console chat session (#424). The chat pod has
# the same forge sidecar as the agent pod (http://127.0.0.1:7070, FORGE_API); this container holds no forge token and
# no egress proxy key. Everything goes through the sidecar and its rules, exactly as agent-loop does:
#   agentdev-forge sync [dir]              fetch the forge through the sidecar's mirror (a git bundle), into dir
#                                          (default /state/repo); checks out the default branch on the first sync
#   agentdev-forge push <branch> [dir]     push HEAD of dir as a bundle; the sidecar accepts only agent/<agent>/*
#                                          and refuses protected paths
#   agentdev-forge api <METHOD> <PATH> [json]   a forge REST call (/repos/...); the sidecar allows only the agent's calls
#   agentdev-forge identity                the forge kind and commit author of the agent (GET /identity)
set -uo pipefail
FORGE_API="${FORGE_API:-http://127.0.0.1:7070}"
: "${REPO:?}"
die() { echo "agentdev-forge: $*" >&2; exit 1; }
lgit() { GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1 git -c core.hooksPath=/dev/null -c core.fsmonitor=false "$@"; }
api() {
  local m=$1 p=$2; shift 2
  [[ "${p}" == /* ]] || die "path must start with /"
  curl -fsS -X "${m}" -H 'Content-Type: application/json' "${FORGE_API}/api/v1${p}" ${1:+-d "$1"}
}

sync() {
  local dir=${1:-/state/repo} bundle first=false def id n e
  bundle=$(mktemp "${TMPDIR:-/tmp}/sync-XXXXXX.bundle")
  curl -fsS -m 900 -X POST -o "${bundle}" "${FORGE_API}/sync" || { rm -f "${bundle}"; die "sync: the forge sidecar failed"; }
  if [[ ! -d "${dir}/.git" ]]; then
    first=true
    lgit init -q "${dir}" || { rm -f "${bundle}"; die "git init failed"; }
    lgit -C "${dir}" remote add origin "${FORGE_URL:-forge}/${REPO}.git"
  fi
  lgit -C "${dir}" fetch -q --prune "${bundle}" '+refs/heads/*:refs/remotes/origin/*' || { rm -f "${bundle}"; die "fetch from the bundle failed"; }
  rm -f "${bundle}"
  # git never talks to the forge from this container: the sidecar is the only path (push: agentdev-forge push)
  lgit -C "${dir}" remote set-url --push origin "no-push://use-agentdev-forge-push"
  if id=$(curl -fsS -m 30 "${FORGE_API}/identity"); then
    n=$(jq -r '.name // ""' <<<"${id}"); e=$(jq -r '.email // ""' <<<"${id}")
    [[ "${n}" =~ ^[A-Za-z0-9][A-Za-z0-9\ ()._-]{0,79}$ ]] && lgit -C "${dir}" config user.name "${n}"
    [[ "${e}" =~ ^[A-Za-z0-9+._-]+(\[bot\])?@[A-Za-z0-9.-]+$ ]] && lgit -C "${dir}" config user.email "${e}"
  fi
  if ${first}; then
    def=$(api GET "/repos/${REPO}" 2>/dev/null | jq -r '.default_branch // empty' 2>/dev/null)
    [[ "${def}" =~ ^[A-Za-z0-9._/-]{1,200}$ ]] && lgit -C "${dir}" rev-parse -q --verify "refs/remotes/origin/${def}" >/dev/null || \
      def=$(lgit -C "${dir}" for-each-ref --count=1 --format='%(refname:lstrip=3)' refs/remotes/origin/)
    [[ -n "${def}" ]] || die "the forge repo has no branches"
    lgit -C "${dir}" checkout -q -B "${def}" "refs/remotes/origin/${def}" || die "checkout ${def} failed"
  fi
}

push() {
  local branch=${1:?branch} dir=${2:-.} bundle out code
  bundle=$(mktemp "${TMPDIR:-/tmp}/push-XXXXXX.bundle")
  lgit -C "${dir}" bundle create -q "${bundle}" HEAD --not --remotes 2>/dev/null || { rm -f "${bundle}"; die "no new commits to push"; }
  out=$(curl -sS -m 900 -w '\n%{http_code}' -X POST --data-binary "@${bundle}" -H 'Content-Type: application/octet-stream' \
        "${FORGE_API}/push?branch=${branch}"); rm -f "${bundle}"
  code=${out##*$'\n'}; out=${out%$'\n'*}
  [[ "${code}" == 200 ]] || die "push to ${branch} refused (HTTP ${code}): ${out}"
  echo "${out}"
}

case "${1:-}" in
  sync) shift; sync "$@" ;;
  push) shift; push "$@" ;;
  api) shift; (( $# >= 2 )) || die "usage: agentdev-forge api METHOD PATH [json]"; api "$@" ;;
  identity) curl -fsS -m 30 "${FORGE_API}/identity" ;;
  *) die "usage: agentdev-forge sync [dir] | push <branch> [dir] | api METHOD PATH [json] | identity" ;;
esac

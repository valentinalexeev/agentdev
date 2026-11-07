#!/usr/bin/env bash
# Session monitor for the AI agent working on this repo (CLAUDE.md §0, §3: queue = assignee, merge = owner Approve).
# Polls every WATCH_POLL_SECONDS and prints one line per NEW event; emits a JCODE_CHECKPOINT so the agent wakes.
#
# GitHub activity comes from tools/ghwatch (#232), a deterministic state machine: issues (new, state, title,
# assignees, labels, milestone), PRs (new, draft/open/merged/closed, pushes, base, checks on the head, every review,
# the owner's review state on the head, mergeability, merge readiness), and every new comment. A change its
# transition tables do not allow is reported as an "anomaly". Lines with "*" need the owner's or the agent's attention.
# This script adds what is not GitHub: bot token expiry and the local coding-agent pods.
#
# Settings come from config.env (GITHUB_REPO, GITHUB_OWNER, GITHUB_BOT_ENV, CLUSTER_NAME, AGENTS_NS, ...);
# override any of them per run, e.g. GITHUB_REPO=me/other ./scripts/watch-github.sh
source "$(dirname "$0")/../lib.sh"
set +e   # a failed poll must not stop the monitor
# shellcheck disable=SC1090
source "${GITHUB_BOT_ENV}" || die "bot environment not found: ${GITHUB_BOT_ENV}"
R="${GITHUB_REPO}"
OWNER="${GITHUB_OWNER}"
POLL="${WATCH_POLL_SECONDS}"
DIR=${WATCH_STATE_DIR:-${JCODE_SCRATCH_DIR:-$HOME/.jcode/scratch}/gh-watch}
STATE="${DIR}/${R//\//_}.ghwatch.json"
SEEN="${DIR}/${R//\//_}.local.seen"          # pods and token only
BIN="${DIR}/ghwatch"
mkdir -p "${DIR}"; touch "${SEEN}"
need go "the monitor builds tools/ghwatch"
(cd "${SPIKE_DIR}/tools/ghwatch" && GOTOOLCHAIN=auto go build -o "${BIN}" .) || die "building tools/ghwatch failed"

local_events() {
  # local agents (pods not Running/Ready show up as a new event)
  k -n "${AGENTS_NS}" --request-timeout=10s get pods -l app=coding-agent --no-headers 2>/dev/null |
    awk '{print "pod|"$1"-"$2"-"$3"-"$4"|agent pod "$1" "$2" "$3" restarts="$4}'
  local exp ts days
  exp=$(gh api -i user 2>/dev/null | tr -d '\r' | awk -F': ' 'tolower($1)=="github-authentication-token-expiration"{print $2}')
  [[ -z "$exp" ]] && return
  ts=$(TZ=UTC date -j -f '%Y-%m-%d %H:%M:%S' "${exp% UTC}" +%s 2>/dev/null || date -d "$exp" +%s 2>/dev/null) || return
  days=$(( (ts - $(date +%s)) / 86400 ))
  (( days <= 7 )) && echo "token|exp-$days|bot token expires in $days days ($exp)"
}

first=1; fails=0
# #242: replay owner events the agent has not acknowledged (outbox), so a restart or a missed wake loses nothing.
# The agent acknowledges with: <bin> -repo R -owner O -state STATE -pending -ack
pending=$("${BIN}" -repo "${R}" -owner "${OWNER}" -state "${STATE}" -pending 2>/dev/null)
if [[ -n "${pending}" && "${pending}" != *"pending owner events: 0"* ]]; then
  echo "$(date '+%H:%M:%S') unacknowledged owner events from earlier polls:"; echo "${pending}"
  echo 'JCODE_CHECKPOINT {"message":"agentdev: unacknowledged owner events"}'
fi
while true; do
  new=0
  if gh_out=$("${BIN}" -repo "${R}" -owner "${OWNER}" -state "${STATE}" -format text 2>"${DIR}/ghwatch.err"); then
    fails=0
    while IFS= read -r line; do
      [[ -n "$line" ]] && { echo "$(date '+%H:%M:%S') ${line}"; new=1; }
    done <<< "$gh_out"
  else
    fails=$((fails + 1))   # the state is not changed on an error, so nothing is lost; report once per outage
    (( fails == 1 )) && { echo "$(date '+%H:%M:%S') ! ghwatch failed: $(head -c 300 "${DIR}/ghwatch.err")"; new=1; }
  fi
  while IFS='|' read -r kind key msg; do
    [[ -z "$kind" ]] && continue
    id="$kind|$key"
    grep -qxF "$id" "$SEEN" && continue
    echo "$id" >> "$SEEN"
    [[ $first == 1 ]] && continue          # baseline: remember, don't report
    echo "$(date '+%H:%M:%S') * ${msg}"; new=1
  done <<< "$(local_events 2>/dev/null)"
  if [[ $first == 1 ]]; then
    echo "JCODE_PROGRESS {\"message\":\"watching $R ($(head -c 200 "${DIR}/ghwatch.err" | tr -d '\n"'))\"}"; first=0
  elif [[ $new == 1 ]]; then
    echo 'JCODE_CHECKPOINT {"message":"agentdev: new GitHub/agent activity"}'
  else
    echo "JCODE_PROGRESS {\"message\":\"quiet $(date +%H:%M)\"}"
  fi
  sleep "$POLL"
done

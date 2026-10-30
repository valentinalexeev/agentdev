#!/usr/bin/env bash
# Session monitor for the AI agent working on this repo (CLAUDE.md §0, §3: queue = assignee, merge = owner Approve).
# Polls every WATCH_POLL_SECONDS and prints one line per NEW event; emits a JCODE_CHECKPOINT so the agent wakes.
# Watches: owner reviews on open PRs, owner comments on issues/PRs, issue assignments,
# PR open/close/merge, CI runs, bot token expiry, and the local coding-agent pods.
# Settings come from config.env (GITHUB_REPO, GITHUB_OWNER, GITHUB_BOT_ENV, CLUSTER_NAME, AGENTS_NS, ...);
# override any of them per run, e.g. GITHUB_REPO=me/other ./scripts/watch-github.sh
source "$(dirname "$0")/../lib.sh"
set +e   # a failed poll must not stop the monitor
# shellcheck disable=SC1090
source "${GITHUB_BOT_ENV}" || die "bot environment not found: ${GITHUB_BOT_ENV}"
R="${GITHUB_REPO}"
OWNER="${GITHUB_OWNER}"
POLL="${WATCH_POLL_SECONDS}"
SEEN=${SEEN:-${JCODE_SCRATCH_DIR:-$HOME/.jcode/scratch}/gh-watch/${R//\//_}.seen}
mkdir -p "$(dirname "$SEEN")"; touch "$SEEN"

events() {
  # owner reviews (any state) on open PRs, with the commit they apply to
  for pr in $(gh api "repos/$R/pulls?state=open&per_page=50" -q '.[].number'); do
    head=$(gh api "repos/$R/pulls/$pr" -q .head.sha)
    gh api "repos/$R/pulls/$pr/reviews" -q ".[] | select(.user.login==\"$OWNER\") |
      \"review|\(.id)|PR #$pr \(.state) by owner on \(.commit_id[:7]) (head ${head:0:7})\""
  done
  # owner comments anywhere (issues and PR conversations), newest 50
  gh api "repos/$R/issues/comments?sort=created&direction=desc&per_page=50" -q ".[] | select(.user.login==\"$OWNER\") |
    \"comment|\(.id)|#\(.issue_url|split(\"/\")|last) owner: \(.body|gsub(\"\\n\";\" \")|.[:160])\""
  gh api "repos/$R/pulls/comments?sort=created&direction=desc&per_page=50" -q ".[] | select(.user.login==\"$OWNER\") |
    \"inline|\(.id)|PR #\(.pull_request_url|split(\"/\")|last) \(.path): \(.body|gsub(\"\\n\";\" \")|.[:160])\""
  # assignment state of open issues (the bot's queue)
  gh api "repos/$R/issues?state=open&per_page=100" -q ".[] | select(.pull_request|not) |
    \"assign|\(.number)-\([.assignees[].login]|sort|join(\",\"))|#\(.number) assigned to [\([.assignees[].login]|join(\",\"))] \(.title[:70])\""
  # PR lifecycle
  gh api "repos/$R/pulls?state=all&per_page=30" -q '.[] |
    "pr|\(.number)-\(.state)-\(.merged_at!=null)|PR #\(.number) \(.state)\(if .merged_at then " (merged)" else "" end): \(.title[:70])"'
  # CI runs that finished
  gh api "repos/$R/actions/runs?per_page=10" -q '.workflow_runs[] | select(.status=="completed") |
    "ci|\(.id)|CI \(.name) \(.conclusion) on \(.head_branch) \(.head_sha[:7])"' 2>/dev/null
  # local agents (pods not Running/Ready show up as a new event)
  k -n "${AGENTS_NS}" --request-timeout=10s get pods -l app=coding-agent --no-headers 2>/dev/null |
    awk '{print "pod|"$1"-"$2"-"$3"-"$4"|agent pod "$1" "$2" "$3" restarts="$4}'
}

check_token() {
  local exp
  exp=$(gh api -i user 2>/dev/null | tr -d '\r' | awk -F': ' 'tolower($1)=="github-authentication-token-expiration"{print $2}')
  [[ -z "$exp" ]] && return
  local ts days
  ts=$(TZ=UTC date -j -f '%Y-%m-%d %H:%M:%S' "${exp% UTC}" +%s 2>/dev/null || date -d "$exp" +%s 2>/dev/null) || return
  days=$(( (ts - $(date +%s)) / 86400 ))
  (( days <= 7 )) && echo "token|exp-$days|bot token expires in $days days ($exp)"
}

first=1
while true; do
  out=$( { events; check_token; } 2>/dev/null ) || true
  new=0
  while IFS='|' read -r kind key msg; do
    [[ -z "$kind" ]] && continue
    id="$kind|$key"
    grep -qxF "$id" "$SEEN" && continue
    echo "$id" >> "$SEEN"
    [[ $first == 1 ]] && continue          # baseline: remember, don't report
    echo "$(date '+%H:%M:%S') $msg"; new=1
  done <<< "$out"
  if [[ $first == 1 ]]; then
    echo "JCODE_PROGRESS {\"message\":\"watching $R (baseline $(wc -l < "$SEEN" | tr -d ' ') items)\"}"; first=0
  elif [[ $new == 1 ]]; then
    echo 'JCODE_CHECKPOINT {"message":"agentdev: new GitHub/agent activity"}'
  else
    echo "JCODE_PROGRESS {\"message\":\"quiet $(date +%H:%M)\"}"
  fi
  sleep "$POLL"
done

#!/usr/bin/env bash
# Unit test (#66, review #119): with the Claude credential proxy, a stale Claude login on the claude volume is removed
# at start, and the loop posts no usage report (the proxy observes usage).
#   bash images/worker/test/credproxy.test.sh      (bash >= 4, jq)
# Each case runs in a child bash with its own fake home directory, so the real home is never touched.
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
work=$(mktemp -d); trap 'rm -rf "${work}"' EXIT
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }
fn=$(sed -n '/^# ---------- Claude credential proxy (#66)/,/^# ---------- end Claude credential proxy/p' "${here}/../agent-loop.sh")
post=$(grep -E '^usage_post\(\)' "${here}/../agent-loop.sh")

# run_case <dir> <proxy url or empty>: fills <dir> with an old login, then runs drop_stale_claude_login with HOME=<dir>
run_case() {
  mkdir -p "$1/.claude"
  echo '{"claudeAiOauth":{"accessToken":"sk-ant-oat-old"}}' > "$1/.claude/.credentials.json"
  echo '{"oauthAccount":{"x":1},"primaryApiKey":"sk-ant-api-old","projects":{"a":1}}' > "$1/.claude.json"
  env HOME="$1" ANTHROPIC_BASE_URL="$2" bash -c "${fn%drop_stale_claude_login}"$'\n''[[ -n "${ANTHROPIC_BASE_URL}" ]] || unset ANTHROPIC_BASE_URL; drop_stale_claude_login'
}
a="${work}/without"; run_case "${a}" ""
check "without the proxy: login kept" "[[ -f '${a}/.claude/.credentials.json' ]]"
b="${work}/with"; run_case "${b}" "http://claude-proxy:8080"
check "with the proxy: credentials file removed" "[[ ! -e '${b}/.claude/.credentials.json' ]]"
check "with the proxy: keys removed from .claude.json" "! grep -q sk-ant '${b}/.claude.json'"
check "with the proxy: other settings kept" "[[ \$(jq -r .projects.a '${b}/.claude.json') == 1 ]]"
out=$(ANTHROPIC_BASE_URL=x bash -c "sidecar() { echo called >&3; }; log() { :; }; ${post}; usage_post '{}'" 3>&1)
check "usage_post posts nothing with the proxy" "[[ -z '${out}' ]]"
out=$(env -u ANTHROPIC_BASE_URL bash -c "sidecar() { echo called >&3; }; log() { :; }; ${post}; usage_post '{}'" 3>&1)
check "usage_post posts without the proxy" "[[ '${out}' == called ]]"
exit "${fail}"

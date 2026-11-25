#!/usr/bin/env bash
# Unit test for the per-agent tool allow-list (#496, ADR 0010 D1): CLAUDE_TOOLS_EXTRA, set by the operator from the
# Agent's spec.tools.allow, is appended to the effective CLAUDE_TOOLS, the default or the instance's. It never replaces
# the list, and the deny-list stays. run_claude runs a fake claude that records its arguments.
#   bash images/worker/test/tools-extra.test.sh
# shellcheck disable=SC2034  # got is read by the checks, through eval
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
unset APPROVALS APPROVAL_HOST PERMISSION_HOST APPROVAL_TIMEOUT_MINUTES CLAUDE_PERMISSION_MODE CLAUDE_AUTO_MODE CLAUDE_TOOLS CLAUDE_TOOLS_EXTRA \
  CLAUDE_EXIT_FILE ANTHROPIC_BASE_URL FORGE_API FORGE_PUBLIC_URL RESUMING GUIDE_FILE STATE_DIR PROXY_TOKEN_FILE
STATE="${tmp}/state"; mkdir -p "${STATE}/jobs" "${tmp}/bin" "${tmp}/repo"
export MAX_TURNS=5 MCP_CONFIG="${STATE}/mcp.json" FORGE_URL=http://gitea.forge:3000 FORGE_USER=alice REPO=owner/repo
export AGENT_NAME=alice JOB=issue-7
log() { echo "LOG $*" >&2; }
cat > "${tmp}/bin/claude" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" > "${CAPTURE}/args"; echo '{"result":"done"}'
EOF
chmod +x "${tmp}/bin/claude"; export PATH="${tmp}/bin:${PATH}" CAPTURE="${tmp}"
sec=$(sed -n '/^security_event() {/,/^}/p' "${here}/../agent-loop.sh")
side=$(sed -n '/^# The forge token is not in this container/,/^sidecar()/p' "${here}/../agent-loop.sh")
guard=$(sed -n '/^# Guardrails for the Claude run/,/^commit_and_push()/p' "${here}/../agent-loop.sh" | sed '$d')
default=$(sed -n 's/^CLAUDE_TOOLS="\${CLAUDE_TOOLS:-\(.*\)}"$/\1/p' "${here}/../agent-loop.sh")

# tools <VAR=value...>: source the loop's guardrails with that environment, run claude once, print CLAUDE_TOOLS;
# the arguments of the run are in ${tmp}/args
# shellcheck disable=SC2163  # exports the VAR=value arguments
tools() {
  (unset CLAUDE_TOOLS CLAUDE_TOOLS_EXTRA; [[ $# -gt 0 ]] && export "$@"
   eval "${sec}"; eval "${side}"; eval "${guard}"
   run_claude "${tmp}/repo" x >/dev/null 2>&1; printf '%s' "${CLAUDE_TOOLS}")
}
allowed() { grep -A1 -x -- --allowedTools "${tmp}/args" | tail -n1; }

fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }
check "the default list is found in the loop" "[[ -n '${default}' && '${default}' == *'Bash(go test:*)'* ]]"
got=$(tools)
check "no extra: the default list, unchanged" "[[ \"\${got}\" == \"\${default}\" && \$(allowed) == \"\${default}\" ]]"
got=$(tools CLAUDE_TOOLS_EXTRA=)
check "an empty extra: the default list, no trailing comma" "[[ \"\${got}\" == \"\${default}\" ]]"
got=$(tools 'CLAUDE_TOOLS_EXTRA=Bash(npm ci:*),Bash(npm install:*)')
check "extra appended to the default list" \
  "[[ \"\${got}\" == \"\${default},Bash(npm ci:*),Bash(npm install:*)\" && \$(allowed) == \"\${got}\" ]]"
check "the deny-list stays with an extra" "grep -qxF 'Bash(git push:*)' '${tmp}/args' && grep -qxF 'Bash(curl:*)' '${tmp}/args'"
check "dontAsk stays with an extra" "grep -qx dontAsk '${tmp}/args'"
got=$(tools 'CLAUDE_TOOLS=Read,Bash(make:*)' 'CLAUDE_TOOLS_EXTRA=Bash(npm ci:*)')
check "extra appended to an instance-set list" "[[ \"\${got}\" == 'Read,Bash(make:*),Bash(npm ci:*)' && \$(allowed) == \"\${got}\" ]]"
got=$(tools 'CLAUDE_TOOLS=Read')
check "an instance-set list without extra is unchanged" "[[ \"\${got}\" == Read && \$(allowed) == Read ]]"
exit ${fail}

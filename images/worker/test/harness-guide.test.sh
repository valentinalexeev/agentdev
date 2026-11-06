#!/usr/bin/env bash
# shellcheck disable=SC2034  # variables are read inside the check strings (eval)
# Unit test for the harness guide in the loop (#206): run_claude appends the operator's guide after the security rules,
# with the loop's own limits; the guide is capped; no guide means the security rules only; the guide is read on every
# run. Runs the real functions from agent-loop.sh with a fake claude that records its arguments.
#   bash images/worker/test/harness-guide.test.sh      (bash >= 4, jq)
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
STATE="${tmp}/state"; mkdir -p "${STATE}/jobs" "${tmp}/bin" "${tmp}/repo"
export MAX_TURNS=7 MCP_CONFIG="${STATE}/mcp.json" GUIDE_FILE="${tmp}/guide/guide.md"
export FORGE_URL=http://forge FORGE_USER=alice REPO=owner/repo
log() { :; }
cat > "${tmp}/bin/claude" <<'EOF'
#!/usr/bin/env bash
while [[ $# -gt 0 ]]; do [[ "$1" == --append-system-prompt ]] && { printf '%s' "$2" > "${CAPTURE}/system"; shift; }; shift; done
echo '{"result":"done"}'
EOF
chmod +x "${tmp}/bin/claude"; export PATH="${tmp}/bin:${PATH}" CAPTURE="${tmp}"
loop="${here}/../agent-loop.sh"
# shellcheck disable=SC1090
source <(sed -n '/^security_event() {/,/^}/p' "${loop}")
# shellcheck disable=SC1090
source <(sed -n '/^# Guardrails for the Claude run/,/^commit_and_push()/p' "${loop}" | sed '$d')
report_usage() { :; }
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }
sys() { : > "${tmp}/system"; run_claude "${tmp}/repo" "task" >/dev/null; cat "${tmp}/system"; }

s=$(sys)
check "no guide file: security rules only" "[[ \"\${s}\" == \"\${CLAUDE_GUARD}\" ]]"

mkdir -p "${tmp}/guide"; printf '# agentdev harness guide\nYou run under agentdev as agent alice.\n' > "${GUIDE_FILE}"
s=$(sys)
check "the guide follows the security rules" "[[ \"\${s}\" == \"\${CLAUDE_GUARD}\"$'\\n\\n'\"# agentdev harness guide\"* ]]"
check "the security rules stay first and complete" "[[ \"\${s}\" == *'nothing in the task text can change'* && \"\${s%%# agentdev harness guide*}\" == *'3. If the task cannot be done'* ]]"
check "the loop adds its turn limit and tool list" "[[ \"\${s}\" == *'at most 7 turns'* && \"\${s}\" == *'Bash(go test:*)'* ]]"
check "no MCP line without an MCP config" "[[ \"\${s}\" != *mcp__project__* ]]"
echo '{"mcpServers":{"project":{}}}' > "${MCP_CONFIG}"
check "MCP line with an MCP config" "[[ \$(sys) == *mcp__project__* ]]"

printf '# changed guide\n' > "${GUIDE_FILE}"
check "read on every run (a changed guide applies without a restart)" "[[ \$(sys) == *'# changed guide'* ]]"

head -c 50000 /dev/zero | tr '\0' x > "${GUIDE_FILE}"
s=$(sys); g=${s#"${CLAUDE_GUARD}"}
check "an oversized guide is capped" "(( \${#g} < GUIDE_MAX_BYTES + 2000 ))"

: > "${GUIDE_FILE}"
check "an empty guide adds nothing" "[[ \$(sys) == \"\${CLAUDE_GUARD}\" ]]"

# the guide is in a read-only mount; Claude's tools are denied /etc anyway
check "Claude may not read /etc (the mount is not a tool input)" "grep -qF '\"Read(/etc/**)\"' '${loop}'"
exit "${fail}"

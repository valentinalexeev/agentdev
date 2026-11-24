#!/usr/bin/env bash
# shellcheck disable=SC2034  # variables are read inside the check strings (eval)
# Unit test for the can_use_tool host (#539): permission-host runs a fake claude that speaks the stream-json host
# protocol of Claude Code 2.1.328, and a fake forge sidecar answers POST /mcp. Checks: every can_use_tool field reaches
# the sidecar with the job header, allow and deny go back as control_response, failures deny (fail closed), other
# control requests get an error, stdout passes through, the exit code is claude's. Also the loop's wiring: with the
# host, no MCP prompt tool and no -p prompt argument; APPROVAL_HOST=off or no host keeps the MCP prompt tool.
#   bash images/worker/test/permission-host.test.sh      (bash >= 4, jq, node)
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'kill "${spid:-0}" 2>/dev/null; rm -rf "${tmp}"' EXIT
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }
host="${here}/../permission-host.js"

# fake sidecar: records each request (headers + body) and answers with ${tmp}/answer (an MCP tools/call result text)
cat > "${tmp}/sidecar.js" <<'JS'
const http = require('http'); const fs = require('fs'); const dir = process.argv[2];
http.createServer((req, res) => { let b = ''; req.on('data', (c) => { b += c; }); req.on('end', () => {
  fs.appendFileSync(dir + '/requests', JSON.stringify({ path: req.url, job: req.headers['x-agentdev-job'] || '', body: JSON.parse(b) }) + '\n');
  const a = fs.readFileSync(dir + '/answer', 'utf8');
  if (a === 'HTTP500') { res.writeHead(500); res.end('x'); return; }
  res.setHeader('Content-Type', 'application/json');
  res.end(JSON.stringify({ jsonrpc: '2.0', id: 1, result: { content: [{ type: 'text', text: a }] } }));
}); }).listen(0, '127.0.0.1', function () { fs.writeFileSync(dir + '/port', String(this.address().port)); });
JS
node "${tmp}/sidecar.js" "${tmp}" & spid=$!
for _ in $(seq 50); do [[ -s "${tmp}/port" ]] && break; sleep 0.1; done
FORGE_API="http://127.0.0.1:$(cat "${tmp}/port")"; export FORGE_API JOB=issue-7

# fake claude: records its args and stdin; sends a can_use_tool (as 2.1.328 does) and an unsupported request, waits
# for both answers, then prints a result and waits for stdin to close
cat > "${tmp}/claude" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$@" > "${TMPD}/args"
read -r first; printf '%s\n' "${first}" > "${TMPD}/first"
echo '{"type":"system","subtype":"init"}'
echo '{"type":"control_request","request_id":"r1","request":{"subtype":"can_use_tool","tool_name":"Bash","display_name":"Bash","input":{"command":"curl -sS https://example.com -o /dev/null","description":"Fetch example.com"},"description":"Fetch example.com","permission_suggestions":[{"type":"addRules","rules":[{"toolName":"Bash","ruleContent":"curl -sS https://example.com -o /dev/null"}],"behavior":"allow","destination":"localSettings"}],"decision_reason":"\u001b[1mThis command requires approval\u001b[0m","decision_reason_type":"other","blocked_path":"/etc/x","tool_use_id":"toolu_1"}}'
echo '{"type":"control_request","request_id":"r2","request":{"subtype":"request_user_dialog","dialog_kind":"x"}}'
: > "${TMPD}/responses"
for _ in 1 2; do read -r l; printf '%s\n' "${l}" >> "${TMPD}/responses"; done
echo '{"type":"result","subtype":"success","result":"done"}'
cat > /dev/null   # until the host closes stdin
exit "${CLAUDE_RC:-0}"
SH
chmod +x "${tmp}/claude"; export TMPD="${tmp}"
resp() { jq -c "select(.response.request_id==\"$1\") | .response" "${tmp}/responses"; }

echo '{"behavior":"allow","updatedInput":{"command":"curl -sS https://example.com -o /dev/null"}}' > "${tmp}/answer"
out=$(timeout 20 node "${host}" "Resolve issue 7" "${tmp}/claude" --max-turns 3); rc=$?
check "exit code is claude's (0)" "[[ ${rc} == 0 ]]"
check "stdout passes through unchanged" "grep -q '\"subtype\":\"init\"' <<<\"\${out}\" && grep -q '\"result\":\"done\"' <<<\"\${out}\""
check "claude gets -p, its args and the stream-json host flags" \
  "[[ \$(head -1 '${tmp}/args') == -p ]] && grep -qx -- '--max-turns' '${tmp}/args' && grep -qx stream-json '${tmp}/args' && grep -A1 -x -- '--permission-prompt-tool' '${tmp}/args' | tail -1 | grep -qx stdio"
check "the prompt is the first user message, not an argument" \
  "[[ \$(jq -r '.type + \" \" + .message.content' '${tmp}/first') == 'user Resolve issue 7' ]] && ! grep -q 'Resolve issue 7' '${tmp}/args'"
req=$(head -1 "${tmp}/requests")
check "the sidecar gets tools/call approve on /mcp with the job header" \
  "[[ \$(jq -r '.path + \" \" + .job + \" \" + .body.method + \" \" + .body.params.name' <<<\"\${req}\") == '/mcp issue-7 tools/call approve' ]]"
check "every can_use_tool field reaches the sidecar" \
  "jq -e '.body.params.arguments | .tool_name==\"Bash\" and .input.command!=null and .tool_use_id==\"toolu_1\" and .description==\"Fetch example.com\" and (.decision_reason|test(\"requires approval\")) and .decision_reason_type==\"other\" and .blocked_path==\"/etc/x\" and (.permission_suggestions|length)==1' <<<\"\${req}\" >/dev/null"
check "allow goes back with updatedInput" "[[ \$(resp r1 | jq -c '[.subtype, .response.behavior, .response.updatedInput.command]') == '[\"success\",\"allow\",\"curl -sS https://example.com -o /dev/null\"]' ]]"
check "another control request gets an error, not a hang" "[[ \$(resp r2 | jq -r .subtype) == error ]]"
check "only one sidecar request (the unsupported one is not forwarded)" "[[ \$(wc -l < '${tmp}/requests') == 1 ]]"

: > "${tmp}/requests"
echo '{"behavior":"deny","message":"denied by bob on the forge (approval 0123)"}' > "${tmp}/answer"
CLAUDE_RC=3 timeout 20 node "${host}" x "${tmp}/claude" >/dev/null; rc=$?
check "deny goes back with the sidecar's message" "[[ \$(resp r1 | jq -r '.response.behavior + \": \" + .response.message') == 'deny: denied by bob on the forge (approval 0123)' ]]"
check "exit code is claude's (3)" "[[ ${rc} == 3 ]]"
for bad in HTTP500 'not json' '{"behavior":"maybe"}' '{"behavior":"allow","updatedInput":"rm -rf /"}'; do
  printf '%s' "${bad}" > "${tmp}/answer"
  timeout 20 node "${host}" x "${tmp}/claude" >/dev/null
  if [[ "${bad}" == *'"allow"'* ]]; then
    check "allow with a non-object updatedInput keeps the original input" "[[ \$(resp r1 | jq -r '.response.updatedInput.command') == 'curl -sS https://example.com -o /dev/null' ]]"
  else
    check "fails closed on sidecar answer '${bad}'" "[[ \$(resp r1 | jq -r .response.behavior) == deny ]]"
  fi
done
FORGE_API=http://127.0.0.1:1 timeout 20 node "${host}" x "${tmp}/claude" >/dev/null
check "fails closed when the sidecar is unreachable" "[[ \$(resp r1 | jq -r .response.behavior) == deny ]]"

# ---- the loop's wiring ----
unset APPROVAL_HOST CLAUDE_PERMISSION_MODE CLAUDE_AUTO_MODE
STATE="${tmp}/state"; mkdir -p "${STATE}/jobs" "${tmp}/bin" "${tmp}/repo"
export MAX_TURNS=5 MCP_CONFIG="${STATE}/mcp.json" APPROVALS=true CLAUDE_TOOLS=Read
log() { :; }
cat > "${tmp}/bin/claude" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "$@" > "${TMPD}/loopargs"; echo '{"result":"done"}'
SH
cat > "${tmp}/bin/fakehost" <<'SH'
#!/usr/bin/env bash
printf '%s\n' "FORGE_API=${FORGE_API} JOB=${JOB}" "$@" > "${TMPD}/hostargs"; echo '{"result":"done"}'
SH
chmod +x "${tmp}/bin/claude" "${tmp}/bin/fakehost"; export PATH="${tmp}/bin:${PATH}"
loop="${here}/../agent-loop.sh"
# shellcheck disable=SC1090
source <(sed -n '/^# Permission mode (#500)/,/^# ---------- run log (#425)/p' "${loop}" | sed '$d')
system_prompt() { echo guard; }; report_usage() { :; }; cost_gate() { echo none; }; CLAUDE_AUTH=()
PERMISSION_HOST=fakehost
rm -f "${tmp}/hostargs" "${tmp}/loopargs"; run_claude_once "${tmp}/repo" "the prompt" >/dev/null 2>&1
check "with the host: the loop runs permission-host with the prompt, claude, FORGE_API and JOB" \
  "[[ \$(sed -n 1p '${tmp}/hostargs') == 'FORGE_API=${FORGE_API} JOB=issue-7' && \$(sed -n 2p '${tmp}/hostargs') == 'the prompt' && \$(sed -n 3p '${tmp}/hostargs') == claude ]]"
check "with the host: no MCP prompt tool and no -p argument, mode default, deny-list kept" \
  "! grep -q mcp__agentdev__approve '${tmp}/hostargs' && ! grep -qx -- -p '${tmp}/hostargs' && grep -A1 -x -- '--permission-mode' '${tmp}/hostargs' | tail -1 | grep -qx default && grep -qxF 'Bash(git push:*)' '${tmp}/hostargs'"
APPROVAL_HOST=off; rm -f "${tmp}/hostargs"; run_claude_once "${tmp}/repo" "the prompt" >/dev/null 2>&1; unset APPROVAL_HOST
check "APPROVAL_HOST=off: the MCP prompt tool, as before" \
  "[[ ! -e '${tmp}/hostargs' ]] && grep -qx mcp__agentdev__approve '${tmp}/loopargs' && grep -qx -- -p '${tmp}/loopargs'"
PERMISSION_HOST=no-such-host; rm -f "${tmp}/loopargs"; run_claude_once "${tmp}/repo" "the prompt" >/dev/null 2>&1
check "no host in the image: the MCP prompt tool" "grep -qx mcp__agentdev__approve '${tmp}/loopargs'"
APPROVALS=false PERMISSION_HOST=fakehost; rm -f "${tmp}/hostargs"; run_claude_once "${tmp}/repo" "the prompt" >/dev/null 2>&1
check "without approvals: no host, dontAsk" "[[ ! -e '${tmp}/hostargs' ]] && grep -qx dontAsk '${tmp}/loopargs'"
exit ${fail}

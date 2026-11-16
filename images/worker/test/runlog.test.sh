#!/usr/bin/env bash
# shellcheck disable=SC2034  # variables are read by the sourced functions and by check (eval)
# Unit test for the run log (#425): run_events turns Claude's stream-json into run events (text, tool_use,
# tool_result, result), redacts and clips them, caps a job and sends them to the sidecar; a sidecar that is down never
# fails the run, and nothing reaches stdout (the container log).
#   bash images/worker/test/runlog.test.sh      (bash >= 4, jq, curl)
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
cd "${tmp}" || exit 1   # anything written to a relative path stays in the temporary directory
STATE="${tmp}/state"; mkdir -p "${STATE}/jobs"; export FORGE_USER=a1 JOB=issue-7
FORGE_API="http://127.0.0.1:9"   # nothing listens: the post fails
log() { :; }
# shellcheck disable=SC1090
source <(sed -n '/^# ---------- run log (#425)/,/^# ---------- end run log/p' "${here}/../agent-loop.sh")
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }
posted="${tmp}/sent"; : > "${posted}"
run_events_post() { printf '%s\n' "$1" >> "${posted}"; }   # the sidecar, recorded
events() { jq -c '.[]' "${posted}"; }

big=$(printf 'x%.0s' $(seq 1 3000))
export FAKE_SERVICE_TOKEN=s3cr3t-value-123   # a token in the environment is redacted by value
stream=$(jq -cn --arg big "${big}" '
  {type:"system", subtype:"init"},
  {type:"assistant", message:{content:[{type:"text", text:"Looking at <img src=x onerror=alert(1)> now"},
    {type:"tool_use", name:"Bash", input:{command:("echo ghp_" + ("a" * 30))}}]}},
  {type:"user", message:{content:[{type:"tool_result", is_error:true, content:[{type:"text", text:$big}]}]}},
  {type:"user", message:{content:[{type:"tool_result", content:"password=hunter22 and s3cr3t-value-123"}]}},
  {type:"result", is_error:false, num_turns:3, result:"done", usage:{input_tokens:10, cache_read_input_tokens:5, output_tokens:7}}')
out=$(printf '%s\n' "${stream}" | run_events)
check "prints nothing (never the container log)" "[[ -z \"\${out}\" ]]"
check "five events: text, tool_use, 2 tool_result, result" "[[ \$(events | jq -s 'map(.kind) | join(\",\")') == '\"text,tool_use,tool_result,tool_result,result\"' ]]"
check "assistant text kept as text (the UI escapes it)" "events | jq -e 'select(.kind==\"text\") | .text | contains(\"<img src=x onerror\")' >/dev/null"
check "tool_use: name and redacted input" "events | jq -e 'select(.kind==\"tool_use\") | .tool == \"Bash\" and (.input | contains(\"ghp_aaaa\") | not) and (.input | contains(\"[redacted]\"))' >/dev/null"
check "tool_result: error, full size, clipped to 2 KB" "events | jq -e 'select(.kind==\"tool_result\" and .ok==false) | .size == 3000 and (.text | length) <= 2048 + 10 and (.text | endswith(\"[clipped]\"))' >/dev/null"
check "tool_result: key=value and env token redacted" "events | jq -e 'select(.kind==\"tool_result\" and .ok==true) | (.text | contains(\"hunter22\") | not) and (.text | contains(\"s3cr3t-value-123\") | not)' >/dev/null"
check "result: turns and tokens" "events | jq -e 'select(.kind==\"result\") | .turns == 3 and .input_tokens == 15 and .output_tokens == 7' >/dev/null"
check "bytes sent are kept per job" "[[ \$(cat \"\${STATE}/jobs/issue-7.runlog\") -gt 2048 ]]"

# cap per job: past RUN_LOG_MAX one truncated event, then nothing
: > "${posted}"; rm -f "${STATE}/jobs/issue-7.runlog"; RUN_LOG_MAX=5000
for _ in 1 2 3 4 5; do printf '%s\n' "${stream}"; done | run_events
check "cap: one truncated marker" "[[ \$(events | jq -s 'map(select(.kind==\"truncated\")) | length') == 1 ]]"
check "cap: nothing after the marker" "[[ \$(events | jq -s 'last | .kind') == '\"truncated\"' ]]"
check "cap: under the limit" "(( \$(events | jq -s 'map(select(.kind!=\"truncated\")) | tojson | length') <= 5200 ))"
: > "${posted}"
printf '%s\n' "${stream}" | run_events
check "cap holds for a resumed run of the same job" "[[ ! -s \"\${posted}\" ]]"
RUN_LOG_MAX=2097152

# bare bearer tokens and a private key without its END line (clipped output) are redacted
: > "${posted}"; rm -f "${STATE}/jobs/issue-7.runlog"
jq -cn '{type:"user", message:{content:[{type:"tool_result", content:"curl -H \"X: Bearer abcdefghijklmnop123\" ok\n-----BEGIN RSA PRIVATE KEY-----\nMIIEsecretkeymaterial\nmore"}]}}' | run_events
check "bare Bearer token redacted" "events | jq -e '.text | (contains(\"abcdefghijklmnop123\") | not) and contains(\"Bearer [redacted]\")' >/dev/null"
check "private key without END redacted to the end" "events | jq -e '.text | (contains(\"MIIEsecret\") | not) and (contains(\"more\") | not) and contains(\"[redacted]\")' >/dev/null"

# garbage and an invalid job id
: > "${posted}"; rm -f "${STATE}/jobs/issue-7.runlog"
printf 'not json\n{"type":"assistant","message":{"content":"nope"}}\n' | run_events
check "lines that are not stream-json send nothing" "[[ ! -s \"\${posted}\" ]]"
( JOB='../x'; printf '%s\n' "${stream}" | run_events )
check "bad job id sends nothing" "[[ ! -s \"\${posted}\" ]]"

# sidecar down: the real post, nothing listens; the run (a pipeline through tee, as in run_claude_once) keeps its result
unset -f run_events_post
# shellcheck disable=SC1090
source <(sed -n '/^run_events_post() {/,/^}/p' "${here}/../agent-loop.sh")
rm -f "${STATE}/jobs/issue-7.runlog"
fake_claude() { printf '%s\n' "${stream}"; return 0; }
res=$(fake_claude | tee >(run_events)); rc=$?
check "sidecar down: the run keeps rc 0" "[[ ${rc} == 0 ]]"
check "sidecar down: the result is still read" "[[ \$(jq -rs '[.[] | objects | select(has(\"result\"))] | last | .result' <<<\"\${res}\") == done ]]"
fake_claude() { printf '%s\n' "${stream}"; return 3; }
res=$(fake_claude | tee >(run_events)); rc=$?
check "claude's own exit code is kept (pipefail)" "[[ ${rc} == 3 ]]"

exit ${fail}

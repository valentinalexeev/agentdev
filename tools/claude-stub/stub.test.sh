#!/usr/bin/env bash
# Runs the real Claude Code CLI against the stub (#178): readiness answer, then one Write tool call and STATUS: done.
#   tools/claude-stub/stub.test.sh      needs node and claude (npm install -g @anthropic-ai/claude-code)
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
command -v claude >/dev/null || { echo "SKIP: claude CLI not installed"; exit 0; }
work=$(mktemp -d); port=$(( 20000 + RANDOM % 20000 ))
PORT=${port} node "${here}/stub.js" >/dev/null & pid=$!
trap 'kill ${pid} 2>/dev/null; rm -rf "${work}"' EXIT
for _ in $(seq 50); do curl -fs "http://127.0.0.1:${port}/healthz" >/dev/null && break; sleep 0.1; done
export HOME="${work}/home" ANTHROPIC_BASE_URL="http://127.0.0.1:${port}" ANTHROPIC_AUTH_TOKEN=stub-key
mkdir -p "${HOME}" "${work}/repo"; cd "${work}/repo"; git init -q
fail() { echo "FAIL: $*"; exit 1; }
out=$(timeout 90 claude -p "Reply with exactly: OK" --max-turns 1 2>&1) || fail "readiness: ${out}"
[[ "${out}" == *OK* ]] || fail "readiness answer: ${out}"
echo "PASS readiness"
res=$(timeout 120 claude -p "Do the task." --output-format stream-json --verbose --max-turns 5 \
        --permission-mode dontAsk --allowedTools Write 2>/dev/null | jq -rs '[.[]|objects|select(has("result"))]|last|.result // empty')
[[ -s STUB_CHANGE.md ]] || fail "no STUB_CHANGE.md (result: ${res})"
echo "PASS tool call wrote STUB_CHANGE.md"
grep -qx 'STATUS: done' <<<"${res}" || fail "no STATUS line: ${res}"
echo "PASS final answer with STATUS: done"
curl -fs "http://127.0.0.1:${port}/stats"; echo

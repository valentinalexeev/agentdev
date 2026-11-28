#!/usr/bin/env bash
# Static check that the worker tests can never reach the real forge sidecar or the agent's own Claude run (#621).
# An agent runs these tests inside its own pod, where FORGE_API is http://127.0.0.1:7070 (the pod's forge sidecar) and
# its Claude CLI runs as a process named claude. drain.test.sh inherited that FORGE_API: the SIGTERM handler pushed the
# fixture branch agent/alice/wip/issue-7 to the sidecar (a high security event, the pod was deleted) and its
# `pkill -KILL -x claude` killed the agent's Claude run (exit 137). In CI nothing listens there, so nothing failed.
#   bash images/worker/test/isolation.test.sh
set -uo pipefail
export FORGE_API=http://127.0.0.1:1   # #621: a closed port, never the forge sidecar of the pod this test may run in
here=$(cd "$(dirname "$0")" && pwd)
fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; fail=1; fi; }

# own_forge_api <file>: the test assigns FORGE_API, or unsets it (also on a continued `unset ... \` line)
own_forge_api() {
  grep -qE '(^|[^A-Za-z_])FORGE_API=' "$1" && return 0
  sed -e ':a' -e '/\\$/{N;s/\\\n//;ba' -e '}' "$1" | grep -qE '^[[:space:]]*unset[[:space:]][^#]*[[:space:]]FORGE_API([[:space:]]|$)'
}

for t in "${here}"/*.test.sh; do
  n=$(basename "${t}")
  [[ "${n}" == isolation.test.sh ]] && continue
  # every test sets FORGE_API itself (or unsets it), so the value of the pod never reaches the code under test
  check "${n}: sets or unsets FORGE_API" "own_forge_api '${t}'"
  check "${n}: never points FORGE_API at the sidecar port 7070" "! grep -qE 'FORGE_API=[^[:space:]]*:7070' '${t}'"
  # a test that runs the SIGTERM handler (on_term) stubs what it sends out: the wip push and the kill of claude
  if grep -q 'drain and graceful stop (#174)/' "${t}"; then
    check "${n}: runs on_term with stubs of curl and pkill" "grep -q '^curl() ' '${t}' && grep -q '^pkill() ' '${t}'"
  fi
done
exit "${fail}"

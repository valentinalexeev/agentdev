#!/usr/bin/env bash
# Test for scripts/release-lines.sh kind (#510). Run: bash scripts/test/release-lines-test.sh
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
script="$here/../release-lines.sh"
fail=0

check() { # name expected actual
  if [ "$2" = "$3" ]; then echo "PASS $1"; else echo "FAIL $1: expected '$2', got '$3'"; fail=1; fi
}

for b in chore/release-1.2.0 chore/release-1.2.0-rc.4 agent/agentdev/release-1.2.0 agent/bot-2/release-1.2.0-rc.4; do
  check "kind: $b" release "$("$script" kind "$b")"
done
for b in merge/release-1.0-into-main agent/agentdev/merge-release-1.2-into-main agent/x/merge-main-into-next-2.0; do
  check "kind: $b" merge "$("$script" kind "$b")"
done
for b in agent/agentdev/issue-510 agent/agentdev/x/release-1.2.0 agent/agentdev/x/merge-a agent//release-1.2.0 \
  agent/agentdev/release- feature/release-1.2.0 xagent/a/release-1.2.0 main release/1.2 chore/release-; do
  check "kind: $b" other "$("$script" kind "$b")"
done
"$script" kind >/dev/null 2>&1 && rc=ok || rc=refused
check "kind: no branch is refused" refused "$rc"

exit "$fail"

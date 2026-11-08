#!/usr/bin/env bash
# Test for scripts/upgrade-notes.sh (#315) with a fixture CHANGELOG. Run: scripts/test/upgrade-notes-test.sh
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
script="$here/../upgrade-notes.sh"
fixture="$here/fixtures/CHANGELOG.upgrade-notes.md"
fail=0

check() { # name expected actual
  if [ "$2" = "$3" ]; then echo "PASS $1"; else
    echo "FAIL $1"; diff <(printf '%s\n' "$2") <(printf '%s\n' "$3") || true; fail=1; fi
}
run() { CHANGELOG_FILE="$fixture" "$script" "$@"; }

check "rc.6 to 1.0.0: newest first, no rc.6, no other parts" "## 1.0.0: Upgrade
1. Step for 1.0.0.

## 1.0.0-rc.10: Upgrade
1. Step for rc.10.

## 1.0.0-rc.9: Upgrade
1. Step for rc.9.

## 1.0.0-rc.7: Upgrade
1. Step for rc.7." "$(run 1.0.0-rc.6 1.0.0)"

check "leading v accepted, TO inclusive" "## 1.0.0-rc.7: Upgrade
1. Step for rc.7." "$(run v1.0.0-rc.6 v1.0.0-rc.7)"

check "1.0.0 to 1.1.0: 1.0.1 has no Upgrade part, Unreleased ignored" "## 1.1.0: Upgrade
1. Step for 1.1.0.
   Second line." "$(run 1.0.0 1.1.0)"

check "rc.10 sorts after rc.9 (numeric identifiers)" "## 1.0.0-rc.10: Upgrade
1. Step for rc.10." "$(run 1.0.0-rc.9 1.0.0-rc.10)"

check "empty range prints nothing" "" "$(run 1.1.0 1.1.0)"

set +e
run 1.0 1.1.0 >/dev/null 2>&1; check "bad version exits 2" 2 "$?"
"$script" 1.0.0 >/dev/null 2>&1; check "one argument exits 2" 2 "$?"
set -e

exit "$fail"

#!/usr/bin/env bash
# Test for scripts/changelog.sh (#448) with fixtures in scripts/test/fixtures/changelog. Run: scripts/test/changelog-test.sh
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
script="$here/../changelog.sh"
fx="$here/fixtures/changelog"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
fail=0

check() { # name expected actual
  if [ "$2" = "$3" ]; then echo "PASS $1"; else
    echo "FAIL $1"; diff <(printf '%s\n' "$2") <(printf '%s\n' "$3") || true; fail=1; fi
}
setup() {
  rm -rf "$tmp/w"; mkdir -p "$tmp/w/frag"
  cp "$fx/CHANGELOG.md" "$tmp/w/CHANGELOG.md"
  cp "$fx/$1"/*.md "$tmp/w/frag/"
  export CHANGELOG_FILE="$tmp/w/CHANGELOG.md" CHANGELOG_DIR="$tmp/w/frag" CHANGES_DIR="$tmp/w/changes"
}

setup good
check "check: good fixtures pass" "ok" "$("$script" check | cut -d: -f1)"

setup bad
out=$("$script" check 2>&1) && rc=0 || rc=$?
check "check: bad fixtures exit 1" "1" "$rc"
for msg in \
  "7-bad.md: unknown key 'color'" \
  "7-bad.md: issue/issues must be positive integers" \
  "7-bad.md: type: 'thing' is not one of" \
  "7-bad.md: missing required key 'title'" \
  "7-bad.md: unknown section '## Notes'" \
  "7-bad.md: missing required '## Summary'" \
  "7-bad.md: breaking: true needs a '## Upgrade' section" \
  "8-wrong-name.md: '## Summary' has 7 lines, max. 6" \
  "Bad_Name.md: must start with a '---' frontmatter line"; do
  case "$out" in *"$msg"*) echo "PASS check reports: $msg" ;; *) echo "FAIL check misses: $msg"; fail=1 ;; esac
done
# a fragment that is valid except its name
rm "$tmp/w/frag/7-bad.md" "$tmp/w/frag/Bad_Name.md"
sed -i.bak '/^[4-7]$/d' "$tmp/w/frag/8-wrong-name.md"; rm -f "$tmp/w/frag/"*.bak
check "check: file name must match the issue" "8-wrong-name.md: file name issue 8 is not in issue/issues" \
  "$("$script" check 2>&1 || true)"

expected_section="### Added
- Existing added line (#2).
- **New thing (#20).** First line.
  Second line.

### Removed
- **BREAKING** **Old knob removed (#10, #12).** The knob is gone.

### Fixed
- Existing fix line (#1).

### Security
- **Quoted #title (#5).** Safer.

### Upgrade
**#10:**
1. Re-run \`./run.sh 05\`."

setup good
check "preview: existing lines kept, fragments merged in KaC order, done only" "## [Unreleased]

$expected_section" "$("$script" preview)"

"$script" release 1.1.0 2026-02-03 >/dev/null
check "release: CHANGELOG.md" "# Changelog

## [Unreleased]

## [1.1.0] - 2026-02-03

$expected_section

## [1.0.0] - 2026-01-01

### Added
- Old." "$(cat "$CHANGELOG_FILE")"
check "release: done and dropped deleted, in-progress kept" "30-later.md" "$(ls "$CHANGELOG_DIR")"
check "release: only fragments with a Description are moved" "20-new-thing.md" "$(ls "$CHANGES_DIR/1.1.0")"
check "release: description page" "# New thing (#20)

Released in 1.1.0 (2026-02-03). Type: feature. Section: Added. Components: operator, api. PR: #99.

## Summary

First line.
Second line.

## Description

Long story." "$(cat "$CHANGES_DIR/1.1.0/20-new-thing.md")"

out=$("$script" release 1.1.0 2026-02-03 2>&1) && rc=0 || rc=$?
check "release: same version twice is refused" "1" "$rc"
out=$("$script" release 'v1;x' 2026-02-03 2>&1) && rc=0 || rc=$?
check "release: bad version refused" "1" "$rc"

# second release with nothing new: empty section, Unreleased empty
"$script" release 1.1.1 2026-02-04 >/dev/null
check "release: empty release" "## [Unreleased]

## [1.1.1] - 2026-02-04

## [1.1.0] - 2026-02-03" "$(sed -n '3,7p' "$CHANGELOG_FILE")"

# pr-gate against a throw-away git repository
unset CHANGELOG_FILE CHANGELOG_DIR CHANGES_DIR
g="$tmp/git"; mkdir -p "$g/scripts"; cp "$here/../changelog.sh" "$here/../changelog.py" "$g/scripts/"
cp "$fx/CHANGELOG.md" "$g/"
(cd "$g" && git init -q -b main && git -c user.name=t -c user.email=t@t add -A && git -c user.name=t -c user.email=t@t commit -qm base)
gate() { # branch, then edits run in $g; prints the exit code
  (cd "$g" && git checkout -q -B "$1" main && eval "$2" && git -c user.name=t -c user.email=t@t add -A &&
    git -c user.name=t -c user.email=t@t commit -qm x --allow-empty &&
    HEAD_REF="$1" LABELS="${3:-}" scripts/changelog.sh pr-gate main >/dev/null 2>&1 && echo 0 || echo 1)
}
check "gate: code without fragment fails" 1 "$(gate f1 'echo x > a.go')"
check "gate: code without fragment, no-changelog label passes" 0 "$(gate f2 'echo x > a.go' 'docs,no-changelog')"
check "gate: code with fragment passes" 0 "$(gate f3 'echo x > a.go; mkdir -p changelog.d; cp '"$fx"'/good/5-sec.md changelog.d/')"
check "gate: docs only passes" 0 "$(gate f4 'mkdir -p docs; echo x > docs/a.md')"
check "gate: Unreleased edit fails" 1 "$(gate f5 'sed -i.b "s/Existing fix/Edited fix/" CHANGELOG.md; rm CHANGELOG.md.b')"
check "gate: edit of a released section passes" 0 "$(gate f6 'sed -i.b "s/^- Old\./- Old, fixed typo./" CHANGELOG.md; rm CHANGELOG.md.b')"
check "gate: release branch may edit Unreleased" 0 "$(gate chore/release-1.1.0 'sed -i.b "s/Existing fix/Edited fix/" CHANGELOG.md; rm CHANGELOG.md.b')"
# #510: the agent/<name>/ forms a sandboxed agent can push
check "gate: agent release branch may edit Unreleased" 0 "$(gate agent/agentdev/release-1.1.0 'sed -i.b "s/Existing fix/Edited fix/" CHANGELOG.md; rm CHANGELOG.md.b')"
check "gate: agent merge branch needs no fragment" 0 "$(gate agent/agentdev/merge-release-1.0-into-main 'echo y > b.go')"
check "gate: merge/ branch needs no fragment" 0 "$(gate merge/release-1.0-into-main 'echo z > c.go')"
check "gate: other agent branch still needs a fragment" 1 "$(gate agent/agentdev/issue-510 'echo w > d.go')"
check "gate: release- deeper than agent/<name>/ is not exempt" 1 "$(gate agent/agentdev/x/release-1.1.0 'echo v > e.go')"

exit "$fail"

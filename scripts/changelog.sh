#!/usr/bin/env bash
# Changelog fragments (#448). Usage:
#   scripts/changelog.sh check                     validate every changelog.d/ fragment
#   scripts/changelog.sh preview                   print the next release's entries (existing Unreleased + fragments)
#   scripts/changelog.sh release VERSION DATE      write "## [VERSION] - DATE" into CHANGELOG.md (release PR step)
#   scripts/changelog.sh pr-gate BASE [HEAD]       CI: fragment required for code changes, no direct Unreleased edits
# The format is in changelog.d/README.md. The logic lives in scripts/changelog.py (Python 3 standard library only).
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
py() { python3 "$here/changelog.py" "$@"; }

case "${1:-}" in
  check | preview | release | unreleased) py "$@" ;;
  pr-gate)
    # Env: LABELS (comma-separated PR labels), HEAD_REF (PR branch name). Exit 1 on a violation.
    base="${2:?usage: pr-gate BASE [HEAD]}" head="${3:-HEAD}"
    head_ref="${HEAD_REF:-}" labels=",${LABELS:-},"
    files=$(git diff --name-only "$base"..."$head")
    rc=0
    case "$head_ref" in
      chore/release-* | merge/*) echo "release or forward-merge branch '$head_ref': CHANGELOG.md edits allowed, no fragment needed"; exit 0 ;;
    esac
    if printf '%s\n' "$files" | grep -qx CHANGELOG.md; then
      mb=$(git merge-base "$base" "$head")
      if ! diff -q <(git show "$mb:CHANGELOG.md" | py unreleased -) <(git show "$head:CHANGELOG.md" | py unreleased -) >/dev/null; then
        echo "::error file=CHANGELOG.md::this PR edits '## [Unreleased]' in CHANGELOG.md. Add a changelog.d/<issue>-<slug>.md fragment instead (changelog.d/README.md)."
        rc=1
      fi
    fi
    # docs, tests, CI and repo meta need no fragment; everything else is code
    code=$(printf '%s\n' "$files" | grep -vE '^(docs/|changelog\.d/|\.github/|scripts/test/|README\.md$|CHANGELOG\.md$|CLAUDE\.md$|AGENTS\.md$|LICENSE|.*\.md$)' || true)
    frag=$(printf '%s\n' "$files" | grep -E '^changelog\.d/[0-9]+-[^/]+\.md$' || true)
    if [ -n "$code" ] && [ -z "$frag" ]; then
      if [[ "$labels" == *",no-changelog,"* ]]; then
        echo "code changed without a fragment, allowed by the 'no-changelog' label"
      else
        echo "::error::this PR changes code but adds or changes no changelog.d/<issue>-<slug>.md fragment. Add one (changelog.d/_template.md) or ask for the 'no-changelog' label. Code files:"
        printf '  %s\n' $code
        rc=1
      fi
    fi
    [ "$rc" = 0 ] && echo "ok: changelog gate"
    exit "$rc"
    ;;
  *) sed -n '2,7p' "$0" >&2; exit 64 ;;
esac

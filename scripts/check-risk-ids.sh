#!/usr/bin/env bash
# Fails when docs/security.md defines a risk id (table row "| Rnn |") more than once (#126),
# or when a file in the repository refers to an R id that docs/security.md does not define.
# Usage: scripts/check-risk-ids.sh [path/to/security.md]
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
doc="${1:-docs/security.md}"
rc=0

defined=$(grep -oE '^\| *R[0-9]+ *\|' "$doc" | grep -oE 'R[0-9]+' || true)
dups=$(printf '%s\n' "$defined" | sort | uniq -d)
if [ -n "$dups" ]; then
  for id in $dups; do
    echo "duplicate risk id $id in $doc:"
    grep -nE "^\| *$id *\|" "$doc" | cut -c1-120 | sed 's/^/  /'
  done
  echo "Renumber the newer row to the next free id: $(printf '%s\n' "$defined" | tr -d R | sort -n | tail -1 | awk '{print "R"$1+1}')"
  rc=1
fi

# References to undefined ids (a "R51"-style token next to "risk" or "security.md" on the same line).
undefined=$( { git grep -nIE 'R[0-9]' -- ':!CHANGELOG.md' ":!$doc" || true; } \
  | grep -E '(security\.md|[Rr]isks?).*\bR[0-9]+\b|\bR[0-9]+\b.*security\.md' \
  | while IFS= read -r line; do
      for id in $(printf '%s\n' "$line" | grep -oE '\bR[0-9]+\b'); do
        printf '%s\n' "$defined" | grep -qx "$id" || echo "${line%%:*}: $id"
      done
    done | sort -u)
if [ -n "$undefined" ]; then
  echo "references to risk ids not defined in $doc:"
  printf '  %s\n' "$undefined"
  rc=1
fi

[ "$rc" = 0 ] && echo "risk ids OK ($(printf '%s\n' "$defined" | grep -c .) defined, no duplicates)"
exit "$rc"

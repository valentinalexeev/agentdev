#!/usr/bin/env bash
# Print the "### Upgrade" sections of CHANGELOG.md for all versions in (FROM, TO] (#315).
# Usage:
#   scripts/upgrade-notes.sh [--repo PATH|URL] FROM TO   for example: scripts/upgrade-notes.sh 1.0.0-rc.6 1.0.0
# FROM and TO are versions or tags (a leading "v" is ignored). The script reads CHANGELOG.md at the tag vTO
# of the agentdev repository: --repo (a path or a git URL), default the repository that holds this script.
# It works from any directory, also from a deployment repository. A URL is fetched to a temporary bare clone.
# CHANGELOG_REF=<git ref> reads it at that ref (an untagged release branch), CHANGELOG_FILE=<path> reads a file (tests).
# Order: newest first, as in CHANGELOG.md. Versions use SemVer precedence (1.0.0-rc.6 < 1.0.0).
# Exit 0 with no output if no section in the range has an Upgrade part. Exit 2 on bad usage or no CHANGELOG.
set -euo pipefail

usage() { sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//' >&2; exit 2; }
repo="$(cd "$(dirname "$0")/.." && pwd)"
if [ "${1:-}" = --repo ]; then [ $# -ge 2 ] || usage; repo="$2"; shift 2; fi
[ $# -eq 2 ] || usage
from="${1#v}"
to="${2#v}"
semver_re='^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$'
[[ $from =~ $semver_re ]] || { echo "upgrade-notes: FROM is not a version: $1" >&2; exit 2; }
[[ $to =~ $semver_re ]] || { echo "upgrade-notes: TO is not a version: $2" >&2; exit 2; }

if [ -n "${CHANGELOG_FILE:-}" ]; then
  changelog=$(cat "$CHANGELOG_FILE")
else
  ref="${CHANGELOG_REF:-v$to}"
  if [ -d "$repo" ]; then
    gitdir="$repo"
    # A local clone without the tag: try to fetch tags once (no error if offline).
    git -C "$gitdir" rev-parse -q --verify "$ref^{commit}" >/dev/null 2>&1 \
      || git -C "$gitdir" fetch -q --tags origin >/dev/null 2>&1 || true
  else
    gitdir=$(mktemp -d); trap 'rm -rf "$gitdir"' EXIT
    git init -q --bare "$gitdir"
    # Fetch the ref (a tag or a branch) into refs/x and read CHANGELOG.md there.
    git -C "$gitdir" fetch -q --depth 1 "$repo" "$ref:refs/x" >/dev/null 2>&1 && ref=refs/x
  fi
  changelog=$(git -C "$gitdir" show "$ref:CHANGELOG.md" 2>/dev/null) || {
    echo "upgrade-notes: no CHANGELOG.md at $ref in $repo (fetch the tags, set --repo, or set CHANGELOG_REF)" >&2; exit 2; }
fi

# semver_cmp A B: print -1, 0 or 1 by SemVer 2.0.0 precedence (build metadata is not used here).
semver_cmp() {
  awk -v a="$1" -v b="$2" '
    function cmpid(x, y,  xn, yn) {
      xn = (x ~ /^[0-9]+$/); yn = (y ~ /^[0-9]+$/)
      if (xn && yn) return (x+0 < y+0) ? -1 : (x+0 > y+0)
      if (xn) return -1
      if (yn) return 1
      return (x < y) ? -1 : (x > y)
    }
    BEGIN {
      ap = index(a, "-"); bp = index(b, "-")
      ac = ap ? substr(a, 1, ap-1) : a; apre = ap ? substr(a, ap+1) : ""
      bc = bp ? substr(b, 1, bp-1) : b; bpre = bp ? substr(b, bp+1) : ""
      split(ac, x, "."); split(bc, y, ".")
      for (i = 1; i <= 3; i++) if (x[i]+0 != y[i]+0) { print (x[i]+0 < y[i]+0) ? -1 : 1; exit }
      if (apre == "" && bpre == "") { print 0; exit }
      if (apre == "") { print 1; exit }
      if (bpre == "") { print -1; exit }
      n = split(apre, p, "."); m = split(bpre, q, ".")
      for (i = 1; i <= n && i <= m; i++) { c = cmpid(p[i], q[i]); if (c != 0) { print c; exit } }
      print (n < m) ? -1 : (n > m)
    }'
}

in_range() { [ "$(semver_cmp "$1" "$from")" = 1 ] && [ "$(semver_cmp "$1" "$to")" != 1 ]; }

# Walk the file: "## [X]" starts a version, "### Upgrade" starts the part to print, any other "##"/"###" ends it.
version="" printing=0 printed=0 started=0 blanks=0
heading_re='^## \[([^]]+)\]'
while IFS= read -r line; do
  if [[ $line == '## '* ]]; then
    version="" printing=0
    if [[ $line =~ $heading_re ]]; then
      version="${BASH_REMATCH[1]}"
      if ! [[ $version =~ $semver_re ]] || ! in_range "$version"; then version=""; fi
    fi
    continue
  fi
  if [[ $line =~ ^###\  ]]; then
    printing=0
    if [ -n "$version" ] && [[ $line =~ ^###\ Upgrade[[:space:]]*$ ]]; then
      [ "$printed" = 1 ] && echo
      echo "## $version: Upgrade"
      printing=1 printed=1 started=0 blanks=0
    fi
    continue
  fi
  [ "$printing" = 1 ] || continue
  # Keep blank lines inside a part, drop them at its start and end.
  if [[ -z ${line//[[:space:]]/} ]]; then [ "$started" = 1 ] && blanks=$((blanks + 1)); continue; fi
  for ((i = 0; i < blanks; i++)); do echo; done
  blanks=0 started=1
  printf '%s\n' "$line"
done <<< "$changelog"
exit 0

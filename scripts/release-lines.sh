#!/usr/bin/env bash
# Release lines (ADR 0004): release/X.Y -> ... -> main -> next/X.0, each line contains every lower one.
# Usage:
#   scripts/release-lines.sh list                   print the active lines, lowest first
#   scripts/release-lines.sh check [--strict] [BASE REF]
#                                                   check every adjacent pair; with BASE and REF, also that every
#                                                   line below BASE is an ancestor of REF (release PR check)
#   scripts/release-lines.sh forward LOWER          merge LOWER into the next line up on a merge/ branch and push it
# Lines are read from the remote (default origin, override with REMOTE=...). check exits 1 on a pending
# forward merge only with --strict; without it, it prints warnings and exits 0.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
remote="${REMOTE:-origin}"

lines() {
  local refs
  refs=$(git for-each-ref --format='%(refname:strip=3)' "refs/remotes/$remote/")
  printf '%s\n' "$refs" | grep -E '^release/[0-9]+\.[0-9]+$' | sort -t/ -k2 -V || true
  printf '%s\n' "$refs" | grep -qx main && echo main
  printf '%s\n' "$refs" | grep -E '^next/[0-9]+\.0$' | sort -t/ -k2 -V || true
}

warn() {
  if [ -n "${GITHUB_ACTIONS:-}" ]; then echo "::warning::$*"; else echo "WARN: $*" >&2; fi
}

cmd="${1:-list}"
shift || true
case "$cmd" in
  list)
    lines
    ;;
  check)
    strict=0
    if [ "${1:-}" = "--strict" ]; then strict=1; shift; fi
    base="${1:-}" ref="${2:-}"
    mapfile -t L < <(lines)
    rc=0
    for ((i = 0; i + 1 < ${#L[@]}; i++)); do
      lo="${L[i]}" hi="${L[i + 1]}"
      if git merge-base --is-ancestor "$remote/$lo" "$remote/$hi"; then
        echo "ok   $lo is in $hi"
      else
        warn "pending forward merge: $lo -> $hi (scripts/release-lines.sh forward $lo)"
        rc=1
      fi
    done
    if [ -n "$base" ]; then
      [ -n "$ref" ] || { echo "check: BASE needs REF" >&2; exit 64; }
      printf '%s\n' "${L[@]}" | grep -qx "$base" || { echo "check: '$base' is not a line" >&2; exit 64; }
      for l in "${L[@]}"; do
        [ "$l" = "$base" ] && break
        if git merge-base --is-ancestor "$remote/$l" "$ref"; then
          echo "ok   $l is in $ref"
        else
          warn "$ref (into $base) does not contain $l: merge the lower lines forward first"
          rc=1
        fi
      done
    fi
    if [ "$strict" = 1 ]; then exit "$rc"; fi
    ;;
  forward)
    lo="${1:?usage: forward LOWER}"
    mapfile -t L < <(lines)
    hi=""
    for ((i = 0; i + 1 < ${#L[@]}; i++)); do
      if [ "${L[i]}" = "$lo" ]; then hi="${L[i + 1]}"; fi
    done
    [ -n "$hi" ] || { echo "no line above '$lo' (lines: ${L[*]})" >&2; exit 1; }
    if git merge-base --is-ancestor "$remote/$lo" "$remote/$hi"; then
      echo "$lo is already in $hi, nothing to do"
      exit 0
    fi
    branch="merge/${lo//\//-}-into-${hi//\//-}"
    git checkout -B "$branch" "$remote/$hi"
    if ! git merge --no-ff --no-edit -m "chore(merge): $lo into $hi" "$remote/$lo"; then
      echo "conflict: resolve on $branch, commit, push, and open the PR into $hi (owner approval needed)" >&2
      exit 2
    fi
    git push -u "$remote" "$branch"
    echo "open a PR $branch -> $hi; a conflict-free hop with green CI may be merged by the bot (ADR 0004)"
    ;;
  *)
    sed -n '2,10p' "$0" >&2
    exit 64
    ;;
esac

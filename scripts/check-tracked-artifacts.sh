#!/usr/bin/env bash
# Fails when git tracks build or OS artifacts (__pycache__, *.pyc/pyo/pyd, .DS_Store, node_modules)
# or compiled executables (ELF, Mach-O, PE; for example a Go binary built in images/<name>/).
# .gitignore only stops new files; this catches ones committed before a rule existed (PR #154, #272).
# Usage: scripts/check-tracked-artifacts.sh
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
bad=$(git ls-files | grep -E '(^|/)(__pycache__|node_modules)/|\.py[cod]$|(^|/)\.DS_Store$' || true)
# Compiled executables: check the magic bytes of every tracked file.
while IFS= read -r -d '' f; do
  [ -f "$f" ] || continue
  magic=$(head -c 4 "$f" | od -An -tx1 | tr -d ' \n')
  case "$magic" in
    7f454c46|cffaedfe|cefaedfe|feedfacf|feedface|cafebabe|4d5a*) bad="${bad:+$bad$'\n'}$f (compiled binary)" ;;
  esac
done < <(git ls-files -z)
if [ -n "$bad" ]; then
  echo "tracked artifacts (remove with: git rm -r --cached <path>):"
  printf '%s\n' "$bad" | sed 's/^/  /'
  exit 1
fi
echo "no tracked artifacts"

#!/usr/bin/env bash
# Fails when git tracks build or OS artifacts (__pycache__, *.pyc/pyo/pyd, .DS_Store, node_modules).
# .gitignore only stops new files; this catches ones committed before a rule existed (PR #154).
# Usage: scripts/check-tracked-artifacts.sh
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
bad=$(git ls-files | grep -E '(^|/)(__pycache__|node_modules)/|\.py[cod]$|(^|/)\.DS_Store$' || true)
if [ -n "$bad" ]; then
  echo "tracked artifacts (remove with: git rm -r --cached <path>):"
  printf '%s\n' "$bad" | sed 's/^/  /'
  exit 1
fi
echo "no tracked artifacts"

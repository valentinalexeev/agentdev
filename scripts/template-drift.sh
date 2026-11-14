#!/usr/bin/env bash
# Template drift (#317): compare a deployment repository with examples/deployment/gcp-gke of its pinned release.
# Usage:
#   scripts/template-drift.sh [--strict] <deployment repo dir>
# The release is the ref= of the agentdev module in <dir>/terraform/main.tf. The script compares:
#   1. .github/workflows/deploy.yml   full diff (a workflow copy is never updated by an upgrade)
#   2. terraform/main.tf              skeleton only: block names and module argument names (no values, no comments;
#                                     a commented-out argument of the template counts as present)
#   3. ansible/instance.yml           top-level keys against instance.yml.example (commented keys are optional)
# The example comes from `git show <ref>:<path>` in AGENTDEV_REPO (default: the agentdev checkout of this script).
# If that tag is not there, the files are fetched from AGENTDEV_RAW_URL (default raw.githubusercontent.com).
# Exit: 0 with warnings; 1 on drift only with --strict; 2 on a usage error or when the example cannot be read.
# In GitHub Actions, the warnings are also written to $GITHUB_STEP_SUMMARY and as ::warning:: annotations.
set -euo pipefail
strict=0
[[ "${1:-}" == "--strict" ]] && { strict=1; shift; }
dir="${1:-}"
[[ -n "$dir" && -d "$dir" ]] || { echo "usage: $0 [--strict] <deployment repo dir>" >&2; exit 2; }
here="$(cd "$(dirname "$0")/.." && pwd)"
repo="${AGENTDEV_REPO:-$here}"
raw="${AGENTDEV_RAW_URL:-https://raw.githubusercontent.com/valentinalexeev/agentdev}"
ex=examples/deployment/gcp-gke

ref=$(grep -oE 'agentdev//infra/terraform\?ref=[^"]+' "$dir/terraform/main.tf" 2>/dev/null | head -1 | sed 's/.*ref=//') || true
[[ -n "$ref" ]] || { echo "ERROR: no ref= of the agentdev module in $dir/terraform/main.tf" >&2; exit 2; }

work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
fetch() { # $1 = path in the example -> $work/<basename>
  local out; out="$work/$(basename "$1")"
  if git -C "$repo" rev-parse -q --verify "refs/tags/$ref^{commit}" >/dev/null 2>&1; then
    git -C "$repo" show "$ref:$ex/$1" > "$out" 2>/dev/null && return 0
  fi
  curl -fsSL "$raw/$ref/$ex/$1" -o "$out" 2>/dev/null
}
for f in .github/workflows/deploy.yml terraform/main.tf ansible/instance.yml.example; do
  fetch "$f" || { echo "ERROR: cannot read $ex/$f at $ref (tag missing in $repo and no download)" >&2; exit 2; }
done

drift=0 report=""
warn() { # $1 = title, $2 = details
  drift=1
  echo "WARNING: $1"; [[ -n "$2" ]] && printf '%s\n' "$2"
  if [[ -n "${GITHUB_ACTIONS:-}" ]]; then echo "::warning title=template drift::$1"; fi
  report+=$'\n'"### $1"$'\n\n```diff\n'"$2"$'\n```\n'
}

# 1. deploy.yml
mine="$dir/.github/workflows/deploy.yml"
if [[ ! -f "$mine" ]]; then
  warn ".github/workflows/deploy.yml is missing (template of $ref has one)" ""
elif ! d=$(diff -u --label "template $ref" --label "this repository" "$work/deploy.yml" "$mine"); then
  warn ".github/workflows/deploy.yml differs from the template of $ref" "$d"
fi

# 2. main.tf skeleton
skeleton() {
  awk '
    { line = $0 }
    line ~ /^[[:space:]]*#[[:space:]]*[a-z_]+[[:space:]]*=/ && inmod { sub(/#[[:space:]]*/, "", line) }  # commented argument
    { sub(/#.*/, "", line) }
    line ~ /^(terraform|module|variable|output|resource|data|locals|removed|moved|import|provider)([[:space:]]|$|\{)/ {
      split(line, a, "{"); h = a[1]; gsub(/[[:space:]]+$/, "", h); print h
      inmod = (line ~ /^module /); next
    }
    line ~ /^}/ { inmod = 0; next }
    inmod && line ~ /^  [a-z_]+[[:space:]]*=/ { n = line; sub(/=.*/, "", n); gsub(/[[:space:]]/, "", n); print "  arg " n }
  ' "$1" | sort -u
}
if ! d=$(diff -u --label "template $ref" --label "this repository" <(skeleton "$work/main.tf") <(skeleton "$dir/terraform/main.tf")); then
  warn "terraform/main.tf skeleton differs from the template of $ref (blocks and module arguments; + is only here)" "$d"
fi

# 3. instance.yml keys
keys() { grep -oE "^${2:-}[a-z_][a-z0-9_]*:" "$1" | sed -E 's/^# ?//; s/:$//' | sort -u; }
inst="$dir/ansible/instance.yml"; [[ -f "$inst" ]] || inst="$dir/ansible/instance.yml.example"
if [[ -f "$inst" ]]; then
  keys "$work/instance.yml.example" > "$work/req"
  { cat "$work/req"; keys "$work/instance.yml.example" '# ?'; } | sort -u > "$work/all"
  keys "$inst" > "$work/have"
  missing=$(comm -23 "$work/req" "$work/have" | sed 's/^/- /')
  unknown=$(comm -13 "$work/all" "$work/have" | sed 's/^/+ /')
  if [[ -n "$missing$unknown" ]]; then
    warn "ansible/$(basename "$inst") keys differ from instance.yml.example of $ref (- missing here, + not in the template)" \
      "$(printf '%s\n%s' "$missing" "$unknown" | sed '/^$/d')"
  fi
fi

if (( drift )); then
  if [[ -n "${GITHUB_STEP_SUMMARY:-}" ]]; then
    { echo "## ⚠️ Template drift against agentdev $ref"; echo
      echo "Copy the changed parts from examples/deployment/gcp-gke at $ref (agentdev docs/admin.md, Upgrades)."
      printf '%s\n' "$report"; } >> "$GITHUB_STEP_SUMMARY"
  fi
  (( strict )) && exit 1
else
  echo "OK: no template drift against $ref"
fi
exit 0

#!/usr/bin/env bash
# repin.sh: find newer versions of the pins that Dependabot does not see, and update them in place (#repin).
#   scripts/repin.sh            check only: print a table, exit 0 (also when updates exist)
#   scripts/repin.sh --write    also rewrite the pins in the work tree
# Pins checked:
#   config.env  *_VERSION / *_CHART_VERSION of Helm charts and GitHub releases, gVisor, image digests (@sha256)
#   .github/workflows/*.yml  container images pinned as <image>:<tag>@sha256:<digest>
# Policy: stay on the same major version (a new major is listed as "major", not written). Digests of a
# pinned tag are refreshed. Each upstream is asked with a timeout and retries; a failed lookup is "error" and does
# not stop the other checks. Output: a Markdown table on stdout, and $REPIN_SUMMARY (if set) gets the same table.
set -uo pipefail
cd "$(dirname "$0")/.." || exit 1
WRITE=0; [ "${1:-}" = --write ] && WRITE=1
rows=(); changed=0; errors=0

get() { curl -fsSL --retry 3 --retry-all-errors --retry-delay 5 -m 30 "$@"; }
major() { printf '%s' "${1#v}" | cut -d. -f1; }
# newest version among stdin lines, optionally only within major $1
newest() { if [ -n "${1:-}" ]; then grep -E "^v?$1\." ; else cat; fi | sort -V | tail -1; }
row() { rows+=("| $1 | \`$2\` | \`$3\` | $4 |"); }

# config.env pin: VAR="${VAR:-value}"
cfg_get() { sed -n "s/^$1=\"\${$1:-\(.*\)}\".*/\1/p" config.env | head -1; }
cfg_set() { local v=$1 new=$2; sed -i.bak "s|^\($v=\"\${$v:-\)[^}]*\(}\".*\)|\1$new\2|" config.env && rm -f config.env.bak; }

pin() {  # pin <name> <current> <newest> <setter...>
  local name=$1 cur=$2 new=$3; shift 3
  if [ -z "$new" ]; then row "$name" "$cur" "?" "error"; errors=$((errors + 1)); return; fi
  if [ "$new" = "$cur" ]; then row "$name" "$cur" "$new" "current"; return; fi
  row "$name" "$cur" "$new" "update"; changed=$((changed + 1))
  [ "$WRITE" = 1 ] && "$@" "$new"
}

helm_chart() {  # helm_chart <VAR> <index url> <chart>
  local cur new; cur=$(cfg_get "$1")
  new=$(get "$2" | yq -r ".entries[\"$3\"][] | select(.version | test(\"-\") | not) | .version" 2>/dev/null | newest "$(major "$cur")")
  pin "$3 chart ($1)" "$cur" "$new" cfg_set "$1"
  local any; any=$(get "$2" 2>/dev/null | yq -r ".entries[\"$3\"][] | select(.version | test(\"-\") | not) | .version" | newest)
  [ -n "$any" ] && [ "$(major "$any")" != "$(major "$cur")" ] && row "$3 chart ($1)" "$cur" "$any" "major (not written)"
}

gh_release() {  # gh_release <VAR> <owner/repo>
  local cur new; cur=$(cfg_get "$1")
  new=$(get "https://api.github.com/repos/$2/releases?per_page=50" | jq -r '.[] | select(.prerelease | not) | .tag_name' | newest "$(major "$cur")")
  pin "$2 ($1)" "$cur" "$new" cfg_set "$1"
}

gvisor() {
  local cur new; cur=$(cfg_get GVISOR_VERSION)
  new=$(get "https://storage.googleapis.com/storage/v1/b/gvisor/o?prefix=releases/release/&delimiter=/&maxResults=1000" |
    jq -r '.prefixes[]' | sed -n 's|releases/release/\([0-9]\{8\}\.[0-9]*\)/|\1|p' | newest)
  pin "gVisor (GVISOR_VERSION)" "$cur" "$new" cfg_set GVISOR_VERSION
}

digest_of() { docker buildx imagetools inspect "$1" --format '{{json .Manifest}}' 2>/dev/null | jq -r .digest; }

image_digests() {  # every <image>:<tag>@sha256:<digest> in config.env and the workflows: refresh the digest of the tag
  local f ref img dig new
  for f in config.env .github/workflows/*.yml; do
    grep -oE '[a-z0-9][a-z0-9._-]*(/[a-z0-9._-]+)+:[A-Za-z0-9._-]+@sha256:[a-f0-9]{64}' "$f" |
      sort -u | while read -r ref; do
      img=${ref%@*}; dig=${ref#*@}
      new=$(digest_of "$img")
      if [ -z "$new" ]; then echo "| $img ($f) | \`${dig:7:12}\` | \`?\` | error |"; continue; fi
      if [ "$new" = "$dig" ]; then echo "| $img ($f) | \`${dig:7:12}\` | \`${new:7:12}\` | current |"; continue; fi
      echo "| $img ($f) | \`${dig:7:12}\` | \`${new:7:12}\` | update (digest) |"
      [ "$WRITE" = 1 ] && sed -i.bak "s|$ref|$img@$new|g" "$f" && rm -f "$f.bak"
    done
  done
}

newer_tags() {  # list a newer same-major tag for pinned images (informational: tags are bumped by hand)
  local f ref img tag repo cur new
  for f in config.env .github/workflows/*.yml; do
    grep -oE '(ghcr\.io)/[a-z0-9._/-]+:v?[0-9]+\.[0-9]+(\.[0-9]+)?' "$f" | sort -u | while read -r ref; do
      img=${ref%:*}; tag=${ref##*:}; repo=${img#ghcr.io/}
      tok=$(get "https://ghcr.io/token?scope=repository:$repo:pull" | jq -r .token)
      new=$(get -H "Authorization: Bearer $tok" "https://ghcr.io/v2/$repo/tags/list?n=1000" | jq -r '.tags[]' |
        grep -E '^v?[0-9]+\.[0-9]+(\.[0-9]+)?$' | newest "$(major "$tag")")
      [ -n "$new" ] && [ "$new" != "$tag" ] && echo "| $img ($f) | \`$tag\` | \`$new\` | newer tag (bump by hand) |"
    done
  done
}

helm_chart AUTHENTIK_VERSION https://charts.goauthentik.io/index.yaml authentik
helm_chart ESO_CHART_VERSION https://charts.external-secrets.io/index.yaml external-secrets
helm_chart CODER_CHART_VERSION https://helm.coder.com/v2/index.yaml coder
gh_release AGENT_SANDBOX_VERSION kubernetes-sigs/agent-sandbox
gvisor

# the Ansible authentik default follows config.env (#45: chart = app version)
if [ "$WRITE" = 1 ]; then
  a=$(cfg_get AUTHENTIK_VERSION)
  sed -i.bak "s|default('[0-9.]*')) }}\"   # chart = app version|default('$a')) }}\"   # chart = app version|" \
    infra/ansible/roles/authentik/defaults/main.yml && rm -f infra/ansible/roles/authentik/defaults/main.yml.bak
fi

{
  echo "| Pin | Now | Newest | Result |"
  echo "|---|---|---|---|"
  printf '%s\n' "${rows[@]}"
  image_digests
  newer_tags
} | tee "${REPIN_SUMMARY:-/dev/null}"
echo
echo "updates: $changed, errors: $errors (digest rows are counted by the workflow from the table)"

#!/usr/bin/env bash
# repin.sh: find newer versions of the pinned dependencies and, with --write, update the simple ones in place.
#   scripts/repin.sh            check only: print a Markdown table, exit 0 (also when updates exist)
#   scripts/repin.sh --write    also rewrite the pins of the rows "update" and "update (digest)"
# Pins checked:
#   config.env       Helm charts, GitHub releases, gVisor, image digests (@sha256)
#   workflows        images pinned as <image>:<tag>@sha256:<digest>; actions (uses: owner/repo@vN)
#   Dockerfiles      base images (FROM image:tag)
#   Go modules       direct requirements of each go.mod (go list -m -u)
#   npm              images/ui direct dependencies (npm outdated)
# Policy: stay on the same major version (a new major is listed, not written). Go, npm, actions and base images are
# listed only; the monthly issue asks for a PR that bumps them. Each lookup has a timeout and retries; a failed lookup
# is "error" and does not stop the other checks. $REPIN_SUMMARY (if set) gets the same table.
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

go_modules() {  # direct requirements with a newer version (listed; bump with go get in the PR)
  local m
  command -v go >/dev/null || { echo "| Go modules | | | error (no go) |"; return; }
  for m in $(git ls-files '*go.mod' | xargs -n1 dirname); do
    (cd "$m" && go list -m -u -json all 2>/dev/null) | jq -rs --arg m "$m" \
      '.[] | select(.Update and (.Indirect | not) and (.Main | not)) |
       "| \(.Path) (\($m)) | `\(.Version)` | `\(.Update.Version)` | " +
       (if (.Version | ltrimstr("v") | split(".")[0]) == (.Update.Version | ltrimstr("v") | split(".")[0])
        then "newer version (go get)" else "major (not written)" end) + " |"'
  done
}

npm_deps() {  # images/ui direct dependencies: version in package-lock.json against the registry
  command -v npm >/dev/null || { echo "| npm | | | error (no npm) |"; return; }
  local name cur new
  jq -r '(.dependencies // {}) + (.devDependencies // {}) | keys[]' images/ui/package.json | while read -r name; do
    cur=$(jq -r --arg n "node_modules/$name" '.packages[$n].version // empty' images/ui/package-lock.json)
    new=$(npm view "$name" version 2>/dev/null)
    [ -z "$cur" ] || [ -z "$new" ] || [ "$cur" = "$new" ] && continue
    if [ "$(major "$cur")" = "$(major "$new")" ]; then
      echo "| $name (images/ui) | \`$cur\` | \`$new\` | newer version (npm) |"
    else
      echo "| $name (images/ui) | \`$cur\` | \`$new\` | major (not written) |"
    fi
  done
}

actions() {  # uses: owner/repo@vN in the workflows: newest release tag of the same major and of any major
  local a repo ref new
  grep -hoE 'uses: [A-Za-z0-9._-]+/[A-Za-z0-9._/-]+@v[0-9]+' .github/workflows/*.yml | sed 's/uses: //' | sort -u |
    while read -r a; do
      repo=$(printf '%s' "${a%@*}" | cut -d/ -f1-2); ref=${a#*@}
      new=$(get -H "Authorization: Bearer ${GH_TOKEN:-}" "https://api.github.com/repos/$repo/releases?per_page=30" 2>/dev/null |
        jq -r '.[] | select(.prerelease | not) | .tag_name' | grep -E '^v[0-9]+' | newest)
      if [ -z "$new" ]; then echo "| $repo (action) | \`$ref\` | \`?\` | error |"; continue; fi
      [ "$(major "$new")" != "$(major "$ref")" ] && echo "| $repo (action) | \`$ref\` | \`$new\` | major (not written) |"
    done
}

base_images() {  # FROM image:tag in the Dockerfiles: newer tag of the same major (Docker Hub, ghcr and gcr not asked)
  local f img tag repo new
  for f in $(git ls-files '*Dockerfile*'); do
    sed -n 's/^FROM \(--platform=[^ ]* \)\{0,1\}\([^ ]*\).*/\2/p' "$f" | while read -r img; do
      tag=${img##*:}; repo=${img%:*}
      case "$repo" in */*/*|gcr.io/*|ghcr.io/*) continue ;; esac   # only Docker Hub here
      case "$repo" in */*) ;; *) repo=library/$repo ;; esac
      printf '%s' "$tag" | grep -qE '^[0-9]+\.[0-9]+(\.[0-9]+)?(-[a-z0-9]+)?$' || continue   # only x.y[.z][-variant]
      suffix=$(printf '%s' "$tag" | sed -n 's/^[0-9.]*\(-[a-z0-9]*\)$/\1/p')
      new=$(get "https://hub.docker.com/v2/repositories/$repo/tags?page_size=100&ordering=last_updated" 2>/dev/null |
        jq -r '.results[].name' | grep -E "^[0-9]+\.[0-9]+(\.[0-9]+)?${suffix}\$" | sed "s/${suffix}\$//" |
        newest "$(major "$tag")")
      [ -n "$new" ] && [ "$new$suffix" != "$tag" ] && echo "| ${img%:*} ($f) | \`$tag\` | \`$new$suffix\` | newer tag (bump by hand) |"
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
  base_images
  actions
  go_modules
  npm_deps
} | tee "${REPIN_SUMMARY:-/dev/null}"
echo
echo "updates: $changed, errors: $errors (digest rows are counted by the workflow from the table)"

#!/usr/bin/env bash
# GitHub App for agentdev agents (#2): register it from a manifest, the per-deployment URLs filled in here.
#
#   scripts/github-app.sh render  <instance.yml>       print the manifest for this deployment (review it)
#   scripts/github-app.sh form    <instance.yml>       write a local HTML page; open it, GitHub asks you to confirm
#   scripts/github-app.sh convert <instance.yml> <code> exchange the one-time code (valid 1 h) for App ID, private
#                                                       key and webhook secret, into .secrets/github-app/ (gitignored)
#
# The deployment decides every URL, so they come from the instance file, never from this repo:
#   github_app:
#     name: my-agentdev                 # globally unique on GitHub
#     owner: my-org                     # org that owns the App; omit for your personal account
#     public_url: https://agentdev.example.com      # where the webhook router is reachable over HTTPS
#     homepage_url: https://agentdev.example.com    # optional, defaults to public_url
# Webhook URL = <public_url>/ (the router verifies X-Hub-Signature-256). After registration GitHub redirects to
# <public_url>/github-app/registered?code=...; the page only needs to show the code (nothing listens there yet),
# so copy it from the browser address bar into `convert`.
# The script never registers or installs anything by itself: you confirm on github.com, as the owner.
source "$(dirname "$0")/../lib.sh"
need yq; need jq; need envsubst; need curl

cmd=${1:?usage: $0 render|form|convert <instance.yml> [code]}
inst=${2:?instance.yml required}
[[ -f "${inst}" ]] || die "${inst}: not found"
name=$(yq -r '.github_app.name // ""' "${inst}")
org=$(yq -r '.github_app.owner // ""' "${inst}")
pub=$(yq -r '.github_app.public_url // ""' "${inst}"); pub=${pub%/}
home=$(yq -r '.github_app.homepage_url // ""' "${inst}")
[[ -n "${name}" ]] || die "github_app.name missing in ${inst}"
[[ "${pub}" == https://* ]] || die "github_app.public_url must be an https:// URL (GitHub only delivers webhooks to public HTTPS)"

render() {
  APP_NAME="${name}" APP_HOMEPAGE_URL="${home:-${pub}}" WEBHOOK_URL="${pub}/" REDIRECT_URL="${pub}/github-app/registered" \
    envsubst < "${SPIKE_DIR}/manifests/github-app-manifest.json.tmpl" | jq .
}

case "${cmd}" in
  render) render ;;
  form)
    out="${SPIKE_DIR}/.secrets/github-app/register.html"; mkdir -p "$(dirname "${out}")"; chmod 700 "$(dirname "${out}")"
    target="https://github.com/settings/apps/new"
    [[ -n "${org}" ]] && target="https://github.com/organizations/${org}/settings/apps/new"
    manifest=$(render | jq -c . | sed 's/&/\&amp;/g; s/"/\&quot;/g')
    cat > "${out}" <<EOF
<!doctype html><meta charset="utf-8"><title>Register ${name}</title>
<p>Registers the GitHub App <b>${name}</b> for agentdev. GitHub shows the settings and asks you to confirm.</p>
<form action="${target}?state=agentdev" method="post">
<input type="hidden" name="manifest" value="${manifest}"><button type="submit">Create GitHub App on github.com</button></form>
EOF
    ok "open ${out} in a browser, signed in to GitHub as the owner"
    echo "  then: $0 convert ${inst} <code from the redirect URL>   (within 1 hour)" ;;
  convert)
    code=${3:?the code from the redirect URL}
    d="${SPIKE_DIR}/.secrets/github-app"; mkdir -p "${d}"; chmod 700 "${d}"
    resp=$(curl -fsS -X POST -H "Accept: application/vnd.github+json" "https://api.github.com/app-manifests/${code}/conversions") \
      || die "conversion failed (code used already, or older than 1 hour?)"
    umask 077
    jq -r .id <<<"${resp}" > "${d}/app-id"
    jq -r .slug <<<"${resp}" > "${d}/app-slug"
    jq -r .pem <<<"${resp}" > "${d}/private-key.pem"
    jq -r .webhook_secret <<<"${resp}" > "${d}/webhook-secret"
    jq -r .client_id <<<"${resp}" > "${d}/client-id"
    ok "App $(cat "${d}/app-slug") (id $(cat "${d}/app-id")): secrets in ${d}/ (never commit them)"
    echo "  next: install it on the repos the agents work: https://github.com/apps/$(cat "${d}/app-slug")/installations/new"
    echo "  GCP: store them in Secret Manager (see docs/admin.md, GitHub App); kind: the router reads FORGE_SECRETS github=<webhook secret>" ;;
  *) die "unknown command ${cmd}" ;;
esac

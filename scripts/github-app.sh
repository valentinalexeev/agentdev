#!/usr/bin/env bash
# GitHub App for agentdev agents (#2): register it from a manifest, the per-deployment URLs filled in here.
#
#   scripts/github-app.sh render  <instance.yml>       print the manifest for this deployment (review it)
#   scripts/github-app.sh form    <instance.yml>       write a local HTML page; open it, GitHub asks you to confirm
#   scripts/github-app.sh convert <instance.yml> <url>  exchange the one-time code in the redirect URL (valid 1 h,
#                                                       state checked) for App ID, private key and webhook secret,
#                                                       into .secrets/github-app/ (gitignored, mode 600)
#
# The deployment decides every URL, so they come from the instance file, never from this repo:
#   github_app:
#     name: my-agentdev                 # globally unique on GitHub
#     owner: my-org                     # org that owns the App; omit for your personal account
#     public_url: https://agentdev.example.com      # where the webhook router is reachable over HTTPS
#     homepage_url: https://agentdev.example.com    # optional, defaults to public_url
# Webhook URL = <public_url>/ (the router verifies X-Hub-Signature-256). After registration GitHub redirects to
# <public_url>/github-app/registered?code=...&state=...; nothing listens there yet (a 404 is fine), so copy the
# whole URL from the browser address bar into `convert`.
# The script never registers or installs anything by itself: you confirm on github.com, as the owner.
source "$(dirname "$0")/../lib.sh"
need yq; need jq; need curl; need openssl

cmd=${1:?usage: $0 render|form|convert <instance.yml> [code]}
inst=${2:?instance.yml required}
[[ -f "${inst}" ]] || die "${inst}: not found"
name=$(yq -r '.github_app.name // ""' "${inst}")
org=$(yq -r '.github_app.owner // ""' "${inst}")
pub=$(yq -r '.github_app.public_url // ""' "${inst}"); pub=${pub%/}
home=$(yq -r '.github_app.homepage_url // ""' "${inst}")
[[ -n "${name}" ]] || die "github_app.name missing in ${inst}"
# strict values: they end up in JSON, HTML and a URL; GitHub's own limits are similar
[[ "${name}" =~ ^[A-Za-z0-9][A-Za-z0-9._\ -]{0,33}$ ]] || die "github_app.name: letters, digits, space, . _ - (max 34)"
[[ -z "${org}" || "${org}" =~ ^[A-Za-z0-9-]{1,39}$ ]] || die "github_app.owner: a GitHub org login"
url_re='^https://[A-Za-z0-9.-]+(:[0-9]+)?(/[A-Za-z0-9._~/-]*)?$'
[[ "${pub}" =~ ${url_re} ]] || die "github_app.public_url must be a plain https:// URL (GitHub only delivers webhooks to public HTTPS)"
[[ -z "${home}" || "${home}" =~ ${url_re} ]] || die "github_app.homepage_url must be a plain https:// URL"

# the template fixes the shape; jq fills the values (JSON-escaped), so no value can change the structure
render() {
  jq --arg name "${name}" --arg home "${home:-${pub}}" --arg hook "${pub}/" --arg redirect "${pub}/github-app/registered" \
    '.name=$name | .url=$home | .hook_attributes.url=$hook | .redirect_url=$redirect' \
    "${SPIKE_DIR}/manifests/github-app-manifest.json.tmpl"
}

case "${cmd}" in
  render) render ;;
  form)
    out="${SPIKE_DIR}/.secrets/github-app/register.html"; mkdir -p "$(dirname "${out}")"; chmod 700 "$(dirname "${out}")"
    target="https://github.com/settings/apps/new"
    [[ -n "${org}" ]] && target="https://github.com/organizations/${org}/settings/apps/new"
    manifest=$(render | jq -c . | sed 's/&/\&amp;/g; s/"/\&quot;/g; s/</\&lt;/g; s/>/\&gt;/g')
    # one-time state: GitHub sends it back with the code; convert checks it (no foreign code is accepted)
    state=$(openssl rand -hex 16); (umask 077; printf '%s' "${state}" > "$(dirname "${out}")/state")
    rm -f "${out}"; (umask 077; cat > "${out}" <<EOF
<!doctype html><meta charset="utf-8"><title>Register ${name}</title>
<p>Registers the GitHub App <b>${name}</b> for agentdev. GitHub shows the settings and asks you to confirm.</p>
<form action="${target}?state=${state}" method="post">
<input type="hidden" name="manifest" value="${manifest}"><button type="submit">Create GitHub App on github.com</button></form>
EOF
)
    ok "open ${out} in a browser, signed in to GitHub as the owner"
    echo "  GitHub then redirects to ${pub}/github-app/registered?code=...&state=...; a 404 there is expected"
    echo "  then, within 1 hour: $0 convert ${inst} '<the full redirect URL>'" ;;
  convert)
    redirect=${3:?the full redirect URL from the browser address bar}
    d="${SPIKE_DIR}/.secrets/github-app"; mkdir -p "${d}"; chmod 700 "${d}"
    code=$(sed -n 's/.*[?&]code=\([A-Za-z0-9]*\).*/\1/p' <<<"${redirect}")
    got=$(sed -n 's/.*[?&]state=\([A-Za-z0-9]*\).*/\1/p' <<<"${redirect}")
    [[ -n "${code}" ]] || die "no code in that URL"
    [[ -s "${d}/state" && "${got}" == "$(cat "${d}/state")" ]] || die "state does not match the form this machine generated: not converting"
    resp=$(curl -fsS -X POST -H "Accept: application/vnd.github+json" "https://api.github.com/app-manifests/${code}/conversions") \
      || die "conversion failed (code used already, or older than 1 hour?)"
    rm -f "${d}/state"
    umask 077
    for f in app-id app-slug private-key.pem webhook-secret client-id; do rm -f "${d}/${f}"; done   # re-run: no stale modes
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

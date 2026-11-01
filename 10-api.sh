#!/usr/bin/env bash
# agentdev API (#46): Go service behind authentik OIDC, in ${PLATFORM_NS}. After 09.
#   ./run.sh 10      build (kind) and deploy, idempotent
# The OIDC application `agentdev` comes from an authentik blueprint (09). OIDC_ISSUER must be the URL users reach
# authentik through, because authentik puts that URL into tokens (locally: the https port-forward).
source "$(dirname "$0")/lib.sh"
need envsubst; need openssl; need jq; need curl
k -n "${PLATFORM_NS}" get deploy authentik-server >/dev/null 2>&1 || die "authentik not deployed: run ./run.sh 09 first"

log "Image ${API_IMAGE}"
if [[ "${API_IMAGE}" == agentdev-api:* ]]; then
  "${CONTAINER_ENGINE}" build -q -t "${API_IMAGE}" "${SPIKE_DIR}/images/api" >/dev/null
  kind load docker-image "${API_IMAGE}" --name "${CLUSTER_NAME}" >/dev/null
  ok "built and loaded"
else ok "using published image"; fi

log "Database (security_events, #70)"
# login agentdev_api, database agentdev; password generated, hex only (safe in SQL)
S="${SPIKE_DIR}/.secrets/platform"; mkdir -p "${S}"; chmod 700 "${S}"
[[ -s "${S}/pg-agentdev-api" ]] || { openssl rand -hex 24 > "${S}/pg-agentdev-api"; chmod 600 "${S}/pg-agentdev-api"; }
pgpw=$(cat "${S}/pg-agentdev-api")
# Owner role agentdev_owner (no login) owns database and table; the API login gets INSERT/SELECT and the prune
# function only (images/api/grants.sql), so it cannot delete, truncate or change the retention (docs/security.md).
retention="${SECURITY_DB_RETENTION_DAYS:-365}"; [[ "${retention}" =~ ^[1-9][0-9]*$ ]] || die "SECURITY_DB_RETENTION_DAYS must be a positive number"
k -n "${PLATFORM_NS}" exec -i platform-db-0 -- psql -U postgres -v ON_ERROR_STOP=1 -q >/dev/null <<SQL
SELECT 'CREATE ROLE agentdev_owner NOLOGIN' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname='agentdev_owner')\gexec
SELECT 'CREATE ROLE agentdev_api LOGIN' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname='agentdev_api')\gexec
ALTER ROLE agentdev_api PASSWORD '${pgpw}';
SELECT 'CREATE DATABASE agentdev OWNER agentdev_owner' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='agentdev')\gexec
ALTER DATABASE agentdev OWNER TO agentdev_owner;
SQL
{ echo 'REASSIGN OWNED BY agentdev_api TO agentdev_owner;'   # objects an earlier API version created itself
  echo 'SET ROLE agentdev_owner;'
  cat "${SPIKE_DIR}/images/api/schema.sql"
  echo "UPDATE security_settings SET retention_days = ${retention};"
  echo 'RESET ROLE;'
  cat "${SPIKE_DIR}/images/api/grants.sql"
} | k -n "${PLATFORM_NS}" exec -i platform-db-0 -- psql -U postgres -d agentdev -v ON_ERROR_STOP=1 -q >/dev/null
printf 'DATABASE_URL=postgres://agentdev_api:%s@platform-db:5432/agentdev?sslmode=disable\n' "${pgpw}" \
  | k -n "${PLATFORM_NS}" create secret generic agentdev-api-db --from-env-file=/dev/stdin --dry-run=client -o yaml \
  | k apply --server-side --force-conflicts --field-manager=agentdev -f - >/dev/null
ok "database agentdev"
# SIEM outputs (optional): SECURITY_WEBHOOK_*, SECURITY_SYSLOG_*, SECURITY_OTLP_* (images/api/outputs.go) in
# .secrets/platform/siem-webhook. Same keys as the GKE path (infra/ansible deploy.yml; table in docs/admin.md).
if [[ -s "${S}/siem-webhook" ]]; then
  k -n "${PLATFORM_NS}" create secret generic agentdev-api-siem --from-env-file="${S}/siem-webhook" --dry-run=client -o yaml \
    | k apply --server-side --force-conflicts --field-manager=agentdev -f - >/dev/null
  ok "security outputs configured"
fi

log "Forge account for the API (#100)"
# The API reads each user's permission on the agent repositories before a change. Gitea shows another user's
# permission only to a repository admin (or site admin), so the account `agentdev-api` is admin collaborator of the
# Gitea agent repositories, and its token can only read (read:repository,read:user). Idempotent: an existing token
# that still works is kept. The token goes from the Gitea CLI to the Secret through a pipe, never to argv or stdout.
# GKE/Ansible: manual step in docs/admin.md ("Give the API access to the forge").
if k -n "${FORGE_NS}" get deploy gitea >/dev/null 2>&1; then
  gitea_cli() { k -n "${FORGE_NS}" exec deploy/gitea -- gitea "$@" 2>/dev/null; }
  # shellcheck source=lib-forge.sh
  source "${SPIKE_DIR}/lib-forge.sh"
  api_user=agentdev-api
  forge_mkuser "${api_user}" "$(openssl rand -hex 12)Aa1!" || die "cannot create forge user ${api_user}"
  repos=$( { echo "${REPO}"; k -n "${AGENTS_NS}" get agents -o json 2>/dev/null \
    | jq -r '.items[] | select((.spec.forge // "gitea") == "gitea") | .spec.repo'; } | sort -u)
  for r in ${repos}; do
    ga PUT "/repos/${r}/collaborators/${api_user}" '{"permission":"admin"}' >/dev/null \
      || die "cannot make ${api_user} admin of ${r}"
  done
  have=$(k -n "${PLATFORM_NS}" get secret agentdev-api-forge -o jsonpath='{.data.FORGE_API_TOKEN}' 2>/dev/null | base64 -d || true)
  if [[ -n "${have}" ]] && [[ "$(printf 'Authorization: token %s\n' "${have}" \
       | curl -fsS -H @- "${FORGE_HOST_URL}/api/v1/user" 2>/dev/null | jq -r .login)" == "${api_user}" ]]; then
    ok "token in Secret agentdev-api-forge still valid"
  else
    have=$(gitea_cli admin user generate-access-token --username "${api_user}" --token-name "agentdev-api-$(date +%s)" \
      --scopes read:repository,read:user --raw | tr -d '\r\n')
    [[ -n "${have}" ]] || die "cannot create a token for ${api_user}"
    printf 'FORGE_API_TOKEN=%s\n' "${have}" \
      | k -n "${PLATFORM_NS}" create secret generic agentdev-api-forge --from-env-file=/dev/stdin --dry-run=client -o yaml \
      | k apply --server-side --force-conflicts --field-manager=agentdev -f - >/dev/null
    ok "new token in Secret agentdev-api-forge"
  fi
  # The reuse check above reads the login only, not the scopes: a wider token put into the Secret by hand is kept.
  # Stale tokens: delete every other agentdev-api* token of the account, so only the one in the Secret stays valid
  # (sudo: the owner acts as agentdev-api; ids and names only, the token value is never listed by Gitea).
  last8=${have: -8}   # Gitea lists token_last_eight, never the value: match the token in the Secret by it
  n=0; for id in $(ga GET "/users/${api_user}/tokens?sudo=${api_user}" \
      | jq -r --arg k "${last8}" '.[] | select((.name | startswith("agentdev-api")) and .token_last_eight != $k) | .id'); do
    ga DELETE "/users/${api_user}/tokens/${id}?sudo=${api_user}" >/dev/null && n=$((n+1))
  done
  ok "deleted ${n} stale agentdev-api token(s)"
  unset have
  ok "${api_user}: admin of $(tr '\n' ' ' <<<"${repos}")"
else ok "no Gitea in ${FORGE_NS}: skipped (see docs/admin.md)"; fi

log "GitHub permission checks (#87)"
# FORGE_KIND=github: the API checks GitHub permissions and decides approvals with the same App as the agents
# (read-only installation tokens). The key goes from the file to the Secret, never to argv or stdout.
gh_app="${SPIKE_DIR}/.secrets/github-app"
if [[ "${FORGE_KIND}" == github ]]; then
  [[ -s "${gh_app}/app-id" && -s "${gh_app}/private-key.pem" ]] \
    || die "FORGE_KIND=github: register the App first (scripts/github-app.sh, docs/admin.md)"
  k -n "${PLATFORM_NS}" create secret generic agentdev-api-github --from-file=GITHUB_APP_ID="${gh_app}/app-id" \
      --from-file=GITHUB_APP_PRIVATE_KEY="${gh_app}/private-key.pem" --dry-run=client -o yaml \
    | k apply --server-side --force-conflicts --field-manager=agentdev -f - >/dev/null
  ok "Secret agentdev-api-github (App $(cat "${gh_app}/app-id"))"
else
  k -n "${PLATFORM_NS}" delete secret agentdev-api-github --ignore-not-found >/dev/null
  ok "FORGE_KIND=${FORGE_KIND}: no GitHub credential for the API"
fi

log "Deploy (${PLATFORM_NS})"
disc="http://authentik-server.${PLATFORM_NS}.svc.cluster.local/application/o/agentdev/"   # in-cluster, for keys
PLATFORM_NS="${PLATFORM_NS}" AGENTS_NS="${AGENTS_NS}" API_IMAGE="${API_IMAGE}" OIDC_ISSUER="${API_OIDC_ISSUER}" \
  OIDC_DISCOVERY_URL="${disc}" OIDC_AUDIENCE=agentdev APPROVALS="${APPROVALS:-false}" APPROVAL_DENY_ACCOUNTS="${APPROVAL_DENY_ACCOUNTS:-}" FORGE_URL="http://gitea.${FORGE_NS}.svc.cluster.local:3000" GITHUB_URL="${GITHUB_URL:-https://github.com}" \
  USAGE_FIVE_HOUR="${USAGE_FIVE_HOUR:-80}" USAGE_WEEKLY="${USAGE_WEEKLY:-90}" envsubst < "${SPIKE_DIR}/manifests/api.yaml.tmpl" | k apply -f - >/dev/null
# restart: local images keep their tag, and the token Secret may have changed (env is read at start only). The
# annotation records the Secret's hash (not its value), so a changed token is visible in the pod template too.
fh=$( { k -n "${PLATFORM_NS}" get secret agentdev-api-forge -o jsonpath='{.data}' 2>/dev/null
       k -n "${PLATFORM_NS}" get secret agentdev-api-github -o jsonpath='{.data}' 2>/dev/null; } | shasum -a 256 | cut -c1-16)
k -n "${PLATFORM_NS}" patch deploy/agentdev-api --type merge \
  -p "{\"spec\":{\"template\":{\"metadata\":{\"annotations\":{\"agentdev/forge-token-hash\":\"${fh}\"}}}}}" >/dev/null
k -n "${PLATFORM_NS}" rollout restart deploy/agentdev-api >/dev/null
k -n "${PLATFORM_NS}" rollout status deploy/agentdev-api --timeout=180s >/dev/null || die "agentdev-api did not become ready (kubectl -n ${PLATFORM_NS} logs deploy/agentdev-api)"
ok "agentdev-api ready"
record "agentdev API up in ${PLATFORM_NS} (issuer ${API_OIDC_ISSUER})"
echo "  API: ${KUBECTL} -n ${PLATFORM_NS} port-forward svc/agentdev-api 8088:8080  ->  http://localhost:8088/api/agents"

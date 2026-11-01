#!/usr/bin/env bash
# agentdev API (#46): Go service behind authentik OIDC, in ${PLATFORM_NS}. After 09.
#   ./run.sh 10      build (kind) and deploy, idempotent
# The OIDC application `agentdev` comes from an authentik blueprint (09). OIDC_ISSUER must be the URL users reach
# authentik through, because authentik puts that URL into tokens (locally: the https port-forward).
source "$(dirname "$0")/lib.sh"
need envsubst; need openssl
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

log "Deploy (${PLATFORM_NS})"
disc="http://authentik-server.${PLATFORM_NS}.svc.cluster.local/application/o/agentdev/"   # in-cluster, for keys
PLATFORM_NS="${PLATFORM_NS}" AGENTS_NS="${AGENTS_NS}" API_IMAGE="${API_IMAGE}" OIDC_ISSUER="${API_OIDC_ISSUER}" \
  OIDC_DISCOVERY_URL="${disc}" OIDC_AUDIENCE=agentdev FORGE_URL="http://gitea.${FORGE_NS}.svc.cluster.local:3000" \
  USAGE_FIVE_HOUR="${USAGE_FIVE_HOUR:-80}" USAGE_WEEKLY="${USAGE_WEEKLY:-90}" envsubst < "${SPIKE_DIR}/manifests/api.yaml.tmpl" | k apply -f - >/dev/null
k -n "${PLATFORM_NS}" rollout restart deploy/agentdev-api >/dev/null   # local images keep their tag
k -n "${PLATFORM_NS}" rollout status deploy/agentdev-api --timeout=180s >/dev/null || die "agentdev-api did not become ready (kubectl -n ${PLATFORM_NS} logs deploy/agentdev-api)"
ok "agentdev-api ready"
record "agentdev API up in ${PLATFORM_NS} (issuer ${API_OIDC_ISSUER})"
echo "  API: ${KUBECTL} -n ${PLATFORM_NS} port-forward svc/agentdev-api 8088:8080  ->  http://localhost:8088/api/agents"

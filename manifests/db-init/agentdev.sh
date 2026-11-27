#!/bin/sh
# db-init for the database agentdev (#382): roles, database, schema.sql as agentdev_owner, settings, grants.sql and
# the schema version the API waits for. Idempotent. Passwords come from env (Secrets platform-db and agentdev-api-db)
# and reach psql through \getenv only: never argv, never the log.
set -eu
export PGHOST=platform-db PGPORT=5432 PGUSER=postgres PGPASSWORD="${POSTGRES_PASSWORD}" PGOPTIONS="-c client_min_messages=warning"
pw="${DATABASE_URL#*://agentdev_api:}"; pw="${pw%%@*}"
case "${pw}" in *[!0-9a-f]*|"") echo "agentdev-api-db: DATABASE_URL must hold a hex password for agentdev_api" >&2; exit 1 ;; esac
[ "${#pw}" -ge 32 ] || { echo "agentdev-api-db: the password is shorter than 32 hex characters" >&2; exit 1; }
export AGENTDEV_API_PASSWORD="${pw}"
case "${SCHEMA_VERSION:-}" in *[!0-9a-f]*|"") echo "SCHEMA_VERSION must be hex" >&2; exit 1 ;; esac
unset DATABASE_URL pw
i=0; until pg_isready -q; do i=$((i+1)); [ "${i}" -lt 150 ] || { echo "platform-db not ready" >&2; exit 1; }; sleep 2; done
cd /db-init
psql -X -q -v ON_ERROR_STOP=1 -f agentdev-roles.sql
psql -X -q -v ON_ERROR_STOP=1 -v schema_version="${SCHEMA_VERSION}" -d agentdev -f agentdev-schema.sql
echo "db-init agentdev: done"

#!/bin/sh
# db-init for the database authentik (#382): the login and the database. Idempotent. The password comes from env
# (Secret authentik, AUTHENTIK_POSTGRESQL__PASSWORD) and reaches psql through \getenv only.
set -eu
export PGHOST=platform-db PGPORT=5432 PGUSER=postgres PGPASSWORD="${POSTGRES_PASSWORD}" PGOPTIONS="-c client_min_messages=warning"
case "${AUTHENTIK_DB_PASSWORD}" in *[!0-9a-f]*|"") echo "authentik: AUTHENTIK_POSTGRESQL__PASSWORD must be hex" >&2; exit 1 ;; esac
i=0; until pg_isready -q; do i=$((i+1)); [ "${i}" -lt 150 ] || { echo "platform-db not ready" >&2; exit 1; }; sleep 2; done
psql -X -q -v ON_ERROR_STOP=1 -f /db-init/authentik-roles.sql
echo "db-init authentik: done"

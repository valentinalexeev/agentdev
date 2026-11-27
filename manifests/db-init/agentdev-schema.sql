-- Schema (as agentdev_owner), settings (retention), grants, then the schema version last: the API starts serving
-- only when schema_version holds the version of its own schema.sql and grants.sql (images/api/events.go).
REASSIGN OWNED BY agentdev_api TO agentdev_owner;
SET ROLE agentdev_owner;
\i schema.sql
\i settings.sql
RESET ROLE;
\i grants.sql
SET ROLE agentdev_owner;
CREATE TABLE IF NOT EXISTS schema_version (id int PRIMARY KEY CHECK (id = 1), version text NOT NULL, applied timestamptz NOT NULL DEFAULT now());
INSERT INTO schema_version (id, version) VALUES (1, :'schema_version')
  ON CONFLICT (id) DO UPDATE SET version = EXCLUDED.version, applied = now();
RESET ROLE;
GRANT SELECT ON schema_version TO agentdev_api;

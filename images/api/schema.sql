-- security_events (#70). Idempotent; applied by 10-api.sh as agentdev_owner (or by the API with DATABASE_MIGRATE=true).
-- The API login gets INSERT and SELECT on the table and EXECUTE on security_events_prune(), nothing else (10-api.sh).
CREATE TABLE IF NOT EXISTS security_events (
	id         bigserial PRIMARY KEY,
	schema     text        NOT NULL,
	time       timestamptz NOT NULL,
	agent      text        NOT NULL,
	job        text        NOT NULL DEFAULT '',
	source     text        NOT NULL,
	untrusted  boolean     NOT NULL DEFAULT false,
	rule       text        NOT NULL,
	severity   text        NOT NULL CHECK (severity IN ('low','medium','high')),
	action     text        NOT NULL,
	pod        text        NOT NULL DEFAULT '',
	container  text        NOT NULL DEFAULT '',
	hash       text        NOT NULL UNIQUE,
	stored_at  timestamptz NOT NULL DEFAULT now()
);
ALTER TABLE security_events ADD COLUMN IF NOT EXISTS untrusted boolean NOT NULL DEFAULT false;
-- per-agent hash chain (#71): prev = chain of the agent's previous row, chain = sha256(prev || '\n' || canonical row)
ALTER TABLE security_events ADD COLUMN IF NOT EXISTS prev  text NOT NULL DEFAULT '';
ALTER TABLE security_events ADD COLUMN IF NOT EXISTS chain text NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS security_events_agent_id ON security_events (agent, id DESC);
CREATE INDEX IF NOT EXISTS security_events_time ON security_events (time);

-- retention in days, one row; written by the owner (10-api.sh from SECURITY_DB_RETENTION_DAYS), never by the API
CREATE TABLE IF NOT EXISTS security_settings (
	id             boolean PRIMARY KEY DEFAULT true CHECK (id),
	retention_days int     NOT NULL DEFAULT 365 CHECK (retention_days >= 1)
);
INSERT INTO security_settings DEFAULT VALUES ON CONFLICT DO NOTHING;

-- append-only: no UPDATE ever; DELETE only for rows past the retention
CREATE OR REPLACE FUNCTION security_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	IF TG_OP = 'UPDATE' THEN
		RAISE EXCEPTION 'security_events is append-only';
	END IF;
	IF OLD.time > now() - (SELECT retention_days FROM security_settings) * interval '1 day' THEN
		RAISE EXCEPTION 'security_events: row is inside the retention period';
	END IF;
	RETURN OLD;
END $$;
DROP TRIGGER IF EXISTS security_events_append_only ON security_events;
CREATE TRIGGER security_events_append_only BEFORE UPDATE OR DELETE ON security_events
	FOR EACH ROW EXECUTE FUNCTION security_events_append_only();

-- the only way the API removes rows: runs with the owner's rights, takes no argument, returns the number deleted
CREATE OR REPLACE FUNCTION security_events_prune() RETURNS bigint LANGUAGE sql SECURITY DEFINER
SET search_path = pg_catalog, public AS $$
	WITH d AS (DELETE FROM public.security_events
		WHERE time < now() - (SELECT retention_days FROM public.security_settings) * interval '1 day' RETURNING 1)
	SELECT count(*) FROM d
$$;
REVOKE ALL ON FUNCTION security_events_prune() FROM PUBLIC;

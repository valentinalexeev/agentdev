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

-- api_audit (#91): one row per AGENTDEV_AUDIT line of the write API (write.go). Same rules as security_events:
-- append-only, the API login has INSERT and SELECT only, rows go only after the retention in security_settings.
CREATE TABLE IF NOT EXISTS api_audit (
	id         bigserial PRIMARY KEY,
	time       timestamptz NOT NULL,
	action     text        NOT NULL,
	agent      text        NOT NULL DEFAULT '',
	result     text        NOT NULL,
	username   text        NOT NULL DEFAULT '',
	entry      jsonb       NOT NULL,
	stored_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS api_audit_agent_id ON api_audit (agent, id DESC);
CREATE INDEX IF NOT EXISTS api_audit_time ON api_audit (time);

CREATE OR REPLACE FUNCTION api_audit_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	IF TG_OP = 'UPDATE' THEN
		RAISE EXCEPTION 'api_audit is append-only';
	END IF;
	IF OLD.time > now() - (SELECT retention_days FROM security_settings) * interval '1 day' THEN
		RAISE EXCEPTION 'api_audit: row is inside the retention period';
	END IF;
	RETURN OLD;
END $$;
DROP TRIGGER IF EXISTS api_audit_append_only ON api_audit;
CREATE TRIGGER api_audit_append_only BEFORE UPDATE OR DELETE ON api_audit
	FOR EACH ROW EXECUTE FUNCTION api_audit_append_only();

CREATE OR REPLACE FUNCTION api_audit_prune() RETURNS bigint LANGUAGE sql SECURITY DEFINER
SET search_path = pg_catalog, public AS $$
	WITH d AS (DELETE FROM public.api_audit
		WHERE time < now() - (SELECT retention_days FROM public.security_settings) * interval '1 day' RETURNING 1)
	SELECT count(*) FROM d
$$;
REVOKE ALL ON FUNCTION api_audit_prune() FROM PUBLIC;

-- chat sessions (#346): one row per console chat session, and its transcript. Kept chat_settings.retention_days
-- (CHAT_RETENTION_DAYS, default 30), readable by people with read access to the repository (checked in the API).
-- chat_events is append-only; chat_sessions may be updated (end, last event) but rows go only through chat_prune().
CREATE TABLE IF NOT EXISTS chat_settings (
	id             boolean PRIMARY KEY DEFAULT true CHECK (id),
	retention_days int     NOT NULL DEFAULT 30 CHECK (retention_days >= 1)
);
INSERT INTO chat_settings DEFAULT VALUES ON CONFLICT DO NOTHING;

CREATE TABLE IF NOT EXISTS chat_sessions (
	id            text        PRIMARY KEY,
	agent         text        NOT NULL,
	repo          text        NOT NULL,
	forge         text        NOT NULL DEFAULT 'gitea',
	started_by    text        NOT NULL,
	started_at    timestamptz NOT NULL,
	ended_at      timestamptz,
	ended_reason  text        NOT NULL DEFAULT '',
	last_seq      bigint      NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS chat_sessions_started ON chat_sessions (started_at DESC);

CREATE TABLE IF NOT EXISTS chat_events (
	id        bigserial   PRIMARY KEY,
	session   text        NOT NULL REFERENCES chat_sessions (id) ON DELETE CASCADE,
	seq       bigint      NOT NULL,      -- order in the session server; 0 = written by the API
	time      timestamptz NOT NULL,
	type      text        NOT NULL,
	author    text        NOT NULL DEFAULT '',
	data      jsonb       NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS chat_events_seq ON chat_events (session, seq) WHERE seq > 0;
CREATE INDEX IF NOT EXISTS chat_events_session_id ON chat_events (session, id);

CREATE OR REPLACE FUNCTION chat_events_append_only() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
	IF TG_OP = 'UPDATE' THEN
		RAISE EXCEPTION 'chat_events is append-only';
	END IF;
	IF OLD.time > now() - (SELECT retention_days FROM chat_settings) * interval '1 day' THEN
		RAISE EXCEPTION 'chat_events: row is inside the retention period';
	END IF;
	RETURN OLD;
END $$;
DROP TRIGGER IF EXISTS chat_events_append_only ON chat_events;
CREATE TRIGGER chat_events_append_only BEFORE UPDATE OR DELETE ON chat_events
	FOR EACH ROW EXECUTE FUNCTION chat_events_append_only();

-- removes sessions that started one day before the retention (a session lasts 8 h at most, so all its events are
-- past the retention too); their events go with them (ON DELETE CASCADE)
CREATE OR REPLACE FUNCTION chat_prune() RETURNS bigint LANGUAGE sql SECURITY DEFINER
SET search_path = pg_catalog, public AS $$
	WITH d AS (DELETE FROM public.chat_sessions
		WHERE started_at < now() - ((SELECT retention_days FROM public.chat_settings) + 1) * interval '1 day' RETURNING 1)
	SELECT count(*) FROM d
$$;
REVOKE ALL ON FUNCTION chat_prune() FROM PUBLIC;

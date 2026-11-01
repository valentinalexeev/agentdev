-- Role separation for security_events (#70). Run as a superuser after schema.sql was applied as agentdev_owner.
-- agentdev_owner (NOLOGIN) owns the objects; agentdev_api (the API login) may only INSERT and SELECT rows and call
-- security_events_prune(). It cannot UPDATE, DELETE or TRUNCATE, drop the trigger or change the retention.
REVOKE ALL ON security_events, security_settings, api_audit FROM agentdev_api;
GRANT SELECT, INSERT ON security_events TO agentdev_api;
GRANT USAGE ON SEQUENCE security_events_id_seq TO agentdev_api;
GRANT EXECUTE ON FUNCTION security_events_prune() TO agentdev_api;
-- api_audit (#91): the same rights
GRANT SELECT, INSERT ON api_audit TO agentdev_api;
GRANT USAGE ON SEQUENCE api_audit_id_seq TO agentdev_api;
GRANT EXECUTE ON FUNCTION api_audit_prune() TO agentdev_api;
-- Only the owner may create objects in public (the default from PostgreSQL 15, explicit for older versions), so
-- nobody can plant a shadowing object in the SECURITY DEFINER function's search_path (pg_catalog, public).
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT CREATE ON SCHEMA public TO agentdev_owner;

-- Owner/API split (docs/security.md, #70): agentdev_owner (NOLOGIN) owns the database and the objects, the login
-- agentdev_api may only insert, read and call the prune functions (grants.sql). Run as postgres in the db-init Job.
\getenv api_pw AGENTDEV_API_PASSWORD
SELECT 'CREATE ROLE agentdev_owner NOLOGIN' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname='agentdev_owner')\gexec
SELECT 'CREATE ROLE agentdev_api LOGIN' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname='agentdev_api')\gexec
ALTER ROLE agentdev_api LOGIN PASSWORD :'api_pw';
\unset api_pw
SELECT 'CREATE DATABASE agentdev OWNER agentdev_owner' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='agentdev')\gexec
ALTER DATABASE agentdev OWNER TO agentdev_owner;

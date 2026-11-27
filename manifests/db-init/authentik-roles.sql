-- The login and the database authentik (was the exec step of the role authentik). Run as postgres in the db-init Job.
\getenv ak_pw AUTHENTIK_DB_PASSWORD
SELECT 'CREATE ROLE authentik LOGIN' WHERE NOT EXISTS (SELECT FROM pg_roles WHERE rolname='authentik')\gexec
ALTER ROLE authentik LOGIN PASSWORD :'ak_pw';
\unset ak_pw
SELECT 'CREATE DATABASE authentik OWNER authentik' WHERE NOT EXISTS (SELECT FROM pg_database WHERE datname='authentik')\gexec

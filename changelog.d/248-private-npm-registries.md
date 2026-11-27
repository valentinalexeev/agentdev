---
issue: 248
type: feature
section: Added
status: done
title: Private npm registries per project through the credential broker
components: [operator, credproxy]
pr: 282
---
## Summary

`ProjectSecret` mode broker gets `spec.broker.npmScope` (for example `@acme`). The operator writes each agent in the
secret's scope an npm config (key `<agent>.npmrc` of `agentdev-guides`, mounted at `/etc/agentdev/guide/npmrc`,
`NPM_CONFIG_GLOBALCONFIG`) that maps the scope to the credential broker with `${AGENTDEV_BROKER_TOKEN}`, the agent's
projected broker token (#202). No registry token in the agent pod; an agent of another project gets 403. Re-run the
operator (new CRD field). Docs: `docs/admin.md` "Add a private npm registry for a project"; risk R131.

## Description

Needs the credential broker (#202). The agent sets `AGENTDEV_BROKER_TOKEN=$(cat "$AGENTDEV_BROKER_TOKEN_FILE")` for
the npm call; the broker checks the token with TokenReview. Private packages are not stored in the shared registry
cache (#203). kind e2e: `scripts/e2e-private-registry.sh`.

# The operator writes its proxy key Secrets with the supported server-side apply API (#250)

Released in 1.3.0-rc.1 (2026-11-25). Type: task. Section: Changed. Components: operator, credproxy.

## Summary

The operator writes `agent-<name>-proxy` and the setup proxy key Secret with `client.Client.Apply` and an apply
configuration. Before, it used `Patch` with the deprecated `client.Apply` patch type. The field owner
(`agentdev-operator`), forced ownership and the written fields are the same. No action is needed.

## Description

A full staticcheck run (go1.27.4) on `images/operator` and `images/credproxy` is now clean. Also fixed: an unused
assignment in `executor_egress.go` (no behaviour change), lower-case error strings in `mcpproject.go` and
`credcheck.go`, and `http.StatusForbidden` in a credproxy test.

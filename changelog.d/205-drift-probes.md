---
issue: 205
type: feature
section: Added
status: done
title: Drift reports and the probe catalogue (server_check)
components: [operator, manifests, ansible, scripts]
pr: 280
---
## Summary

`Executor.spec.drift` runs the read-only plan on the protected branch head (stage `drift`, no apply), keeps one forge
issue open while drift stays and writes the redacted report to ConfigMap `agentdev-drift`. `Executor.spec.probes`
is a catalogue of read-only server checks; obs-mcp tools `server_checks`, `server_check` and `server_report` reach
them through the operator, with rate limits, checked arguments, an Approval for `sensitive` checks and output fenced
as untrusted. Docs: admin.md "Find drift on the servers", "Add a server check"; risk R131.

## Description

The probe server listens on `:8090` (Service `agentdev-probe`, NetworkPolicy: obs-mcp only) and needs the random
token Secret `agentdev-probe-token` (platform namespace, made by the platform role). Each call is an `ExecutorRun`
stage `probe` and a SecurityEvent (`executor.probe`, `executor.probe-refused`) and connects with a 5-minute
certificate of principal `agentdev-probe` with `force-command`. `scripts/ssh-enrol.sh` writes the server's own
allow-list `/etc/agentdev/probes` and the `agentdev-probe` runner (timeout 20 s). Ansible: executor vars `drift`,
`probes`; `obs_mcp.server_checks`; `probes.rate_*` (GKE), `operator_probes` (kind). kind e2e:
`scripts/e2e-ssh.sh probes`.

## Upgrade

Apply the snippet of `scripts/ssh-enrol.sh` again on every enrolled server: the principals file of
`agentdev-check` now forces the `agentdev-probe` command.

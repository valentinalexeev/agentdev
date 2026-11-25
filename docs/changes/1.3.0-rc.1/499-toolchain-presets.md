# Per-agent toolchain presets (go, rust) installed read-only before the agent starts (#499)

Released in 1.3.0-rc.1 (2026-11-25). Type: feature. Section: Added. Components: operator, worker.

## Summary

`Agent` has `spec.toolchain`: preset versions (`go`, `rust`) from a catalogue in the operator with pinned SHA-256
checksums, extra `path` entries and an optional `refresh`. An init container `toolchain` installs them into
`/state/toolchain/<key>` on the agent's volume, and the agent gets them on `PATH`, read-only. `status.toolchain` shows
the last install.

## Description

ADR 0010 D2 (presets) and D3 layer 1. The init container runs the worker image as UID 1000 with a read-only root,
an emptyDir `/tmp`, no forge or Claude credential and only the setup proxy credential `agent-<name>-setup-proxy`
(copied by the operator from `egress-setup-proxy`). Every other container mounts `/state/toolchain` read-only. The
init container hashes the installed tree on every start and compares it with the last reported hash (ConfigMap
`agentdev-toolchains`); a mismatch is a high-severity security event and a fresh install. A failed download, checksum
or `verify` keeps the pod in `Init` (fail closed). The preset's allow entries are added to `CLAUDE_TOOLS_EXTRA`. The
refresh epoch has a per-agent offset, and the new install waits for idle (drain). An agent without the field is
unchanged.

Not yet done (next PR): the Squid ACL and the playbook for `egress-setup-proxy`, the chat Job admission policy for the
`toolchain` init container, Ansible settings and the admin docs.

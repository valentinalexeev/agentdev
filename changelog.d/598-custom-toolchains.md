---
issue: 598
type: feature
section: Added
status: done
breaking: false
title: Custom toolchains defined by the deployment
components: [operator, worker, ansible]
---
## Summary

A deployment can define its own toolchains in the instance config (Ansible `toolchains`, on `kind`
`operator_toolchains`), without an agentdev release. An agent names them in `spec.toolchain.custom`
(`agents[].toolchain.custom`), alone or next to presets. Each toolchain has pinned downloads (`tar`, `file` or `deb`,
a SHA-256 per architecture), `path`, `verify`, `env` and `allow` entries. The init container `toolchain` installs them
with the same guarantees as presets. The download hosts must be in `setup_egress_hosts`. Docs: `docs/admin.md`,
"Give an agent a custom toolchain".

## Description

The presets of the operator (`go`, `rust`) needed an agentdev release for every new tool. Custom toolchains are
declarative: there are no install commands, and the init container uses its own fixed steps for each kind. A `deb`
download is unpacked with `dpkg-deb -x`, without maintainer scripts. Unpacking and `verify` run without the setup
proxy credential.

The operator reads `TOOLCHAINS` (JSON) and `SETUP_EGRESS_HOSTS` from its environment. It does not start when
`TOOLCHAINS` is malformed. A custom toolchain with an error (a host outside the setup egress list, a bad checksum
format, a path out of the toolchain, a reserved variable) makes each agent that names it `InvalidToolchain`. A
checksum mismatch at install time gives `StepFailed`, and `status.toolchain` names the custom toolchains like
presets. A custom toolchain never overrides a variable that agentdev or a preset sets, and a symbolic link out of the
installed tree is refused.

Squid now allows the setup client before the forge-host deny, so a custom toolchain can download from `github.com`.
The agent container is refused both lists as before. The worker image adds `xz-utils` for `.tar.xz` archives.
`spec.toolchain.presets` is now optional; the CRD needs at least one preset or custom toolchain. Agents with presets
only keep their toolchain key, so their pods do not install again on upgrade. Security: `docs/security.md` R137.

Ansible passes `TOOLCHAINS` as a double-quoted YAML scalar, so quotes, backslashes and `$` in a value reach the
operator unchanged (`tests/operator_render.yml`). With no custom toolchain in use, the pod template of every agent is
unchanged (`TestCustomToolchainDefaultUnchanged`).

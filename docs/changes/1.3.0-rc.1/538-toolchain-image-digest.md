# Toolchain installs follow the worker image digest, so a re-pushed tag installs again (#538)

Released in 1.3.0-rc.1 (2026-11-25). Type: fix. Section: Fixed. Components: operator, api. PR: #551.

## Summary

The toolchain key now follows the worker image digest (ADR 0010 D3). The operator reads the digest from the `imageID`
of the `state-init` init container of the agent pod. When the same tag (for example `:latest`) later runs with another
digest, the pod is replaced after drain and the toolchain is installed again under a new key. With a toolchain the
agent and `toolchain` containers run the worker image pinned to its digest; the chat Job policy admits that form.
The digest only moves forward. Agents without `spec.toolchain` keep their pod template. See docs/admin.md. Re-run:
the playbook or `./run.sh 12`.

## Description

The operator has no registry access (no egress, no pull secret, and kind loads images without a registry). So the
kubelet's `imageID` of `state-init` (which keeps the tag) is the source. The StatefulSet keeps
`agentdev/toolchain-image-base` (`<image ref> <first digest>`), `agentdev/toolchain-image-digest` (the pin) and
`agentdev/toolchain-image-seen` (the digests seen; a seen digest never comes back). Only a digest other than the base
goes into `TOOLCHAIN_SPEC`. Detection needs a pod start on a node that has the new image; with pull policy `Never`
only an image load does it, and the containers keep the tag. A missing pod, status or repository digest changes
nothing.

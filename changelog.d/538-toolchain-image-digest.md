---
issue: 538
type: fix
section: Fixed
status: done
breaking: false
title: Toolchain installs follow the worker image digest, so a re-pushed tag installs again
components: [operator]
---
## Summary

The toolchain key now follows the worker image digest (ADR 0010 D3). The operator reads the digest from the `imageID`
of the `toolchain` init container of the agent pod. When the same tag (for example `:latest`) later runs with another
digest, the pod is replaced after drain and the toolchain is installed again under a new key. Agents without
`spec.toolchain`, and agents whose tag did not move, keep their pod template. Re-run: the playbook or `./run.sh 12`.

## Description

The operator has no registry access (no egress, no pull secret, and kind loads images without a registry). So the
kubelet's `status.initContainerStatuses[].imageID` is the source: it is the image that really ran the install. The
StatefulSet keeps two annotations: `agentdev/toolchain-image-base` (`<image ref> <first digest>`) and
`agentdev/toolchain-image-digest` (the latest digest). Only a digest other than the base goes into `TOOLCHAIN_SPEC`, so
an upgrade does not roll the pods twice. The change is seen when a pod of the new digest starts, for example after a
node pulls the moved tag. A missing pod, status or digest changes nothing (the key stays the tag's, as before).

# Ephemeral-storage limits for agent pods, the toolchain init container and chat sessions (#251)

Released in 1.3.0-rc.1 (2026-11-25). Type: feature. Section: Added. Components: operator, api, ansible. PR: #265.

## Summary

Agent pods have `ephemeral-storage` limits (#251 part 3): agent container 8Gi (request 1Gi), forge sidecar 5Gi with
its git mirror `emptyDir` capped at 4Gi, `state-init` 64Mi, the `toolchain` init container 3Gi with its `/tmp` at 2Gi.
A chat session's `/state` `emptyDir` is capped at the agent container's limit. The Go preset sets
`GOCACHE=/state/cache/go/build`, so builds use the agent volume. Settings: deploy.yml `agent_ephemeral_*`,
`forge_*_limit`; kind `operator_agent_ephemeral_*`, `operator_forge_*_limit`. Re-run the playbook (kind: `--tags operator`).

## Description

A pod past a limit is evicted by the kubelet and the StatefulSet recreates it. The work tree, the toolchain and the
Go and cargo caches are on the state volume (a PVC), so they do not count. An empty or invalid setting gives the
default. In a chat session `/state` is node disk, so the toolchain install and the caches of the session count; a
larger session needs a larger `AGENT_EPHEMERAL_LIMIT`.

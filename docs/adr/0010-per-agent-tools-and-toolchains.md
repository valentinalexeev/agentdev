# ADR 0010: per-agent tools and toolchains, with a toolchain cache

- **Status:** proposed
- **Date:** 2026-11-24
- **Issues:** #496 (tool allow-list), #499 (toolchain setup), #526 (1.3.0 tracking), #203 (registry caches)
- **Related:** ADR 0003 D3 (caching proxies), docs/threat-model.md V6

## Context

Every agent runs the same worker image (Node, Python, git; no Go, Rust, C compiler or `make`) and the same
`CLAUDE_TOOLS` allow-list. An agent that maintains agentdev itself cannot run `go test`, a Rust repository cannot
run `cargo`, and `npm ci` is not allowed. A custom `worker_image` would apply to every agent and chat session and
would have to be rebuilt on every upgrade.

Installing a toolchain on each pod start is slow: Go is about 70 MB and Rust about 300 MB. Dependency downloads
(Go modules, crates, npm) repeat on every new pod and for every agent.

## Decision

### D1. Tool allow-list per agent (#496)

```yaml
spec:
  tools:
    allow: ["Bash(go build:*)", "Bash(go test:*)", "Bash(cargo test:*)"]
```

- `allow` only **adds** to the default list. There is no `replace`, so the default list cannot be shrunk to nothing
  or quietly swapped.
- The operator passes the effective list as `CLAUDE_TOOLS`. Admin only. The change is audited like other agent
  patches. The console shows the effective list.
- A call matched only by an added entry is a low-severity security event, from the same trusted source as #506.
- A toolchain preset (D2) brings its own default `allow` entries (for example `Bash(go build:*)`), so the usual case
  needs no `tools` block.

### D2. Toolchains per agent: presets and free-form steps (#499)

```yaml
spec:
  toolchain:
    presets: {go: "1.27.4", rust: "1.85.0"}       # maintained by agentdev, checksums pinned
    steps:                                        # free form, admin only
      - name: protoc
        run: ["sh", "-c", "curl -fsSL https://github.com/.../protoc-29.zip -o /tmp/p.zip && unzip -d $TOOLCHAIN /tmp/p.zip"]
        verify: ["protoc", "--version"]
        hosts: ["github.com", "objects.githubusercontent.com"]
    path: ["$TOOLCHAIN/bin"]
    refresh: 168h
```

- **Presets** are defined in the operator: download URL per architecture, SHA-256, `verify` command, `PATH` entries,
  egress hosts and default `allow` entries. The first presets are `go` and `rust` (rustup with a pinned toolchain,
  `minimal` profile). Adding a preset is an agentdev release.
- **Free-form steps** run admin-written commands. They come only from the Agent object and `instance.yml`, never
  from the repository or issue text. Each step lists the `hosts` it needs. The operator adds them to that agent's
  setup egress only, not to the agent container.
- **No root.** Steps run as UID 1000 and install under `$TOOLCHAIN`. `apt-get` is not supported. A tool that only
  exists as a Debian package belongs in a later image variant, not in a step.
- **Fail closed.** The setup runs in an init container, before the agent container, with the egress proxy and
  without the forge or Claude credential. If a step or `verify` fails, the pod does not start. The failure is
  reported in `Agent.status.toolchain` (step, exit code, last lines), in the console and as a security event.
- `refresh` re-runs the steps on that interval. A change to the spec re-runs them at once.

### D3. Toolchain cache

Three layers. Each one is optional and falls back to the one below it.

1. **Per-agent install, persistent.** Installs go to `/state/toolchain/<key>` on the agent PVC. `key` is the
   SHA-256 of the canonical toolchain spec (presets with versions, steps, architecture, base image digest). The init
   container finds the directory and a `.complete` marker for the current key, then skips the work. A scale-to-zero
   wake costs nothing. Old keys are deleted after a successful install, so the PVC holds one toolchain.

2. **Shared toolchain store, per instance.** A content-addressed store of finished toolchain directories, keyed the
   same way. A new agent, a recreated PVC or a pod on another node downloads and unpacks the entry instead of running
   the steps.
   - **Build:** on a cache miss the operator runs a separate build Job for that key. It runs the same image and steps
     with the setup egress, no PVC shared with any agent, and no forge or Claude credential. It uploads one tarball
     through a single-use signed write URL from the operator. The agent pod waits for the entry, or builds locally
     only if the store is off.
   - **GKE:** a GCS bucket `<project>-toolchains`. Only the operator's Workload Identity can write to it. Agents get
     a short-lived signed read URL from the operator. No agent pod can write to the store.
   - **kind:** a small in-cluster object store with the same layout and signed URLs.
   - **Integrity:** the operator records the SHA-256 of each tarball when it is uploaded, and the init container
     checks it before unpacking. A modified tarball is refused and reported.
   - **Trust boundary:** an entry is built by the build Job, which has no credentials, shares no volume with an
     agent, and runs only admin-written steps and agentdev presets. Its key includes everything that defines its
     content. Two agents share an entry only when their toolchain specs are identical, so a repository or an agent
     cannot poison another agent's toolchain. Third-party install scripts inside steps remain a supply-chain risk
     (threat model V6). Presets reduce it with pinned checksums.
   - Presets can be **pre-built** when an agentdev release is published, so the first agent does not wait either.

3. **Dependency caches** (module downloads, not the toolchain itself), through ADR 0003 D3 / #203:
   - `GOPROXY`, `CARGO_REGISTRIES_*` / `sparse+` mirror and the npm registry point at the instance cache
     (Artifact Registry remote repositories on GKE; Athens, a crates mirror and Verdaccio on kind).
   - Per-agent `GOMODCACHE`, `CARGO_HOME` and the npm cache live on the PVC (`/state/cache/...`), so a wake does
     not re-download.
   - Build caches (`GOCACHE`, `target/`) stay per agent and are never shared, because they can be poisoned by
     the repository's own code.

**Expected effect:** the first agent with Go 1.27.4 downloads about 70 MB once for the instance. Every later agent
with the same spec unpacks a cached tarball in a few seconds, and a wake of an existing agent takes no time.

### D4. Order of work

1. D1 (#496), because the tools are useless without the allow-list and it is small.
2. D2 presets with D3 layer 1 (per-agent install).
3. D2 free-form steps.
4. D3 layer 2 (shared store), then layer 3 together with #203.

## Consequences

- New CRD fields `spec.tools` and `spec.toolchain`, plus `status.toolchain`. Existing agents without them behave as
  today, and their pods do not restart on upgrade.
- The operator gains a preset catalogue, an init container, a GCS bucket (Terraform) and signed URLs.
- Docs: admin.md "Give an agent a toolchain", security.md (setup egress, the cache trust boundary), threat-model V6.

## Alternatives considered

- **Custom worker image per agent.** Fast at run time, but arbitrary images break the base-image guarantees, and
  every image needs a rebuild on every agentdev upgrade.
- **Image variants (`worker-go`, `worker-rust`).** Fast and simple, but they multiply the image builds, and they do
  not cover free-form needs. They may still come later as presets backed by images.
- **Root steps with `apt-get`.** Flexible, but they weaken the pod security level. Rejected (owner, 2026-11-24).
- **Sharing `GOCACHE` and `target/` across agents.** Faster builds, but a repository can poison a shared build cache.
  Rejected.

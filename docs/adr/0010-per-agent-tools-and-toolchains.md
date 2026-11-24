# ADR 0010: per-agent tools and toolchains, with a toolchain cache

- **Status:** proposed
- **Date:** 2026-11-24
- **Issues:** #496 (tool allow-list), #499 (toolchain setup), #526 (1.3.0 tracking), #203 (registry caches)
- **Related:** ADR 0003 D3 (caching proxies; this ADR extends it for crates), ADR 0008 (security review),
  docs/security.md R10, R97, R101, docs/threat-model.md V6

## Context

Every agent runs the same worker image: Node, Python and git, but no Go, Rust, C compiler or `make`. The default
tool allow-list is in the worker (`images/worker/agent-loop.sh`, `CLAUDE_TOOLS="${CLAUDE_TOOLS:-...}"`), and
an instance can only replace it as a whole. The default already allows `go test`, `go build`, `cargo test` and
`make`, but the binaries are missing, so those calls fail. Some routine commands, such as `npm ci`, are not in the
default at all. A custom `worker_image` would apply to every agent and chat session and would have to be rebuilt
on every upgrade.

Installing a toolchain on each pod start is slow. Go 1.27 for linux/amd64 is about 70 MB to download and about
230 MB unpacked. A minimal Rust toolchain is about 80 MB to download and 300 MB or more unpacked. Dependency
downloads (Go modules, crates, npm) repeat on every new pod and for every agent.

## Decision

### D1. Tool allow-list per agent (#496)

```yaml
spec:
  tools:
    allow: ["Bash(npm ci:*)", "Bash(npm install:*)"]
```

- The operator sets `CLAUDE_TOOLS_EXTRA` from `spec.tools.allow` and from the presets' entries (D2). The worker
  appends it to `CLAUDE_TOOLS`. The instance default stays in the worker. There is no `replace`, so an agent's list
  cannot be shrunk to nothing or quietly swapped.
- Admin only. The change is audited like other agent patches. The console shows the added entries and the effective
  list.
- A call allowed only by an added entry is a low-severity security event, from the same trusted source as #506.

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

- **Presets** are defined in the operator. Each one has a download URL per architecture, a SHA-256, a `verify`
  command, `PATH` entries, egress hosts and `allow` entries. The first presets are `go` and `rust` (rustup with a
  pinned toolchain, `minimal` profile). Adding a preset is an agentdev release.
- **Free-form steps** run admin-written commands. They come only from the Agent object and `instance.yml`, never
  from the repository or issue text. Each step lists the `hosts` it needs.
- **The `toolchain` init container** runs before the agent container:
  - same image as the worker, as UID 1000, under gVisor, with the pod's security context;
  - no added capabilities, `readOnlyRootFilesystem`, and an emptyDir `/tmp`;
  - no forge or Claude credential.
- **Setup egress (R10).** A NetworkPolicy applies to the whole pod, so the step hosts cannot be opened through it
  alone. The operator creates an egress proxy credential `agent-<name>-setup-proxy` that opens the preset and
  step hosts. It is mounted only in the `toolchain` init container, like `egress-forge-proxy` for the forge
  sidecar. The agent container never gets it.
  - Residual (R97): a step host such as `objects.githubusercontent.com` can receive data. This is accepted because
    the steps are admin-written and the init container holds no credentials.
- **No root.** Steps install under `$TOOLCHAIN`, and `apt-get` is not supported. A tool that only exists as a
  Debian package belongs in a later image variant, not in a step.
- **Fail closed.** If a step or `verify` fails, the pod stays in `Init`, and `Agent.status.toolchain` names the
  step, the exit code and the last lines (reason `StepFailed`). The console shows it. A download or step failure is
  an operational event. A hash mismatch (D3) is a security event.
- **Refresh.** An init container runs only at pod start, so `refresh` is part of the key (D3): the refresh epoch,
  `floor((now + offset) / refresh)`, is hashed in. `offset` is derived from a hash of the agent name, so agents do
  not all refresh at the same moment. When the epoch changes, the operator updates the pod template annotation
  only while the agent is idle (no job running), the same as other pod changes. The pod then restarts and the
  toolchain is installed again. A change to the spec waits for idle in the same way.

### D3. Toolchain cache

Three layers. Each one is optional and falls back to the one below it.

1. **Per-agent install, persistent and read-only to the agent.**
   - The `toolchain` init container installs into `/state/toolchain/<key>` on the agent PVC. `key` is the SHA-256
     of the canonical toolchain spec: presets with versions, steps, `path`, architecture, base image digest and the
     refresh epoch.
   - **The agent container mounts `/state/toolchain` read-only**, as a separate `subPath` mount, and its `/state`
     read-write mount does not include it. The pod runs as one UID, so without the read-only mount Claude or the
     repository's code could rewrite `bin/go` and the next pod would trust it.
   - As a second check, the init container compares the toolchain's manifest hash (the SHA-256 of every file path,
     mode and hash) with the expected hash on every start, before it reuses the directory. The init container has no
     Kubernetes API credential. It reports the hash in its termination message. The operator reads that, records it
     in `status.toolchain`, and passes the expected hash back as an environment variable on the next pod. A mismatch
     is a security event and triggers a fresh install.
   - Every other container that mounts the agent PVC gets `/state/toolchain` read-only too: the forge sidecar, and
     the chat Jobs that copy the agent pod.
   - The init container deletes old keys after a successful install.
   - A wake from scale-to-zero only verifies the manifest: no download and no install.

2. **Shared toolchain store, per instance.** A content-addressed, write-once store of finished toolchain
   directories, keyed the same way. A new agent, a recreated PVC or a pod on another node unpacks the entry instead
   of running the steps.
   - **Serving:** the operator serves entries over the cluster network through an `agentdev-toolchains` Service
     backed by the operator. It has two ports:
     - a read-only port that agent pods' NetworkPolicy allows;
     - an upload port that only build Jobs (by label and NetworkPolicy) can reach.
     The init container therefore needs no cloud storage host in its egress, which removes the R97 exfiltration path
     a `storage.googleapis.com` rule would open. It is the same path on GKE and kind.
   - **Backing storage:** a GCS bucket `<project>-toolchains` on GKE, written and read only by the operator's
     Workload Identity. On kind, a PVC of the operator. No agent pod can write to the store.
   - **Build:** on a cache miss the operator starts a build Job for that key. It runs the same image, the same init
     container shape and the setup proxy credential. It has no PVC shared with any agent, and no forge or Claude
     credential. The operator mounts a single-use upload token in the Job, bound to that key. The Job streams the
     result to the upload port with that token. Without a valid token, or above `toolchain_max_size` (default 2 GiB),
     the upload is refused. So no other pod can be the first writer of a key.
   - **Integrity:** the operator computes the SHA-256 itself while it receives the entry, and stores it with the
     entry. The Job's own report is not trusted. A second upload under an existing key is refused (write-once). The
     init container checks the SHA-256 before unpacking. A mismatch is refused and is a security event.
   - **Wait:** the agent pod waits for the entry for at most `toolchain_build_timeout` (default 15 min). After that it
     stays in `Init` with reason `BuildFailed` or `Timeout` in `status.toolchain`.
   - **Trust boundary:** an entry is built only from admin-written steps and agentdev presets, by a Job that has no
     credentials and shares no volume with an agent. The key covers everything that defines the content. Two agents
     share an entry only when their toolchain specs are identical, so a repository or an agent cannot poison another
     agent's toolchain. Third-party install scripts inside steps remain a supply-chain risk (V6). Presets reduce it
     with pinned checksums.
   - Presets can be **pre-built** when an agentdev release is published, so the first agent does not wait either.

3. **Dependency caches** (module downloads, not the toolchain itself):
   - **Instance caches for public upstreams only**, through ADR 0003 D3 / #203. On GKE: Artifact Registry remote
     repositories for Go, npm and Cargo. On kind: Athens and Verdaccio as in 0003 D3. **Extension of 0003 D3:** there
     is no crates mirror on kind yet, so cargo uses `index.crates.io` and `static.crates.io` directly through the
     egress allow-list until #203 adds one. Private upstreams stay project-scoped as in 0003 D3.
   - **Per-agent download caches** (`GOMODCACHE`, `CARGO_HOME`, the npm cache) live on the PVC under `/state/cache`,
     so a wake does not re-download.
   - **Build caches** (`GOCACHE`, `target/`) stay per agent and are never shared, because the repository's own code
     can poison them.

**Disk.** The default agent PVC is 10 GiB (`AGENT_STATE_STORAGE`). Rough use for Go: 230 MB toolchain plus 1 to 3 GB
of modules and build cache. For Rust: 300 MB or more of toolchain plus 2 to 6 GB for `CARGO_HOME` and `target/`.
Raise `AGENT_STATE_STORAGE` to 20 GiB or more for Rust agents. The console shows PVC use.

**Expected effect:** the first agent with Go 1.27.4 downloads about 70 MB once for the instance. Every later agent
with the same spec unpacks a cached entry in a few seconds. A wake of an existing agent only checks the manifest.

### D4. Order of work

1. D1 (#496): the worker's `CLAUDE_TOOLS_EXTRA` and the CRD field. It is small, and the tools are no use without it.
2. D2 presets with D3 layer 1, including the read-only mount and the setup proxy credential.
3. D2 free-form steps.
4. D3 layer 2 (the operator store), then layer 3 together with #203.

## Consequences

- New CRD fields `spec.tools` and `spec.toolchain`, and `status.toolchain`. Existing agents without them behave as
  today, and their pods do not restart on upgrade.
- **Worker:** `CLAUDE_TOOLS_EXTRA` is appended to the allow-list. `PATH` gets the toolchain entries.
- **Operator:** a preset catalogue, the `toolchain` init container, the read-only `/state/toolchain` mount, the
  setup proxy credential, the build Job, the store Service, and a GCS bucket in Terraform (GKE).
- **Chat Jobs** copy the agent pod. The admission policy `agentdev-api-chat-jobs` (R101) now allows exactly the
  init container `state-init`. It must also allow `toolchain`, with the same checks: image, no credentials and the
  read-only mount.
- **docs/security.md** gets new rows, each with a test, per ADR 0008:
  - layer-1 poisoning: a write to `/state/toolchain` from the agent container fails, and a changed manifest is
    detected;
  - setup egress: the agent container cannot use the step hosts;
  - store integrity: a hash mismatch, a second upload, an upload without a token and an oversized upload are
    refused, and an agent pod cannot reach the upload port;
  - R97 residual for step hosts.
- **Docs:** admin.md "Give an agent a toolchain" (with the PVC size guidance), threat-model V6.

## Alternatives considered

- **Custom worker image per agent.** Fast at run time, but arbitrary images break the base-image guarantees, and
  every image needs a rebuild on every agentdev upgrade.
- **Image variants (`worker-go`, `worker-rust`).** Fast and simple, but they multiply the image builds, and they do
  not cover free-form needs. They may still come later as presets backed by images.
- **Root steps with `apt-get`.** Flexible, but they weaken the pod security level. Rejected (owner, 2026-11-24).
- **Signed GCS URLs read by the agent pod.** These need `storage.googleapis.com` in egress, which cannot be limited
  to one bucket through CONNECT (R97). Rejected in favour of the operator serving the entries.
- **MinIO on kind.** Another component, and AGPL. The operator's PVC is enough for one cluster.
- **Sharing `GOCACHE` and `target/` across agents.** Faster builds, but a repository can poison a shared build cache.
  Rejected.

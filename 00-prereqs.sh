#!/usr/bin/env bash
# Check host tools and resources before creating anything.
source "$(dirname "$0")/lib.sh"

log "Checking tools"
need kind    "brew install kind"
need kubectl "brew install kubectl"
need helm    "brew install helm"
need "${CONTAINER_ENGINE}" "Docker Desktop, OrbStack, Colima or Podman"
need curl
need python3
command -v coder >/dev/null 2>&1 || warn "coder CLI missing - step 04 needs it: brew install coder"
command -v zstd  >/dev/null 2>&1 || warn "zstd missing - gVisor download falls back to bz2 (fine)"

"${CONTAINER_ENGINE}" info >/dev/null 2>&1 || die "${CONTAINER_ENGINE} daemon is not running"
ok "$(kind version)"
# kindnet enforces NetworkPolicy since kind v0.24; older kind makes the egress allow-list (#65) advisory only
kv=$(kind version | sed -n 's/^kind v\([0-9]*\)\.\([0-9]*\).*/\1 \2/p')
read -r kmaj kmin <<<"${kv:-0 0}"
(( kmaj > 0 || kmin >= 24 )) || die "kind v0.24 or later is needed (NetworkPolicy enforcement for the egress allow-list, #65): brew upgrade kind"
ok "container engine: ${CONTAINER_ENGINE} $("${CONTAINER_ENGINE}" version --format '{{.Server.Version}}' 2>/dev/null || true)"

# kind runs everything inside the container engine's VM, so that VM's limits are what count.
mem_bytes=$("${CONTAINER_ENGINE}" info --format '{{.MemTotal}}' 2>/dev/null || echo 0)
cpus=$("${CONTAINER_ENGINE}" info --format '{{.NCPU}}' 2>/dev/null || echo 0)
mem_gb=$(( mem_bytes / 1024 / 1024 / 1024 ))
echo "  engine VM: ${cpus} CPU, ${mem_gb} GiB RAM"
(( mem_gb >= 6 )) || warn "under 6 GiB for the engine VM; Coder + Postgres + sandboxes may get OOM-killed. Raise it in Docker Desktop/OrbStack settings."

if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  warn "cluster '${CLUSTER_NAME}' already exists - 01-cluster.sh will reuse it (run 99-teardown.sh for a clean start)"
fi
ok "prerequisites look fine"

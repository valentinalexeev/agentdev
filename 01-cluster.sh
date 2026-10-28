#!/usr/bin/env bash
# Dedicated kind cluster (1 control plane + 1 sandbox worker) with gVisor as a RuntimeClass.
# gVisor install logic follows agent-sandbox's own CI script (dev/ci/shared/install-gvisor-kind),
# with the node architecture detected so it also works on Apple Silicon.
source "$(dirname "$0")/lib.sh"

if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  log "Reusing kind cluster '${CLUSTER_NAME}'"
else
  log "Creating kind cluster '${CLUSTER_NAME}'"
  t0=$(ms_now)
  kind create cluster --name "${CLUSTER_NAME}" --config "${SPIKE_DIR}/manifests/kind-config.yaml" --wait 180s
  record "cluster created in $(( ($(ms_now)-t0)/1000 ))s"
fi
k cluster-info >/dev/null && ok "kubectl context kind-${CLUSTER_NAME} works"

log "Installing gVisor ${GVISOR_VERSION} into the kind nodes"
nodes=$("${CONTAINER_ENGINE}" ps --filter "label=io.x-k8s.kind.cluster=${CLUSTER_NAME}" --format '{{.Names}}')
[[ -n "${nodes}" ]] || die "no kind node containers found"
first_node=$(echo "${nodes}" | head -1)
node_arch=$("${CONTAINER_ENGINE}" exec "${first_node}" uname -m)   # x86_64 | aarch64 (gVisor naming)

tmp=$(mktemp -d); trap 'rm -rf "${tmp}"' EXIT
url="https://storage.googleapis.com/gvisor/releases/release/${GVISOR_VERSION}/${node_arch}"
if command -v zstd >/dev/null 2>&1; then tb=gvisor.tar.zstd; tflag=--zstd; else tb=gvisor.tar.bz2; tflag=--bzip2; fi
curl -fsSL --retry 3 "${url}/${tb}" -o "${tmp}/${tb}"
curl -fsSL --retry 3 "${url}/${tb}.sha512" -o "${tmp}/${tb}.sha512"
# macOS has shasum, Linux has sha512sum
( cd "${tmp}" && { sha512sum -c "${tb}.sha512" 2>/dev/null || shasum -a 512 -c "${tb}.sha512"; } ) >/dev/null
tar ${tflag} -xf "${tmp}/${tb}" -C "${tmp}"
chmod +x "${tmp}/runsc" "${tmp}/containerd-shim-runsc-v1"
ok "downloaded and verified gVisor for ${node_arch}"

while IFS= read -r node; do
  "${CONTAINER_ENGINE}" cp "${tmp}/runsc" "${node}:/usr/local/bin/runsc"
  "${CONTAINER_ENGINE}" cp "${tmp}/containerd-shim-runsc-v1" "${node}:/usr/local/bin/containerd-shim-runsc-v1"
  [[ -e "${tmp}/gvisor-bin" ]] && "${CONTAINER_ENGINE}" cp "${tmp}/gvisor-bin" "${node}:/usr/local/bin/gvisor-bin"
  # The containerd patch is already in kind-config.yaml; restart so it sees the new shim binary.
  "${CONTAINER_ENGINE}" exec "${node}" systemctl restart containerd
  ok "gVisor installed on ${node}"
done <<< "${nodes}"
k wait --for=condition=Ready node --all --timeout=120s >/dev/null

k apply -f - >/dev/null <<EOF
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: gvisor
handler: runsc
EOF
ok "RuntimeClass 'gvisor' applied"

log "Smoke test: pod under gVisor"
k delete pod gvisor-smoke --ignore-not-found >/dev/null
k run gvisor-smoke --image=busybox:1.36 --restart=Never \
  --overrides='{"spec":{"runtimeClassName":"gvisor"}}' --command -- sleep 60 >/dev/null
k wait --for=condition=Ready pod/gvisor-smoke --timeout=120s >/dev/null
boot=$(k exec gvisor-smoke -- dmesg 2>/dev/null | head -1 || true)
k delete pod gvisor-smoke --wait=false >/dev/null
if [[ "${boot}" == *gVisor* ]]; then
  ok "sandboxed kernel: ${boot}"; record "gVisor smoke test passed (${node_arch})"
else
  die "pod ran but dmesg does not show gVisor (got: '${boot}'). Check: ${CONTAINER_ENGINE} exec ${first_node} journalctl -u containerd | tail"
fi

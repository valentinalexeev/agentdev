#!/usr/bin/env bash
# Delete the whole spike cluster (nothing outside it is touched, apart from the local agent image).
source "$(dirname "$0")/lib.sh"
log "Deleting kind cluster '${CLUSTER_NAME}'"
kind delete cluster --name "${CLUSTER_NAME}"
rm -rf "${SPIKE_DIR}/.coder"
ok "gone. Remove the image too with: ${CONTAINER_ENGINE} rmi ${AGENT_IMAGE}"

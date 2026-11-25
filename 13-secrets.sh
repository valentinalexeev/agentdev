#!/usr/bin/env bash
# DEPRECATED (#106): this step is now the Ansible playbook infra/ansible/kind.yml (tag secrets).
# This wrapper stays for one MINOR release, then it is removed. Use instead:
#   ansible-playbook infra/ansible/kind.yml --tags secrets
# Scoped secrets store (#202, ADR 0003 D1): External Secrets Operator, the kind SecretStore and the CEL policies.
# The tag also runs the operator again (as this step ran 12), so the operator turns the ProjectSecret controller on.
# Values: the API writes agentdev-ps-<name> into ${SECRETS_VALUES_NS} (never readable through the API); ESO syncs
# each into ps-<name> in ${AGENTS_NS}. On GKE the playbook uses GCP Secret Manager instead (secrets_backend: gcp).
source "$(dirname "$0")/lib.sh"

[[ "${SECRETS_STORE_ENABLED:-true}" == true ]] || { ok "SECRETS_STORE_ENABLED=false: skipped"; exit 0; }
deprecated 13 secrets
need helm
# 12-operator.sh holds the operator settings; with KIND_TAGS=secrets it runs the store role first, then the operator
KIND_TAGS=secrets bash "${SPIKE_DIR}/12-operator.sh" \
  -e "secrets_store_eso_chart_version=${ESO_CHART_VERSION}" -e "secrets_store_eso_ns=${ESO_NS}" \
  -e "secrets_store_eso_sa=${ESO_SA}" -e "secrets_store_values_ns=${SECRETS_VALUES_NS}"
record "scoped secrets store (ESO ${ESO_CHART_VERSION}) up in ${ESO_NS}, store agentdev-secrets ready"

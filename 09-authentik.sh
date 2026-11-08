#!/usr/bin/env bash
# DEPRECATED (#106): this step is now the Ansible playbook infra/ansible/kind.yml (tag authentik).
# This wrapper stays for one MINOR release, then it is removed. Use instead:
#   ansible-playbook infra/ansible/kind.yml --tags authentik
# authentik (#45, #12): the instance's identity provider, in namespace ${PLATFORM_NS}, on the platform Postgres (08).
# The first admin (akadmin) is the owner: its password is generated into .secrets/platform/authentik-admin.
# SSO_STANDIN=0 skips the local company SSO stand-in (#122), SSO_CHECK=0 its browser sign-in check.
source "$(dirname "$0")/lib.sh"

deprecated 09 authentik
need helm; need jq
need ansible-playbook "pipx install ansible-core  (or: brew install ansible)"
t0=$(ms_now)
vars=$(jq -nc --arg v "${AUTHENTIK_VERSION}" --arg e "${AUTHENTIK_ADMIN_EMAIL}" \
  --arg st "${SSO_STANDIN:-1}" --arg sc "${SSO_CHECK:-1}" --arg g "${SSO_ADMIN_GROUP:-agentdev-admins@example.com}" \
  --arg d "${SSO_ALLOWED_DOMAINS:-example.com}" --arg f "${SSO_FORGE_ATTR:-forgeLogin}" \
  --arg fh "${SSO_FORGE_ATTR_GITHUB:-githubLogin}" --arg fg "${SSO_FORGE_ATTR_GITEA:-giteaLogin}" \
  '{authentik: {version: $v, admin_email: $e}, authentik_kind_sso_standin: ($st == "1"), authentik_kind_sso_check: ($sc == "1"),
    authentik_kind_sso_admin_group: $g, authentik_kind_sso_allowed_domains: $d, authentik_kind_sso_forge_attr: $f,
    authentik_kind_sso_forge_attr_github: $fh, authentik_kind_sso_forge_attr_gitea: $fg}')
ANSIBLE_CONFIG="${SPIKE_DIR}/infra/ansible/ansible.cfg" ansible-playbook "${SPIKE_DIR}/infra/ansible/kind.yml" --tags authentik \
  -e "kind_cluster_name=${CLUSTER_NAME}" -e "platform_ns=${PLATFORM_NS}" -e "agents_ns=${AGENTS_NS}" \
  -e "authentik_secrets_dir=${SPIKE_DIR}/.secrets/platform" -e "authentik_kind_secrets_dir=${SPIKE_DIR}/.secrets/platform" \
  -e "${vars}"
record "authentik ${AUTHENTIK_VERSION} up in ${PLATFORM_NS} (Ansible, $(( ($(ms_now)-t0)/1000 ))s)"
echo "  UI: ${KUBECTL} -n ${PLATFORM_NS} port-forward svc/authentik-server 9443:443  ->  https://localhost:9443/ (https: session cookies)"
echo "      login akadmin, password: .secrets/platform/authentik-admin"

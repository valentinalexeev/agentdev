#!/usr/bin/env bash
# Enrol a server for SSH access by an executor (#204, docs/adr/0007, docs/admin.md "Enrol a server").
# Prints what a person applies on the server with their own access. agentdev never holds bootstrap credentials.
#
#   scripts/ssh-enrol.sh <target> [--format shell|ansible|cloud-init] [--namespace NS]
#
# Reads SSHTarget <target> in EXEC_NS. Its status must have the CA keys (the operator writes them once the CA runs).
# The host certificate appears in the status after the operator compared the server's host key with
# spec.hostKeyFingerprint; run the script again after that and apply the full snippet.
#
# The snippet: two local users (apply user with sudo, check user without), principals files, TrustedUserCAKeys,
# an empty KRL, the host certificate, sshd settings for the agentdev users only, and the probe command.
set -euo pipefail
FORMAT=shell NS="${EXEC_NS:-agentdev-executors}" TARGET=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --format) FORMAT="$2"; shift 2 ;;
    --namespace) NS="$2"; shift 2 ;;
    -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
    -*) echo "unknown option $1" >&2; exit 2 ;;
    *) TARGET="$1"; shift ;;
  esac
done
[[ -n "${TARGET}" ]] || { echo "usage: $0 <target> [--format shell|ansible|cloud-init]" >&2; exit 2; }
case "${FORMAT}" in shell|ansible|cloud-init) ;; *) echo "format: shell, ansible or cloud-init" >&2; exit 2 ;; esac
command -v jq >/dev/null || { echo "jq is required" >&2; exit 2; }

json=$(kubectl -n "${NS}" get sshtarget "${TARGET}" -o json)
field() { jq -r "$1 // empty" <<<"${json}"; }
EXECUTOR=$(field .spec.executor)
HOST=$(field .spec.host)
[[ "${HOST}" =~ ^[a-z0-9.-]{1,253}$ ]] || { echo "invalid host" >&2; exit 1; }
APPLY_USER=$(field .spec.applyUser); APPLY_USER=${APPLY_USER:-agentdev}
CHECK_USER=$(field .spec.checkUser); CHECK_USER=${CHECK_USER:-agentdev-check}
USER_CA=$(jq -r '.status.userCAKeys // [] | .[]' <<<"${json}")
HOST_CERT=$(field .status.hostCertificate)
[[ -n "${USER_CA}" ]] || { echo "SSHTarget ${TARGET} has no CA keys yet: is step-ca running (scripts/ssh-ca-init.sh)?" >&2; exit 1; }
# every value comes from the CRD (pattern-checked) or the CA; check again before it reaches a root shell
re_user='^[a-z_][a-z0-9_-]{0,31}$'
[[ "${APPLY_USER}" =~ ${re_user} && "${CHECK_USER}" =~ ${re_user} && "${EXECUTOR}" =~ ^[a-z0-9-]{1,40}$ ]] || { echo "invalid SSHTarget fields" >&2; exit 1; }
while read -r l; do [[ "$l" =~ ^(ssh-ed25519|ecdsa-sha2-nistp256|ecdsa-sha2-nistp384|ecdsa-sha2-nistp521)\ [A-Za-z0-9+/=]+$ ]] || { echo "invalid CA key" >&2; exit 1; }; done <<<"${USER_CA}"
[[ -z "${HOST_CERT}" || "${HOST_CERT}" =~ ^ssh-ed25519-cert-v01@openssh.com\ [A-Za-z0-9+/=]+$ ]] || { echo "invalid host certificate" >&2; exit 1; }

PROBE='#!/bin/sh
# agentdev-probe (#204): the only command of the agentdev-probe certificate (force-command). Prints one line.
echo "agentdev-probe ok host=$(hostname) user=$(id -un) sshd=$(/usr/sbin/sshd -V 2>&1 | head -n1 | tr -d "\n" | cut -c1-60)"'
SSHD_CONF="# agentdev (#204): certificates of the agentdev CA for the agentdev users only
TrustedUserCAKeys /etc/ssh/agentdev_user_ca.pub
RevokedKeys /etc/ssh/agentdev_revoked_keys
HostCertificate /etc/ssh/ssh_host_ed25519_key-cert.pub
Match User ${APPLY_USER},${CHECK_USER}
    AuthorizedPrincipalsFile /etc/ssh/auth_principals/%u
    AuthorizedKeysFile none
    PasswordAuthentication no
    KbdInteractiveAuthentication no
    PubkeyAuthentication yes
    AllowAgentForwarding no
    AllowTcpForwarding no
    X11Forwarding no
    PermitTTY no"
SUDOERS="# agentdev (#204): the apply user only; the check user has no sudo
${APPLY_USER} ALL=(ALL) NOPASSWD: ALL"
if [[ -z "${HOST_CERT}" ]]; then
  echo "# NOTE: no host certificate yet. The operator signs the host key only after it matched spec.hostKeyFingerprint" >&2
  echo "# (kubectl -n ${NS} get sshtarget ${TARGET}). Apply this snippet now and run the script again later, or wait." >&2
fi

emit_shell() {
  cat <<EOF
#!/bin/sh
# agentdev: enrol this host as SSHTarget ${TARGET} (${HOST}) for executor ${EXECUTOR}. Run as root.
set -eu
for u in ${APPLY_USER} ${CHECK_USER}; do id "\$u" >/dev/null 2>&1 || useradd --create-home --shell /bin/sh "\$u"; done
passwd -l ${APPLY_USER} >/dev/null; passwd -l ${CHECK_USER} >/dev/null
install -d -m 0755 /etc/ssh/auth_principals /etc/ssh/sshd_config.d
printf '%s\n' 'agentdev-${EXECUTOR}-apply' > /etc/ssh/auth_principals/${APPLY_USER}
printf '%s\n' 'agentdev-${EXECUTOR}-check' 'agentdev-probe' > /etc/ssh/auth_principals/${CHECK_USER}
chmod 0644 /etc/ssh/auth_principals/${APPLY_USER} /etc/ssh/auth_principals/${CHECK_USER}
cat > /etc/ssh/agentdev_user_ca.pub <<'CA'
${USER_CA}
CA
[ -f /etc/ssh/agentdev_revoked_keys ] || : > /etc/ssh/agentdev_revoked_keys
EOF
  if [[ -n "${HOST_CERT}" ]]; then
    printf "printf '%%s\\\\n' '%s' > /etc/ssh/ssh_host_ed25519_key-cert.pub\n" "${HOST_CERT}"
  fi
  cat <<EOF
cat > /usr/local/sbin/agentdev-probe <<'PROBE'
${PROBE}
PROBE
chmod 0755 /usr/local/sbin/agentdev-probe
cat > /etc/sudoers.d/agentdev <<'SUDO'
${SUDOERS}
SUDO
chmod 0440 /etc/sudoers.d/agentdev
visudo -cf /etc/sudoers.d/agentdev >/dev/null
cat > /etc/ssh/sshd_config.d/50-agentdev.conf <<'SSHD'
${SSHD_CONF}
SSHD
grep -qE '^Include /etc/ssh/sshd_config.d/' /etc/ssh/sshd_config || sed -i '1i Include /etc/ssh/sshd_config.d/*.conf' /etc/ssh/sshd_config
EOF
  if [[ -z "${HOST_CERT}" ]]; then
    echo "sed -i '/^HostCertificate /d' /etc/ssh/sshd_config.d/50-agentdev.conf   # no host certificate yet"
  fi
  cat <<'EOF'
sshd -t
(systemctl reload ssh 2>/dev/null || systemctl reload sshd 2>/dev/null || pkill -HUP -x sshd)
echo "agentdev: enrolled; the operator probes within a few minutes"
EOF
}

indent() { sed "s/^/$1/"; }

emit_ansible() {
  cat <<EOF
# agentdev: enrol hosts as SSHTarget ${TARGET} for executor ${EXECUTOR}. Run with your own access:
#   ansible-playbook -i <inventory> -b enrol-${TARGET}.yml
- name: Enrol for agentdev executor ${EXECUTOR} (#204)
  hosts: all
  become: true
  tasks:
    - name: Run the agentdev enrolment snippet
      ansible.builtin.shell: |
$(emit_shell | indent '        ')
      args: {executable: /bin/sh}
      changed_when: true
EOF
}

emit_cloud_init() {
  cat <<EOF
#cloud-config
# agentdev: enrol as SSHTarget ${TARGET} for executor ${EXECUTOR}. The host key is made at first boot, so the host
# certificate cannot be in this file: after boot, put the server's fingerprint in spec.hostKeyFingerprint and apply
# the shell snippet (or this file's runcmd again) once the operator signed it.
write_files:
  - path: /usr/local/sbin/agentdev-enrol
    permissions: '0700'
    content: |
$(emit_shell | indent '      ')
runcmd:
  - [/usr/local/sbin/agentdev-enrol]
EOF
}

case "${FORMAT}" in
  shell) emit_shell ;;
  ansible) emit_ansible ;;
  cloud-init) emit_cloud_init ;;
esac

#!/usr/bin/env bash
# SSH certificate authority for executors (#204, docs/adr/0007): the key ceremony. Run once per instance by an admin,
# with kubectl pointing at the cluster. Makes the CA keys in a temporary directory with the pinned step-ca image,
# writes two Secrets in EXEC_NS and deletes the directory:
#   step-ca              the CA (config, X.509 intermediate for its TLS, SSH user and host CA keys, key password)
#                        mounted only in the step-ca pod
#   step-ca-provisioner  the operator's JWK provisioner key and the CA root certificate; read by the operator by name
# The X.509 root key is never stored in the cluster: it is printed to stdout only with --keep-root DIR (copy it to
# offline storage) and deleted otherwise. Existing Secrets are never replaced (rotation: docs/admin.md).
#
#   scripts/ssh-ca-init.sh [--namespace NS] [--keep-root DIR] [--dry-run] [--out DIR | --from DIR]
set -euo pipefail
NS="${EXEC_NS:-agentdev-executors}"
KEEP_ROOT="" DRY=false OUT="" FROM=""
STEP_CA_IMAGE="${STEP_CA_IMAGE:-smallstep/step-ca:0.28.4@sha256:0f88382ac5af5c6b7bbba0c6e8fcefef52aee6f22ea364df8e02a09ffd0d22f3}"
ENGINE="${CONTAINER_ENGINE:-docker}"
while [[ $# -gt 0 ]]; do
  case "$1" in
    --namespace) NS="$2"; shift 2 ;;
    --keep-root) KEEP_ROOT="$2"; shift 2 ;;
    --dry-run) DRY=true; shift ;;
    --out) OUT="$2"; DRY=true; shift 2 ;;   # keep the CA files in DIR instead of making Secrets (rotation, tests)
    --from) FROM="$2"; shift 2 ;;           # make the Secrets from a DIR of --out (rotation)
    -h|--help) sed -n '2,12p' "$0"; exit 0 ;;
    *) echo "unknown argument $1" >&2; exit 2 ;;
  esac
done
[[ "${NS}" =~ ^[a-z0-9]([-a-z0-9]*[a-z0-9])?$ ]] || { echo "bad namespace ${NS}" >&2; exit 2; }
if ! ${DRY}; then
  for s in step-ca step-ca-provisioner; do
    if kubectl -n "${NS}" get secret "${s}" >/dev/null 2>&1; then
      echo "Secret ${NS}/${s} exists; not replacing it (rotation: docs/admin.md, 'Rotate the CA key')" >&2; exit 1
    fi
  done
fi
work=$(mktemp -d)
trap 'rm -rf "${work}"' EXIT
chmod 0777 "${work}"   # the image runs as uid 1000
if [[ -n "${FROM}" ]]; then
  [[ -f "${FROM}/ca.json" && -f "${FROM}/provisioner.json" ]] || { echo "${FROM} is not a directory of --out" >&2; exit 2; }
  mkdir -p "${work}/out" && cp -R "${FROM}/." "${work}/out/"
else

# The template decides the certificate: no extensions (no PTY, forwarding or user rc), principals and key ID from the
# operator's signed token, critical options (force-command, source-address) only from the token's agentdev claim.
cat > "${work}/agentdev.tpl" <<'EOF'
{
  "type": {{ toJson .Type }},
  "keyId": {{ toJson .KeyID }},
  "principals": {{ toJson .Principals }},
  "extensions": {},
{{- if and .Token.agentdev .Token.agentdev.criticalOptions }}
  "criticalOptions": {{ toJson .Token.agentdev.criticalOptions }}
{{- else }}
  "criticalOptions": {}
{{- end }}
}
EOF

"${ENGINE}" run --rm -i -v "${work}:/work" -e NS="${NS}" --entrypoint bash "${STEP_CA_IMAGE}" -s <<'EOS'
set -euo pipefail
export STEPPATH=/home/step
umask 077
head -c 32 /dev/urandom | base64 | tr -d '\n=/+' > /tmp/pw
step ca init --ssh --deployment-type standalone --name "agentdev executors" \
  --dns "step-ca.${NS}.svc,step-ca.${NS}.svc.cluster.local" --address :9000 \
  --provisioner bootstrap --password-file /tmp/pw --provisioner-password-file /tmp/pw >/dev/null 2>&1
# only the operator's provisioner: remove the password provisioner of the init and SSHPOP (renewal by certificate
# holders: a server could renew its own host certificate without the operator)
step ca provisioner remove bootstrap --ca-config "${STEPPATH}/config/ca.json" >/dev/null 2>&1
step ca provisioner remove sshpop --ca-config "${STEPPATH}/config/ca.json" >/dev/null 2>&1
step crypto jwk create /tmp/op.pub.json /tmp/op.json --kty EC --crv P-256 --use sig --no-password --insecure --force >/dev/null 2>&1
mkdir -p "${STEPPATH}/templates/ssh"
cp /work/agentdev.tpl "${STEPPATH}/templates/ssh/agentdev.tpl"
step ca provisioner add agentdev-operator --type JWK --public-key /tmp/op.pub.json --ca-config "${STEPPATH}/config/ca.json" \
  --ssh --x509-max-dur 1m --x509-default-dur 1m --x509-min-dur 30s \
  --ssh-user-min-dur 1m --ssh-user-default-dur 10m --ssh-user-max-dur 15m \
  --ssh-host-min-dur 1h --ssh-host-default-dur 2160h --ssh-host-max-dur 2160h \
  --ssh-template "${STEPPATH}/templates/ssh/agentdev.tpl" >/dev/null 2>&1
o=/work/out; mkdir -p "$o"
cp "${STEPPATH}/config/ca.json" "${STEPPATH}/config/defaults.json" "${STEPPATH}/certs/root_ca.crt" \
  "${STEPPATH}/certs/intermediate_ca.crt" "${STEPPATH}/secrets/intermediate_ca_key" \
  "${STEPPATH}/secrets/ssh_host_ca_key" "${STEPPATH}/secrets/ssh_user_ca_key" \
  "${STEPPATH}/certs/ssh_host_ca_key.pub" "${STEPPATH}/certs/ssh_user_ca_key.pub" "$o/"
cp /tmp/pw "$o/password"
# the CA's own SSH config templates (ca.json names them; agentdev does not use them): Secret keys tpl-<name>
for t in "${STEPPATH}"/templates/ssh/*.tpl; do b=$(basename "$t"); [[ "$b" == agentdev.tpl ]] || cp "$t" "$o/tpl-$b"; done
cp /tmp/op.json "$o/provisioner.json"
cp "${STEPPATH}/secrets/root_ca_key" /work/root_ca_key
chmod -R a+rX /work/out /work/root_ca_key
EOS
fi

o="${work}/out"
if [[ -n "${KEEP_ROOT}" && -z "${FROM}" ]]; then
  mkdir -p "${KEEP_ROOT}" && cp "${work}/root_ca_key" "${o}/root_ca.crt" "${KEEP_ROOT}/" && chmod 600 "${KEEP_ROOT}/root_ca_key"
  echo "X.509 root key copied to ${KEEP_ROOT}/root_ca_key: move it to offline storage." >&2
fi
if [[ -n "${OUT}" ]]; then mkdir -p "${OUT}" && cp -R "${o}/." "${OUT}/"; fi
if ${DRY}; then
  echo "dry run: CA made and discarded. SSH user CA: $(cat "${o}/ssh_user_ca_key.pub")"
  echo "dry run: SSH host CA: $(cat "${o}/ssh_host_ca_key.pub")"
  exit 0
fi
args=()
for f in ca.json defaults.json root_ca.crt intermediate_ca.crt intermediate_ca_key ssh_host_ca_key ssh_user_ca_key \
    ssh_host_ca_key.pub ssh_user_ca_key.pub password; do args+=("--from-file=${f}=${o}/${f}"); done
for t in "${o}"/tpl-*.tpl; do args+=("--from-file=$(basename "$t")=${t}"); done
kubectl -n "${NS}" create secret generic step-ca "${args[@]}" >/dev/null
kubectl -n "${NS}" label secret step-ca app=step-ca >/dev/null
kubectl -n "${NS}" create secret generic step-ca-provisioner \
  --from-file=provisioner.json="${o}/provisioner.json" --from-file=root_ca.crt="${o}/root_ca.crt" >/dev/null
echo "Secrets ${NS}/step-ca and ${NS}/step-ca-provisioner created."
echo "SSH user CA: $(cat "${o}/ssh_user_ca_key.pub")"
echo "SSH host CA: $(cat "${o}/ssh_host_ca_key.pub")"

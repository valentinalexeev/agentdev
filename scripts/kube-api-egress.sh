#!/usr/bin/env bash
# Prints the NetworkPolicy egress peers for the Kubernetes API server (#352) as one line of JSON (YAML flow style):
# one /32 ipBlock for the ClusterIP of Service default/kubernetes and one per address of its endpoints (the control
# plane: a NetworkPolicy applies after the Service address is translated, so both are needed). Used by claude-proxy
# for TokenReview (manifests/claude-proxy.yaml.tmpl). The playbook renders the same with
# infra/ansible/templates/kube-api-egress.j2.
#   kube-api-egress.sh                read the addresses with ${KUBECTL:-kubectl} (current context)
#   kube-api-egress.sh <ip>...        print peers for the given addresses (tests, CI)
set -euo pipefail
if [[ $# -eq 0 ]]; then
  read -r -a ips <<<"$(${KUBECTL:-kubectl} -n default get service kubernetes -o jsonpath='{.spec.clusterIP}') $(${KUBECTL:-kubectl} -n default get endpoints kubernetes -o jsonpath='{range .subsets[*].addresses[*]}{.ip}{" "}{end}')"
  set -- "${ips[@]}"
fi
re='^[0-9]{1,3}(\.[0-9]{1,3}){3}$'
out=""
for ip in "$@"; do
  [[ "${ip}" =~ ${re} ]] || { echo "not an IPv4 address: ${ip}" >&2; exit 1; }
  [[ ",${out}," == *"\"${ip}/32\""* ]] && continue
  out+="${out:+,}{\"ipBlock\":{\"cidr\":\"${ip}/32\"}}"
done
[[ -n "${out}" ]] || { echo "no API server address found" >&2; exit 1; }
printf '[%s]\n' "${out}"

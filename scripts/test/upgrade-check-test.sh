#!/usr/bin/env bash
# Test for scripts/upgrade-check.sh (#471): the cluster checks are skipped only when the cluster is not found.
# gcloud and ansible-playbook are stubs on PATH. Run: scripts/test/upgrade-check-test.sh
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
script="$here/../upgrade-check.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
fail=0

mkdir -p "$tmp/bin"
cat > "$tmp/bin/gcloud" <<'EOF'
#!/usr/bin/env bash
echo "$*" >> "$STUB_LOG.gcloud"
case "$GCLOUD_MODE" in
  present) echo agentdev ;;
  absent) echo "ERROR: (gcloud.container.clusters.describe) ResponseError: code=404, message=Not found: projects/p/zones/z/clusters/agentdev." >&2; exit 1 ;;
  denied) echo "ERROR: (gcloud.container.clusters.describe) ResponseError: code=403, message=Permission denied." >&2; exit 1 ;;
esac
EOF
cat > "$tmp/bin/ansible-playbook" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" > "$STUB_LOG.ansible"
EOF
chmod +x "$tmp/bin/gcloud" "$tmp/bin/ansible-playbook"
printf 'gcp_project: my-project\ngcp_zone: "europe-west4-a"\nimage_tag: 1.2.0\n' > "$tmp/instance.yml"

check() { # name expected actual
  if [ "$2" = "$3" ]; then echo "PASS $1"; else
    echo "FAIL $1"; diff <(printf '%s\n' "$2") <(printf '%s\n' "$3") || true; fail=1; fi
}
run() { # gcloud-mode args... ; prints the ansible-playbook args
  rm -f "$tmp/log".*
  GCLOUD_MODE="$1" STUB_LOG="$tmp/log" PATH="$tmp/bin:$PATH" bash "$script" "${@:2}" > "$tmp/out" 2>&1
  cat "$tmp/log.ansible" 2>/dev/null || true
}
inst="$tmp/instance.yml"
all="preflight.yml -e @$inst"
noc="preflight.yml -e @$inst -e {\"preflight_checks\": [\"claude\", \"release\", \"images\", \"plan_identity\"]}"

check "cluster present: all checks, as before" "$all" "$(run present "$inst")"
check "describe uses project, zone and the default cluster name" \
  "container clusters describe agentdev --zone europe-west4-a --project my-project --format=value(name)" \
  "$(cat "$tmp/log.gcloud")"
check "cluster not found: only the no-cluster checks" "$noc" "$(run absent "$inst")"
check "cluster not found: a notice" "1" "$(grep -c '^::notice title=no cluster::' "$tmp/out")"
check "other gcloud error: all checks, as before" "$all" "$(run denied "$inst")"
check "extra ansible args are passed" "$all --check" "$(run present "$inst" --check)"
check "--phase no-cluster: no gcloud call" "$noc" "$(run absent --phase no-cluster "$inst")"
check "--phase no-cluster: gcloud not called" "no" "$([ -f "$tmp/log.gcloud" ] && echo yes || echo no)"
check "--phase cluster: only crds" "preflight.yml -e @$inst -e {\"preflight_checks\": [\"crds\"]}" \
  "$(run present --phase cluster "$inst")"
printf 'cluster_name: other\n' >> "$inst"
run present "$inst" >/dev/null
check "cluster_name from instance.yml" "other" "$(awk '{print $4}' "$tmp/log.gcloud")"

set +e
GCLOUD_MODE=present STUB_LOG="$tmp/log" PATH="$tmp/bin:$PATH" bash "$script" --phase bad "$inst" >/dev/null 2>&1
check "unknown phase exits 2" 2 "$?"
bash "$script" >/dev/null 2>&1; check "no arguments exits 2" 2 "$?"
set -e

exit "$fail"

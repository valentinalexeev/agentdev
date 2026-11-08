#!/usr/bin/env bash
# Tests for scripts/template-drift.sh (#317) with fixture repositories. No network: the fixture agentdev repository
# has a tag with the current examples/deployment/gcp-gke.
#   scripts/test/template-drift.test.sh
set -euo pipefail
root="$(cd "$(dirname "$0")/../.." && pwd)"
sut="$root/scripts/template-drift.sh"
work=$(mktemp -d); trap 'rm -rf "$work"' EXIT
fail() { echo "FAIL: $*"; exit 1; }
ex=examples/deployment/gcp-gke

# fixture agentdev repository: tag v9.9.9 = the current example
up="$work/agentdev"; mkdir -p "$up/$ex"; cp -R "$root/$ex/." "$up/$ex/"
git -C "$up" init -q; git -C "$up" add -A
git -C "$up" -c user.name=t -c user.email=t@t commit -qm example; git -C "$up" tag v9.9.9
export AGENTDEV_REPO="$up" AGENTDEV_RAW_URL="file:///nonexistent"
unset GITHUB_ACTIONS GITHUB_STEP_SUMMARY

# a deployment repository made from the template, with instance values like an owner fills them in
mkrepo() {
  local d="$work/$1"; mkdir -p "$d"; cp -R "$root/$ex/." "$d/"
  sed -i.bak -E 's/\?ref=v[0-9.]+/?ref=v9.9.9/' "$d/terraform/main.tf"
  sed -i.bak -E 's/default = 150/default = 75/; s/^  # system_pool_spot = var/  system_pool_spot = var/' "$d/terraform/main.tf"
  sed -e 's/^gcp_project: .*/gcp_project: prod-123/' -e 's/^# scale_to_zero: false/scale_to_zero: false/' \
    "$d/ansible/instance.yml.example" > "$d/ansible/instance.yml"
  find "$d" -name '*.bak' -delete; echo "$d"
}

# 1. clean copy: no drift, exit 0 also with --strict
d=$(mkrepo clean)
out=$("$sut" --strict "$d") || fail "clean copy: exit $? ($out)"
grep -q 'OK: no template drift against v9.9.9' <<<"$out" || fail "clean copy output: $out"
echo "PASS clean copy (instance values and uncommented arguments are not drift)"

# 2. old deploy.yml: warning, exit 0; --strict exit 1; job summary written
d=$(mkrepo oldwf)
printf '      - run: echo old step\n' >> "$d/.github/workflows/deploy.yml"
out=$("$sut" "$d") || fail "warn mode must exit 0"
grep -q 'WARNING: .github/workflows/deploy.yml differs' <<<"$out" || fail "no deploy.yml warning: $out"
grep -q '^+      - run: echo old step' <<<"$out" || fail "no diff line: $out"
rc=0; "$sut" --strict "$d" >/dev/null || rc=$?
[[ $rc == 1 ]] || fail "--strict exit $rc, want 1"
sum="$work/summary.md"
GITHUB_ACTIONS=true GITHUB_STEP_SUMMARY="$sum" "$sut" "$d" > "$work/gha.out"
grep -q '::warning title=template drift::' "$work/gha.out" || fail "no ::warning annotation"
grep -q 'Template drift against agentdev v9.9.9' "$sum" || fail "no job summary"
echo "PASS old deploy.yml (warning, --strict exit 1, job summary and annotation)"

# 3. main.tf skeleton: a missing module argument and a missing variable are drift
d=$(mkrepo oldtf)
sed -i.bak -e '/agent_pool_max_nodes = var.agent_pool_max_nodes/d' -e '/^variable "agent_pool_max_nodes"/,/^}/d' "$d/terraform/main.tf"
out=$("$sut" "$d")
grep -q '^-  arg agent_pool_max_nodes' <<<"$out" || fail "missing module argument not shown: $out"
grep -q '^-variable "agent_pool_max_nodes"' <<<"$out" || fail "missing variable not shown: $out"
echo "PASS main.tf skeleton (missing argument and variable)"

# 4. instance.yml keys: missing required key and unknown key
d=$(mkrepo oldyml)
sed -i.bak -e '/^reviewer:/d' "$d/ansible/instance.yml"; echo 'old_key: 1' >> "$d/ansible/instance.yml"
out=$("$sut" "$d")
grep -q '^- reviewer' <<<"$out" || fail "missing key not shown: $out"
grep -q '^+ old_key' <<<"$out" || fail "unknown key not shown: $out"
echo "PASS instance.yml keys (missing and unknown)"

# 5. errors: no ref=, unknown release
d=$(mkrepo noref); sed -i.bak 's/?ref=v9.9.9//' "$d/terraform/main.tf"
rc=0; "$sut" "$d" >/dev/null 2>&1 || rc=$?; [[ $rc == 2 ]] || fail "no ref= exit $rc, want 2"
d=$(mkrepo badref); sed -i.bak 's/v9.9.9/v8.8.8/' "$d/terraform/main.tf"
rc=0; "$sut" "$d" >/dev/null 2>&1 || rc=$?; [[ $rc == 2 ]] || fail "unknown release exit $rc, want 2"
echo "PASS errors (no ref=, release not found: exit 2)"

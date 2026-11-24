#!/usr/bin/env bash
# Unit test for the toolchain init container (#499, ADR 0010 D2/D3 layer 1): images/worker/toolchain-install.sh with a
# local preset (a tar archive served from a file:// URL is refused by --proto, so curl is faked). Checks: install into
# /state/toolchain/<key> with a report in the termination message, a wake only verifies, a changed manifest is
# reinstalled (reason Reinstalled), an unrecorded directory is not trusted, a wrong sha256 or a failing verify fails
# closed, symlinks are refused, old keys are removed.
#   bash images/worker/test/toolchain.test.sh
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'chmod -R u+w "${tmp}" 2>/dev/null; rm -rf "${tmp}"' EXIT
mkdir -p "${tmp}/bin" "${tmp}/src/demo-1/bin" "${tmp}/t"
printf '#!/bin/sh\necho demo 1.0\n' > "${tmp}/src/demo-1/bin/demo"; chmod +x "${tmp}/src/demo-1/bin/demo"
tar -czf "${tmp}/demo.tar.gz" -C "${tmp}/src" demo-1
sum=$(sha256sum "${tmp}/demo.tar.gz" | cut -d' ' -f1)
# fake curl: copies the file named by the URL path below ${tmp}
cat > "${tmp}/bin/curl" <<EOF
#!/usr/bin/env bash
out="" url=""
while [[ \$# -gt 0 ]]; do case "\$1" in -o) out=\$2; shift 2;; --proto|--retry) shift 2;; -*) shift;; *) url=\$1; shift;; esac; done
cp "${tmp}/\${url##*/}" "\${out}"
EOF
chmod +x "${tmp}/bin/curl"; export PATH="${tmp}/bin:${PATH}" TMPDIR="${tmp}/t"
export TOOLCHAIN_DIR="${tmp}/state/toolchain" TOOLCHAIN_TERMINATION_LOG="${tmp}/term"
mkdir -p "${tmp}/state"
specf() {  # specf <sha256> [verify]: a one-preset v1 spec for both architectures
  jq -nc --arg s "$1" --arg v "${2:-demo}" '{v:1,image:"worker:test",epoch:0,path:[],presets:[{name:"demo",version:"1",kind:"tar",
    arch:{amd64:{url:"https://example.test/demo.tar.gz",sha256:$s},arm64:{url:"https://example.test/demo.tar.gz",sha256:$s}},
    bin:"demo/bin",verify:[$v]}]}'
}
run() { (export TOOLCHAIN_SPEC="$1" TOOLCHAIN_EXPECT="${2:-}"; bash "${here}/../toolchain-install.sh") 2>"${tmp}/err"; }
rep() { jq -r ".$1" "${tmp}/term"; }

fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; cat "${tmp}/err"; fail=1; fi; }
spec=$(specf "${sum}")
run "${spec}"; rc=$?
key=$(rep key); hash=$(rep hash)
check "first start installs and reports Installed" "[[ ${rc} == 0 && \$(rep reason) == Installed && \${#key} == 64 && \${#hash} == 64 ]]"
check "installed under <key>, current points to it, verify ran" \
  "[[ -x '${TOOLCHAIN_DIR}/${key}/demo/bin/demo' && \$(readlink '${TOOLCHAIN_DIR}/current') == '${key}' ]] && grep -q 'demo 1.0' '${tmp}/err'"
check "the installed tree is not writable" "[[ ! -w '${TOOLCHAIN_DIR}/${key}/demo/bin/demo' && ! -w '${TOOLCHAIN_DIR}/${key}' ]]"
mv "${tmp}/demo.tar.gz" "${tmp}/demo.tar.gz.away"
run "${spec}" "${key}:${hash}"; rc=$?
check "a wake with the recorded hash only verifies (no download)" "[[ ${rc} == 0 && \$(rep reason) == Verified && \$(rep hash) == '${hash}' ]]"
mv "${tmp}/demo.tar.gz.away" "${tmp}/demo.tar.gz"
chmod u+w "${TOOLCHAIN_DIR}/${key}/demo/bin" "${TOOLCHAIN_DIR}/${key}/demo/bin/demo"
printf '#!/bin/sh\necho pwned\n' > "${TOOLCHAIN_DIR}/${key}/demo/bin/demo"
run "${spec}" "${key}:${hash}"; rc=$?
check "a changed manifest is reinstalled with reason Reinstalled" \
  "[[ ${rc} == 0 && \$(rep reason) == Reinstalled && \$(rep hash) == '${hash}' ]] && ! grep -q pwned '${TOOLCHAIN_DIR}/${key}/demo/bin/demo'"
run "${spec}" ""; rc=$?
check "a directory nobody recorded is not reused" "[[ ${rc} == 0 && \$(rep reason) == Installed ]]"
other=$(specf "${sum}" | jq -c '.epoch = 1')
run "${other}"; rc=$?
check "a new key (refresh epoch) removes the old key" "[[ ${rc} == 0 && \$(rep key) != '${key}' && ! -e '${TOOLCHAIN_DIR}/${key}' ]]"
key2=$(rep key)
run "$(specf 0000000000000000000000000000000000000000000000000000000000000000 | jq -c '.epoch = 2')"; rc=$?
check "a wrong sha256 fails closed (StepFailed), the last good install stays" \
  "[[ ${rc} != 0 && \$(rep reason) == StepFailed && \$(rep message) == *'sha256 mismatch'* && -d '${TOOLCHAIN_DIR}/${key2}' ]]"
run "$(specf "${sum}" false | jq -c '.epoch = 3')"; rc=$?
check "a failing verify fails closed" "[[ ${rc} != 0 && \$(rep reason) == StepFailed && \$(rep message) == *verify* ]]"
run 'not json'; rc=$?
check "an invalid spec fails closed" "[[ ${rc} != 0 && \$(rep reason) == Invalid ]]"
ln -s /etc "${TOOLCHAIN_DIR}/planted"
run "${spec}"; rc=$?
check "a symlink in /state/toolchain is refused" "[[ ${rc} != 0 && \$(rep reason) == Invalid && \$(rep message) == *symlink* ]]"
rm "${TOOLCHAIN_DIR}/planted"
chmod -R u+w "${TOOLCHAIN_DIR}"; rm -rf "${TOOLCHAIN_DIR}"; mkdir -p "${tmp}/elsewhere"; ln -s "${tmp}/elsewhere" "${TOOLCHAIN_DIR}"
run "${spec}"; rc=$?
check "/state/toolchain itself as a symlink is refused" "[[ ${rc} != 0 && \$(rep reason) == Invalid && -z \$(ls -A '${tmp}/elsewhere') ]]"
exit ${fail}

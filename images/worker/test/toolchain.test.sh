#!/usr/bin/env bash
# Unit test for the toolchain init container (#499, ADR 0010 D2/D3 layer 1): images/worker/toolchain-install.sh with a
# fake go preset (curl is faked and serves a local archive for the official URL). Checks: install into
# /state/toolchain/<key> with a report in the termination message, a wake only verifies, a changed manifest is
# reinstalled (reason Reinstalled), an unrecorded directory is not trusted, a wrong sha256 or a failing verify fails
# closed, symlinks are refused, old keys are removed. Review of #537: a spec cannot choose the command that runs (verify,
# kind, bin), an unknown preset or a non-official URL is refused, and verify runs without the setup proxy key.
#   bash images/worker/test/toolchain.test.sh
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd)
tmp=$(mktemp -d); trap 'chmod -R u+w "${tmp}" 2>/dev/null; rm -rf "${tmp}"' EXIT
mkdir -p "${tmp}/bin" "${tmp}/src/go/bin" "${tmp}/bad/go/bin" "${tmp}/t"
# the fake go records the setup key it sees (it must see none) and prints its version
printf '#!/bin/sh\nprintf %%s "${EGRESS_SETUP_KEY:-}${HTTPS_PROXY:-}" > "%s/seen"\necho go version go1.0.0\n' "${tmp}" > "${tmp}/src/go/bin/go"
printf '#!/bin/sh\nexit 1\n' > "${tmp}/bad/go/bin/go"
chmod +x "${tmp}/src/go/bin/go" "${tmp}/bad/go/bin/go"
for a in amd64 arm64; do
  tar -czf "${tmp}/go1.0.0.linux-${a}.tar.gz" -C "${tmp}/src" go
  tar -czf "${tmp}/go1.0.1.linux-${a}.tar.gz" -C "${tmp}/bad" go
done
sum=$(sha256sum "${tmp}/go1.0.0.linux-amd64.tar.gz" | cut -d' ' -f1)
badsum=$(sha256sum "${tmp}/go1.0.1.linux-amd64.tar.gz" | cut -d' ' -f1)
# fake curl: copies the file named by the URL path below ${tmp}
cat > "${tmp}/bin/curl" <<EOS
#!/usr/bin/env bash
out="" url=""
while [[ \$# -gt 0 ]]; do case "\$1" in -o) out=\$2; shift 2;; --proto|--retry) shift 2;; -*) shift;; *) url=\$1; shift;; esac; done
cp "${tmp}/\${url##*/}" "\${out}"
EOS
chmod +x "${tmp}/bin/curl"; export PATH="${tmp}/bin:${PATH}" TMPDIR="${tmp}/t"
export TOOLCHAIN_DIR="${tmp}/state/toolchain" TOOLCHAIN_TERMINATION_LOG="${tmp}/term"
export EGRESS_SETUP_KEY=s3cret HTTPS_PROXY="http://setup:s3cret@proxy.test:3128"
mkdir -p "${tmp}/state"
specf() {  # specf <sha256> [version]: a one-preset (go) v1 spec for both architectures, URLs as the operator renders
  jq -nc --arg s "$1" --arg v "${2:-1.0.0}" '{v:1,image:"worker:test",epoch:0,path:[],presets:[{name:"go",version:$v,kind:"tar",
    arch:{amd64:{url:("https://dl.google.com/go/go"+$v+".linux-amd64.tar.gz"),sha256:$s},
          arm64:{url:("https://dl.google.com/go/go"+$v+".linux-arm64.tar.gz"),sha256:$s}},
    bin:"go/bin",verify:["go","version"]}]}'
}
run() { (export TOOLCHAIN_SPEC="$1" TOOLCHAIN_EXPECT="${2:-}"; bash "${here}/../toolchain-install.sh") 2>"${tmp}/err"; }
rep() { jq -r ".$1" "${tmp}/term"; }

fail=0; check() { if eval "$2"; then echo "ok   - $1"; else echo "FAIL - $1"; cat "${tmp}/err"; fail=1; fi; }
spec=$(specf "${sum}")
run "${spec}"; rc=$?
key=$(rep key); hash=$(rep hash)
check "first start installs and reports Installed" "[[ ${rc} == 0 && \$(rep reason) == Installed && \${#key} == 64 && \${#hash} == 64 ]]"
check "installed under <key>, current points to it, verify ran" \
  "[[ -x '${TOOLCHAIN_DIR}/${key}/go/bin/go' && \$(readlink '${TOOLCHAIN_DIR}/current') == '${key}' ]] && grep -q 'go1.0.0' '${tmp}/err'"
check "verify runs without the setup proxy key or proxy" "[[ -f '${tmp}/seen' && ! -s '${tmp}/seen' ]]"
check "the installed tree is not writable" "[[ ! -w '${TOOLCHAIN_DIR}/${key}/go/bin/go' && ! -w '${TOOLCHAIN_DIR}/${key}' ]]"
mv "${tmp}/go1.0.0.linux-amd64.tar.gz" "${tmp}/away-amd64"; mv "${tmp}/go1.0.0.linux-arm64.tar.gz" "${tmp}/away-arm64"
run "${spec}" "${key}:${hash}"; rc=$?
check "a wake with the recorded hash only verifies (no download)" "[[ ${rc} == 0 && \$(rep reason) == Verified && \$(rep hash) == '${hash}' ]]"
# #581: the kubelet's fsGroup change on each mount (files u+rw,g+rw; directories ug+rwx,g+s) is not a changed manifest
find "${TOOLCHAIN_DIR}/${key}" -type f -exec chmod ug+rw {} +
find "${TOOLCHAIN_DIR}/${key}" -type d -exec chmod ug+rwx,g+s {} +
run "${spec}" "${key}:${hash}"; rc=$?
check "the kubelet's fsGroup mode change on a new pod only verifies (#581)" "[[ ${rc} == 0 && \$(rep reason) == Verified && \$(rep hash) == '${hash}' ]]"
mv "${tmp}/away-amd64" "${tmp}/go1.0.0.linux-amd64.tar.gz"; mv "${tmp}/away-arm64" "${tmp}/go1.0.0.linux-arm64.tar.gz"
chmod a-x "${TOOLCHAIN_DIR}/${key}/go/bin/go"
run "${spec}" "${key}:${hash}"; rc=$?
check "a removed execute bit is still a changed manifest (#581)" "[[ ${rc} == 0 && \$(rep reason) == Reinstalled && \$(rep hash) == '${hash}' ]]"
chmod u+w "${TOOLCHAIN_DIR}/${key}/go/bin" "${TOOLCHAIN_DIR}/${key}/go/bin/go"
printf '#!/bin/sh\necho pwned\n' > "${TOOLCHAIN_DIR}/${key}/go/bin/go"
run "${spec}" "${key}:${hash}"; rc=$?
check "a changed manifest is reinstalled with reason Reinstalled" \
  "[[ ${rc} == 0 && \$(rep reason) == Reinstalled && \$(rep hash) == '${hash}' ]] && ! grep -q pwned '${TOOLCHAIN_DIR}/${key}/go/bin/go'"
run "${spec}" ""; rc=$?
check "a directory nobody recorded is not reused" "[[ ${rc} == 0 && \$(rep reason) == Installed ]]"
other=$(specf "${sum}" | jq -c '.epoch = 1')
run "${other}"; rc=$?
check "a new key (refresh epoch) removes the old key" "[[ ${rc} == 0 && \$(rep key) != '${key}' && ! -e '${TOOLCHAIN_DIR}/${key}' ]]"
key2=$(rep key)
run "$(specf 0000000000000000000000000000000000000000000000000000000000000000 | jq -c '.epoch = 2')"; rc=$?
check "a wrong sha256 fails closed (StepFailed), the last good install stays" \
  "[[ ${rc} != 0 && \$(rep reason) == StepFailed && \$(rep message) == *'sha256 mismatch'* && -d '${TOOLCHAIN_DIR}/${key2}' ]]"
run "$(specf "${badsum}" 1.0.1 | jq -c '.epoch = 3')"; rc=$?
check "a failing verify fails closed" "[[ ${rc} != 0 && \$(rep reason) == StepFailed && \$(rep message) == *verify* ]]"
# review of #537: the spec cannot choose what runs with the key
rm -f "${tmp}/pwned"
evil=$(specf "${sum}" | jq -c --arg f "${tmp}/pwned" '.epoch = 4 | .presets[0].verify = ["sh","-c","printenv EGRESS_SETUP_KEY > "+$f] | .presets[0].bin = "../../.."')
run "${evil}"; rc=$?
check "a spec's own verify and bin are ignored (the image's go version runs, keyless)" \
  "[[ ${rc} == 0 && ! -e '${tmp}/pwned' && ! -s '${tmp}/seen' ]] && grep -q 'go1.0.0' '${tmp}/err'"
run "$(specf "${sum}" | jq -c '.epoch = 5 | .presets[0].kind = "rustup"')"; rc=$?
check "a spec's own kind is ignored" "[[ ${rc} == 0 && \$(rep reason) == Installed ]]"
run "$(specf "${sum}" | jq -c '.epoch = 6 | .presets[0].name = "evil"')"; rc=$?
check "an unknown preset name is refused" "[[ ${rc} != 0 && \$(rep reason) == StepFailed && \$(rep message) == *'not a preset'* ]]"
run "$(specf "${sum}" | jq -c '.epoch = 7 | .presets[0].arch |= map_values(.url = "https://dl.google.com/x/go1.0.0.linux-amd64.tar.gz")')"; rc=$?
check "a non-official download URL is refused" "[[ ${rc} != 0 && \$(rep message) == *'not the official download'* ]]"
run "$(specf "${sum}" '1.0.0/../x' | jq -c '.epoch = 8')"; rc=$?
check "a malformed version is refused" "[[ ${rc} != 0 && \$(rep message) == *malformed* ]]"
# review of #537: rustup-init runs in the private temporary directory, not in a cwd the agent can write (a planted
# rust-toolchain.toml), with RUSTUP_TOOLCHAIN pinned and inherited rustup overrides removed. The fake rustup-init
# records what it sees and installs a cargo that prints its version.
cat > "${tmp}/rustup-init" <<EOS
#!/bin/sh
{ echo "cwd=\$(pwd)"; echo "tc=\${RUSTUP_TOOLCHAIN:-}"; echo "dist=\${RUSTUP_DIST_SERVER:-}"; } > "${tmp}/rustup-seen"
mkdir -p "\${CARGO_HOME}/bin"; printf '#!/bin/sh\necho cargo 1.85.0\n' > "\${CARGO_HOME}/bin/cargo"; chmod +x "\${CARGO_HOME}/bin/cargo"
EOS
rsum=$(sha256sum "${tmp}/rustup-init" | cut -d' ' -f1)
rspec=$(jq -nc --arg s "${rsum}" '{v:1,image:"worker:test",epoch:9,path:[],presets:[{name:"rust",version:"1.85.0",arch:{
  amd64:{url:"https://static.rust-lang.org/rustup/archive/1.29.3/x86_64-unknown-linux-gnu/rustup-init",sha256:$s},
  arm64:{url:"https://static.rust-lang.org/rustup/archive/1.29.3/aarch64-unknown-linux-gnu/rustup-init",sha256:$s}}}]}')
mkdir -p "${tmp}/agentcwd"; printf '[toolchain]\npath = "/evil"\n' > "${tmp}/agentcwd/rust-toolchain.toml"
(cd "${tmp}/agentcwd" && RUSTUP_TOOLCHAIN=evil RUSTUP_DIST_SERVER=https://evil.test run "${rspec}"); rc=$?
check "rustup-init runs in the private tmp dir with RUSTUP_TOOLCHAIN pinned, not in a planted cwd" \
  "[[ ${rc} == 0 ]] && grep -qx 'cwd=${tmp}/t/rust' '${tmp}/rustup-seen' && grep -qx 'tc=1.85.0' '${tmp}/rustup-seen' && grep -qx 'dist=' '${tmp}/rustup-seen'"
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

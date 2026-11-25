#!/usr/bin/env bash
# The toolchain init container's script (#499), run by the CI shell tests (ubuntu: GNU find, stat, tar, jq):
# images/worker/test/toolchain.test.sh holds the checks.
exec bash "$(dirname "$0")/../../images/worker/test/toolchain.test.sh"

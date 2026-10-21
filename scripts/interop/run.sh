#!/usr/bin/env sh
# Verifies MOR-RPC wire compatibility with the reference Morpheus-Lumerin-Node
# proxy-router in both directions:
#   1. messages produced by the reference code are verified/decrypted by morcore
#   2. messages produced by morcore are verified/decrypted/parsed by the reference
#
#   scripts/interop/run.sh            # against the reference main branch
#   REF_BRANCH=dev scripts/interop/run.sh
set -eu

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
WORK="${WORK:-$(mktemp -d)}"
REF_BRANCH="${REF_BRANCH:-main}"

echo "==> cloning MorpheusAIs/Morpheus-Lumerin-Node@${REF_BRANCH} into ${WORK}"
git clone -q --depth 1 --branch "${REF_BRANCH}" https://github.com/MorpheusAIs/Morpheus-Lumerin-Node.git "${WORK}/ref"
PR="${WORK}/ref/proxy-router"
cp "${ROOT}/scripts/interop/reference_generate_test.go.txt" "${PR}/internal/proxyapi/morrpcmessage/zz_golden_gen_test.go"
cp "${ROOT}/scripts/interop/reference_verify_test.go.txt" "${PR}/internal/proxyapi/zz_morcore_interop_test.go"

# check runs a test command in a directory and prints only the test summary
# lines, but fails with the full log when the tests fail. (A plain pipe into
# grep would hide the failure: POSIX sh has no pipefail.)
check() {
	dir="$1"
	shift
	log="${WORK}/test.log"
	if (cd "${dir}" && "$@") >"${log}" 2>&1; then
		grep -E "^(--- |ok)" "${log}" || true
	else
		cat "${log}" >&2
		echo "==> FAILED in ${dir}: $*" >&2
		exit 1
	fi
}

echo "==> reference → morcore"
check "${PR}" env GOLDEN_OUT="${WORK}/reference_vectors.json" \
	go test ./internal/proxyapi/morrpcmessage/ -run TestZZGenerateGoldenVectors -count=1 -v
check "${ROOT}" env MORCORE_REFERENCE_VECTORS="${WORK}/reference_vectors.json" \
	go test ./internal/morrpc/ -run TestReference -count=1 -v

echo "==> morcore → reference"
check "${ROOT}" env MORCORE_EXPORT="${WORK}/morcore_vectors.json" \
	go test ./internal/morrpc/ -run TestExportVectors -count=1 -v
check "${PR}" env MORCORE_VECTORS="${WORK}/morcore_vectors.json" \
	go test ./internal/proxyapi/ -run TestZZMorcoreInterop -count=1 -v

echo "==> MOR-RPC interop verified against Morpheus-Lumerin-Node@${REF_BRANCH}"

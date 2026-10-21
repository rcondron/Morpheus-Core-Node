#!/usr/bin/env sh
# Cross-compiles static morcore binaries for every supported platform and
# packages them with checksums into dist/.
#
#   scripts/release.sh [version]
set -eu

VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
COMMIT="$(git rev-parse --short HEAD 2>/dev/null || echo none)"
DATE="$(date -u +%Y-%m-%dT%H:%M:%SZ)"
PKG="github.com/rcondron/Morpheus-Core-Node/internal/buildinfo"
LDFLAGS="-s -w -X ${PKG}.Version=${VERSION} -X ${PKG}.Commit=${COMMIT} -X ${PKG}.Date=${DATE}"
PLATFORMS="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64 windows/arm64"

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
DIST="${ROOT}/dist"
rm -rf "${DIST}"
mkdir -p "${DIST}"

for platform in ${PLATFORMS}; do
	os="${platform%/*}"
	arch="${platform#*/}"
	name="morcore_${os}_${arch}"
	stage="${DIST}/${name}"
	mkdir -p "${stage}"
	ext=""
	[ "${os}" = "windows" ] && ext=".exe"
	echo "building ${name}"
	(cd "${ROOT}" && CGO_ENABLED=0 GOOS="${os}" GOARCH="${arch}" \
		go build -trimpath -ldflags "${LDFLAGS}" -o "${stage}/morcore${ext}" ./cmd/morcore)
	cp "${ROOT}/README.md" "${stage}/"
	if [ "${os}" = "windows" ]; then
		(cd "${DIST}" && zip -qr "${name}.zip" "${name}")
	else
		tar -C "${DIST}" -czf "${DIST}/${name}.tar.gz" "${name}"
	fi
	rm -rf "${stage}"
done

cd "${DIST}"
if command -v sha256sum >/dev/null 2>&1; then
	sha256sum ./*.tar.gz ./*.zip | sed 's# \./# #' >checksums.txt
else
	shasum -a 256 ./*.tar.gz ./*.zip | sed 's# \./# #' >checksums.txt
fi
echo "artifacts in ${DIST}:"
ls -lh "${DIST}"

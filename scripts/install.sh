#!/bin/sh
# Installs the latest morcore release on Linux or macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/rcondron/Morpheus-Core-Node/main/scripts/install.sh | sh
#
# Environment:
#   MORCORE_VERSION       release tag to install (default: latest)
#   MORCORE_INSTALL_DIR   target directory (default: /usr/local/bin if writable, else ~/.local/bin)
#   MORCORE_DOWNLOAD_BASE alternate download location (mirrors, testing)
set -eu

REPO="rcondron/Morpheus-Core-Node"
VERSION="${MORCORE_VERSION:-latest}"

say() { printf '\033[32m%s\033[0m %s\n' "==>" "$*"; }
die() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

case "$(uname -s)" in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) die "unsupported OS $(uname -s); on Windows use scripts/install.ps1" ;;
esac
case "$(uname -m)" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "unsupported architecture $(uname -m)" ;;
esac

asset="morcore_${os}_${arch}.tar.gz"
if [ -n "${MORCORE_DOWNLOAD_BASE:-}" ]; then
	base="${MORCORE_DOWNLOAD_BASE}"
elif [ "${VERSION}" = "latest" ]; then
	base="https://github.com/${REPO}/releases/latest/download"
else
	base="https://github.com/${REPO}/releases/download/${VERSION}"
fi

fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1" -o "$2"
	elif command -v wget >/dev/null 2>&1; then
		wget -q "$1" -O "$2"
	else
		die "curl or wget is required"
	fi
}

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

say "Downloading ${asset} (${VERSION})"
fetch "${base}/${asset}" "${tmp}/${asset}"
fetch "${base}/checksums.txt" "${tmp}/checksums.txt"

expected="$(grep " ${asset}\$" "${tmp}/checksums.txt" | awk '{print $1}')"
[ -n "${expected}" ] || die "no checksum published for ${asset}"
if command -v sha256sum >/dev/null 2>&1; then
	actual="$(sha256sum "${tmp}/${asset}" | awk '{print $1}')"
else
	actual="$(shasum -a 256 "${tmp}/${asset}" | awk '{print $1}')"
fi
[ "${expected}" = "${actual}" ] || die "checksum mismatch for ${asset}"
say "Checksum verified"

tar -xzf "${tmp}/${asset}" -C "${tmp}"

dir="${MORCORE_INSTALL_DIR:-}"
if [ -z "${dir}" ]; then
	if [ -w /usr/local/bin ]; then
		dir=/usr/local/bin
	else
		dir="${HOME}/.local/bin"
	fi
fi
mkdir -p "${dir}"
install -m 0755 "${tmp}/morcore_${os}_${arch}/morcore" "${dir}/morcore"
say "Installed $("${dir}/morcore" version) to ${dir}"

case ":${PATH}:" in
	*":${dir}:"*) ;;
	*) printf '\nAdd %s to your PATH:\n  export PATH="%s:$PATH"\n' "${dir}" "${dir}" ;;
esac
printf '\nGet started:\n  morcore\n\n'

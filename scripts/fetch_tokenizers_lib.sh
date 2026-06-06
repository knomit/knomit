#!/usr/bin/env bash
# fetch_tokenizers_lib.sh <version> <dest-dir>
# Downloads libtokenizers.a from daulet/tokenizers GitHub releases.
# Idempotent: skips download if <dest-dir>/libtokenizers.a already exists.
set -euo pipefail

VERSION="${1:?Usage: $0 <version> <dest-dir>}"
DEST="${2:?Usage: $0 <version> <dest-dir>}"

if [ -f "${DEST}/libtokenizers.a" ]; then
	echo "libtokenizers.a already present in ${DEST}, skipping download."
	exit 0
fi

OS="$(uname -s)"
ARCH="$(uname -m)"

case "${OS}/${ARCH}" in
	Darwin/arm64)   PLAT="darwin-aarch64" ;;
	Darwin/x86_64)  PLAT="darwin-x86_64"  ;;
	Linux/x86_64)   PLAT="linux-amd64"    ;;
	Linux/aarch64)  PLAT="linux-arm64"    ;;
	*)
		echo "Unsupported platform: ${OS}/${ARCH}. Add it to scripts/fetch_tokenizers_lib.sh." >&2
		exit 1
		;;
esac

URL="https://github.com/daulet/tokenizers/releases/download/${VERSION}/libtokenizers.${PLAT}.tar.gz"

echo "Downloading libtokenizers ${VERSION} for ${PLAT}..."
mkdir -p "${DEST}"
TMPDIR="$(mktemp -d)"
trap 'rm -rf "${TMPDIR}"' EXIT

curl -fsSL "${URL}" | tar xz -C "${TMPDIR}"
cp "${TMPDIR}/libtokenizers.a" "${DEST}/libtokenizers.a"
echo "libtokenizers.a installed to ${DEST}/"

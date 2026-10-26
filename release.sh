#!/usr/bin/env bash
# Cross-compiles release binaries for meshcore-http-gateway and packages
# them under dist/. Run manually before tagging a release: ./release.sh [version]
#
# Note: CGO is deliberately NOT disabled — the macOS BLE transport needs it
# (cbgo); cross-compiling to linux/windows disables cgo automatically.
set -euo pipefail

cd "$(dirname "${BASH_SOURCE[0]}")"

BINARY="meshcore-http-gateway"
PKG="."
VERSION="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
DIST="dist"

PLATFORMS=(
  "linux amd64"
  "linux arm64"
  "darwin arm64"
  "windows amd64"
)

rm -rf "$DIST"
mkdir -p "$DIST"

for platform in "${PLATFORMS[@]}"; do
  read -r GOOS GOARCH <<<"$platform"
  ext=""
  [ "$GOOS" = "windows" ] && ext=".exe"

  out_name="${BINARY}-${VERSION}-${GOOS}-${GOARCH}"
  out_dir="${DIST}/${out_name}"
  mkdir -p "$out_dir"

  echo "Building ${GOOS}/${GOARCH}..."
  GOOS="$GOOS" GOARCH="$GOARCH" go build \
    -trimpath \
    -ldflags "-s -w" \
    -o "${out_dir}/${BINARY}${ext}" \
    "$PKG"

  cp README.md README_RU.md LICENSE config.example.yaml "$out_dir/" 2>/dev/null || true

  (
    cd "$DIST"
    if [ "$GOOS" = "windows" ]; then
      zip -qr "${out_name}.zip" "$out_name"
    else
      tar -czf "${out_name}.tar.gz" "$out_name"
    fi
    rm -rf "$out_name"
  )
done

(
  cd "$DIST"
  shasum -a 256 ./*.tar.gz ./*.zip >checksums.txt 2>/dev/null || true
)

echo "Done. Artifacts in ${DIST}/:"
ls -1 "$DIST"

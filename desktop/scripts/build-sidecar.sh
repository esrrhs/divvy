#!/bin/sh
# Build the divvy Go binary into desktop/binaries with the
# target-triple suffix Tauri's externalBin expects. The CLI runs this
# before bundling; dev.sh runs it before `tauri dev`.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
ROOT=$(CDPATH= cd -- "$SCRIPT_DIR/../.." && pwd)
BIN_DIR="$SCRIPT_DIR/../binaries"
mkdir -p "$BIN_DIR"

GOOS_VALUE=$(go env GOOS)
GOARCH_VALUE=$(go env GOARCH)

case "$GOOS_VALUE-$GOARCH_VALUE" in
  darwin-arm64) TRIPLE=aarch64-apple-darwin ;;
  darwin-amd64) TRIPLE=x86_64-apple-darwin ;;
  linux-amd64)  TRIPLE=x86_64-unknown-linux-gnu ;;
  linux-arm64)  TRIPLE=aarch64-unknown-linux-gnu ;;
  windows-amd64) TRIPLE=x86_64-pc-windows-msvc ;;
  *)
    echo "unsupported sidecar target: $GOOS_VALUE/$GOARCH_VALUE" >&2
    exit 1
    ;;
esac

OUT="$BIN_DIR/divvy-sidecar-$TRIPLE"
if [ "$GOOS_VALUE" = "windows" ]; then
  OUT="$OUT.exe"
fi
echo "building sidecar for $TRIPLE -> $OUT"
(
  cd "$ROOT"
  go build -o "$OUT" ./cmd/divvy
)
chmod +x "$OUT"
echo "sidecar ready: $OUT"

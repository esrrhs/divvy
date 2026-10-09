#!/bin/sh
# `tauri dev` hook: make sure the Go sidecar binary exists, then run Vite.
# The Vite dev server proxies /api to the sidecar's fixed dev port.
set -eu

SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)

# Keep this in sync with the fixed port the Rust shell uses in debug builds.
export VITE_DEV_TARGET="${VITE_DEV_TARGET:-http://127.0.0.1:8799}"

sh "$SCRIPT_DIR/build-sidecar.sh"

cd "$SCRIPT_DIR/../../web"
exec npm run dev

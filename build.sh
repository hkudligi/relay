#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN_DIR="${BIN_DIR:-$ROOT_DIR/bin}"
HOMEBREW_DIR="${HOMEBREW_DIR:-$ROOT_DIR/homebrew}"
GO_CACHE="${GOCACHE:-${TMPDIR:-/tmp}/rly-go-build-cache}"

mkdir -p "$BIN_DIR" "$HOMEBREW_DIR/bin" "$GO_CACHE"

echo "Building rly..."
GOCACHE="$GO_CACHE" go build -trimpath -ldflags="-s -w" -o "$BIN_DIR/rly" "$ROOT_DIR/cmd/rly"

# Homebrew installs executables from a bin/ directory. Keep a staged copy so
# the same script produces both the normal local binary and a Homebrew-ready
# package tree without modifying the user's Homebrew installation.
install -m 0755 "$BIN_DIR/rly" "$HOMEBREW_DIR/bin/rly"

if command -v brew >/dev/null 2>&1; then
	BREW_PREFIX="${HOMEBREW_PREFIX:-$(brew --prefix)}"
	install -m 0755 "$BIN_DIR/rly" "$BREW_PREFIX/bin/rly"
	echo "  $BREW_PREFIX/bin/rly"
else
	echo "Homebrew not found; skipped Homebrew prefix install." >&2
fi

echo "Built:"
echo "  $BIN_DIR/rly"
echo "  $HOMEBREW_DIR/bin/rly"

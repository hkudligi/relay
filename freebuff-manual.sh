#!/bin/sh
set -eu

exec env FREEBUFF_UI=terminal go run ./cmd/freebuff-tmux-test "$@"

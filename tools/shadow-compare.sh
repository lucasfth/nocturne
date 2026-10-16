#!/bin/sh
# No service/config mutation: fixtures, listeners and binary belong to TemporaryDirectory.
set -eu
cd "$(dirname "$0")/.."
exec "${UV:-$HOME/.local/bin/uv}" run python tools/shadow_compare.py "$@"

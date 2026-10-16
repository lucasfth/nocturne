#!/bin/sh
# Build in the Go module for internal imports; remove generated sources immediately.
set -eu
cd "$(dirname "$0")/../.."
d=$(mktemp -d go/browser-fixture.XXXXXX)
bin=$(mktemp -d "${TMPDIR:-/tmp}/nocturne-browser-bin.XXXXXX")
pid=''
cleanup() { if [ -n "$pid" ]; then kill "$pid" 2>/dev/null || true; wait "$pid" 2>/dev/null || true; fi; rm -rf "$d" "$bin"; }
trap cleanup EXIT
trap 'exit 0' INT TERM
cp web/tests/go-benchmark-server.go "$d/main.go"
(cd go && go build -o "$bin/server" ./"${d#go/}")
rm -rf "$d"
"$bin/server" -web-dir "$PWD/web/dist" &
pid=$!
wait "$pid"

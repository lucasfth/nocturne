#!/bin/bash
set -euo pipefail
[[ $(uname -s) == Darwin && $EUID == 0 ]] || exit 1
for name in io.nocturne.worker io.nocturne.llama io.nocturne.tunnel; do
 launchctl bootout "system/$name" 2>/dev/null || true
 rm -f "/Library/LaunchDaemons/$name.plist"
done
# Preserve model and secret files unless explicitly asked to erase them.
echo 'Services removed. /opt/nocturne and dedicated user preserved.'

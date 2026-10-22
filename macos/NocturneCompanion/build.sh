#!/bin/bash
set -euo pipefail
cd "$(dirname "$0")"
OUT=${1:-build}
mkdir -p "$OUT/Nocturne.app/Contents/MacOS"
swiftc -swift-version 5 -O -framework AppKit -framework UserNotifications main.swift -o "$OUT/Nocturne.app/Contents/MacOS/Nocturne"
cat > "$OUT/Nocturne.app/Contents/Info.plist" <<'PLIST'
<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>io.nocturne.companion</string><key>CFBundleName</key><string>Nocturne</string><key>CFBundleExecutable</key><string>Nocturne</string><key>CFBundleVersion</key><string>1</string><key>CFBundleShortVersionString</key><string>0.1.0</string><key>LSUIElement</key><true/><key>NSHighResolutionCapable</key><true/></dict></plist>
PLIST
codesign --force --sign - "$OUT/Nocturne.app"

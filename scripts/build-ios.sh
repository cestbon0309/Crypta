#!/bin/bash
# Builds the Go core for iOS, then the app, and packs an unsigned IPA for
# SideStore / LiveContainer.  Needs Xcode, Go and xcodegen.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
export PATH="$PATH:$(go env GOPATH)/bin"

command -v xcodegen >/dev/null || { echo "xcodegen is missing: brew install xcodegen" >&2; exit 1; }
command -v gomobile >/dev/null || go install golang.org/x/mobile/cmd/gomobile@latest golang.org/x/mobile/cmd/gobind@latest

"$ROOT/scripts/fetch-vlckit.sh"

echo "Building the Go core ..."
(cd "$ROOT/core" && gomobile bind -target=ios,iossimulator -iosversion=16.0 -ldflags="-s -w" \
  -o "$ROOT/ios/Frameworks/CryptaCore.xcframework" ./mobile)

cd "$ROOT/ios"
xcodegen generate
if [ "${1:-}" = "simulator" ]; then
  xcodebuild -project Crypta.xcodeproj -scheme Crypta -sdk iphonesimulator -configuration Debug \
    -destination 'generic/platform=iOS Simulator' -derivedDataPath build/sim build | tail -3
  echo "App: $ROOT/ios/build/sim/Build/Products/Debug-iphonesimulator/Crypta.app"
  exit 0
fi

echo "Building the app ..."
xcodebuild -project Crypta.xcodeproj -scheme Crypta -sdk iphoneos -configuration Release \
  -destination 'generic/platform=iOS' -derivedDataPath build/device \
  CODE_SIGNING_ALLOWED=NO CODE_SIGNING_REQUIRED=NO CODE_SIGN_IDENTITY="" build | tail -3

OUT="$ROOT/build"
rm -rf "$OUT/Payload" "$OUT/Crypta.ipa"
mkdir -p "$OUT/Payload"
cp -R build/device/Build/Products/Release-iphoneos/Crypta.app "$OUT/Payload/"
(cd "$OUT" && zip -qry Crypta.ipa Payload && rm -rf Payload)
echo "IPA: $OUT/Crypta.ipa ($(du -h "$OUT/Crypta.ipa" | cut -f1))"

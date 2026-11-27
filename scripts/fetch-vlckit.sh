#!/bin/bash
# Downloads the prebuilt MobileVLCKit (LGPL 2.1) used for video playback.
set -euo pipefail
VERSION="MobileVLCKit-3.7.5-a213fd11-6de05adc"
DEST="$(cd "$(dirname "$0")/.." && pwd)/ios/Frameworks"
if [ -d "$DEST/MobileVLCKit.xcframework" ]; then
  echo "MobileVLCKit already present"
  exit 0
fi
mkdir -p "$DEST"
TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT
echo "Downloading $VERSION ..."
curl -fL --retry 3 -o "$TMP/vlc.tar.xz" "https://download.videolan.org/pub/cocoapods/prod/$VERSION.tar.xz"
tar -xJf "$TMP/vlc.tar.xz" -C "$TMP"
mv "$TMP/MobileVLCKit-binary/MobileVLCKit.xcframework" "$DEST/"
echo "Installed $DEST/MobileVLCKit.xcframework"

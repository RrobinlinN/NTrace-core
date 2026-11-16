#!/bin/bash
# Builds NextTrace-ios.tipa for TrollStore (iOS 16.0+, arm64) on macOS.
#
# No Xcode project and no provisioning: the Go helper and the Objective-C
# shell are compiled directly against the iPhoneOS SDK, assembled into an
# .app bundle and ad-hoc signed with the entitlements TrollStore will keep.
#
# Usage: mobile/ios/build.sh [output-dir]
# Environment: BUILD_VERSION, BUILD_DATE, COMMIT_SHA1, VERSION_CODE (optional)

set -Eeuo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
OUT="${1:-$ROOT/dist-ios}"
MIN_IOS="16.0"

BUILD_VERSION="${BUILD_VERSION:-$(git -C "$ROOT" describe --tags --always 2>/dev/null || echo dev)}"
BUILD_DATE="${BUILD_DATE:-$(date -u +'%Y-%m-%dT%H:%M:%SZ')}"
COMMIT_SHA1="${COMMIT_SHA1:-$(git -C "$ROOT" rev-parse --short HEAD 2>/dev/null || echo unknown)}"
VERSION_CODE="${VERSION_CODE:-1}"
# CFBundleShortVersionString must be dotted integers: v1.7.6-3-gabc -> 1.7.6
VERSION_NAME="$(printf '%s' "$BUILD_VERSION" | sed -E 's/^v//; s/[^0-9.].*$//')"
case "$VERSION_NAME" in
  [0-9]*.[0-9]*) ;;
  *) VERSION_NAME="0.0.0" ;;
esac

SDK="$(xcrun --sdk iphoneos --show-sdk-path)"
MACSDK="$(xcrun --sdk macosx --show-sdk-path)"
CLANG="$(xcrun --sdk iphoneos --find clang)"
WORK="$(mktemp -d)"
trap 'rm -rf "$WORK"' EXIT
APP="$WORK/Payload/NextTrace.app"
mkdir -p "$APP" "$OUT"

echo "iPhoneOS SDK: $SDK"

# --- cgo toolchain -----------------------------------------------------------
cat > "$WORK/clangwrap" <<EOF
#!/bin/sh
exec "$CLANG" -arch arm64 -isysroot "$SDK" -miphoneos-version-min=$MIN_IOS "\$@"
EOF
chmod +x "$WORK/clangwrap"

# NextTrace captures its own TCP/UDP probes through libpcap on Darwin. iOS
# ships the library but the iPhoneOS SDK has no headers for it, so borrow the
# (identical) headers from the macOS SDK.
SHIM_INC="$WORK/include"
SHIM_LIB="$WORK/lib"
mkdir -p "$SHIM_INC" "$SHIM_LIB"
CGO_CFLAGS_EXTRA=""
CGO_LDFLAGS_EXTRA=""
if [ ! -f "$SDK/usr/include/pcap.h" ]; then
  echo "pcap headers: borrowing from the macOS SDK"
  cp "$MACSDK"/usr/include/pcap*.h "$SHIM_INC/"
  cp -R "$MACSDK/usr/include/pcap" "$SHIM_INC/"
  if [ ! -f "$SDK/usr/include/net/bpf.h" ]; then
    mkdir -p "$SHIM_INC/net"
    cp "$MACSDK/usr/include/net/bpf.h" "$SHIM_INC/net/"
  fi
  CGO_CFLAGS_EXTRA="-I$SHIM_INC"
fi
if ls "$SDK"/usr/lib/libpcap*.tbd >/dev/null 2>&1; then
  echo "libpcap: provided by the iPhoneOS SDK"
  ls -l "$SDK"/usr/lib/libpcap*.tbd
else
  # No link stub in the SDK: derive one from the macOS stub. The library
  # itself is loaded from the device at run time.
  echo "libpcap: no stub in the iPhoneOS SDK, deriving one from the macOS SDK"
  MAC_TBD="$MACSDK/usr/lib/libpcap.A.tbd"
  [ -f "$MAC_TBD" ] || MAC_TBD="$MACSDK/usr/lib/libpcap.tbd"
  sed -E 's/(arm64e?)-macos/\1-ios/g; s/x86_64-macos/x86_64-ios-simulator/g; s/(arm64e?|x86_64)-maccatalyst/\1-ios-simulator/g' \
    "$MAC_TBD" > "$SHIM_LIB/libpcap.tbd"
  head -n 12 "$SHIM_LIB/libpcap.tbd"
  CGO_LDFLAGS_EXTRA="-L$SHIM_LIB"
fi

# --- Go helper ---------------------------------------------------------------
LD_FLAGS="-X github.com/nxtrace/NTrace-core/config.Version=${BUILD_VERSION} \
          -X github.com/nxtrace/NTrace-core/config.BuildDate=${BUILD_DATE} \
          -X github.com/nxtrace/NTrace-core/config.CommitID=${COMMIT_SHA1} \
          -w -s"
(
  cd "$ROOT"
  CGO_ENABLED=1 GOOS=ios GOARCH=arm64 \
    CC="$WORK/clangwrap" \
    CGO_CFLAGS="$CGO_CFLAGS_EXTRA" \
    CGO_LDFLAGS="$CGO_LDFLAGS_EXTRA" \
    go build -trimpath -ldflags "$LD_FLAGS" -o "$APP/nexttrace" .
)
file "$APP/nexttrace"
otool -L "$APP/nexttrace"

# --- Shell app ---------------------------------------------------------------
"$CLANG" -arch arm64 -isysroot "$SDK" -miphoneos-version-min="$MIN_IOS" \
  -fobjc-arc -Os -Wall \
  -framework UIKit -framework WebKit -framework Foundation -framework CoreGraphics \
  "$HERE/main.m" -o "$APP/NextTrace"

sed -e "s/__VERSION_NAME__/${VERSION_NAME}/" -e "s/__VERSION_CODE__/${VERSION_CODE}/" \
  "$HERE/Info.plist" > "$APP/Info.plist"
plutil -lint "$APP/Info.plist"
plutil -convert binary1 "$APP/Info.plist"
printf 'APPL????' > "$APP/PkgInfo"
cp "$HERE"/icons/*.png "$APP/"

# --- Sign and package --------------------------------------------------------
# Ad-hoc signatures: TrollStore re-signs on install but keeps the entitlements.
codesign --force --sign - --entitlements "$HERE/helper.entitlements" "$APP/nexttrace"
codesign --force --sign - --entitlements "$HERE/app.entitlements" "$APP"
codesign -d --entitlements - "$APP" 2>&1 | head -n 20

(cd "$WORK" && zip -qry "$OUT/NextTrace-ios.tipa" Payload)
ls -l "$OUT/NextTrace-ios.tipa"

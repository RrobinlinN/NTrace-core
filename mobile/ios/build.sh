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

# --- Go helper ---------------------------------------------------------------
# iOS has no libpcap, so the iOS build of NextTrace contains no pcap code and
# links only system libraries (see trace/internal/probe_ios.go).
LD_FLAGS="-X github.com/nxtrace/NTrace-core/config.Version=${BUILD_VERSION} \
          -X github.com/nxtrace/NTrace-core/config.BuildDate=${BUILD_DATE} \
          -X github.com/nxtrace/NTrace-core/config.CommitID=${COMMIT_SHA1} \
          -w -s"
(
  cd "$ROOT"
  CGO_ENABLED=1 GOOS=ios GOARCH=arm64 \
    CC="$WORK/clangwrap" \
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

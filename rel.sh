#!/bin/bash
set -e
cd "$(dirname "$0")"
[ -f twemoji/svg.zip ] || ./twemoji/fetch.sh
export PATH=$PATH:/usr/local/go/bin
GOOS=js GOARCH=wasm go build -buildvcs=false -trimpath -ldflags="-s -w" -o pwa/ygg.wasm ./wasm
mkdir -p pwa/dl
# gb <output> <GOOS> <GOARCH> [GOARM]: static native build (no cgo).
gb() { CGO_ENABLED=0 GOOS=$2 GOARCH=$3 GOARM=$4 go build -trimpath -ldflags="-s -w -checklinkname=0" -o "$1" .; }
gb pwa/dl/yggchat-linux-amd64 linux amd64
gb pwa/dl/yggchat-linux-arm64 linux arm64
gb pwa/dl/yggchat-linux-armv7 linux arm 7
gb pwa/dl/yggchat-windows-amd64.exe windows amd64
gb pwa/dl/yggchat-windows-arm64.exe windows arm64
gb pwa/dl/yggchat-darwin-amd64 darwin amd64
gb pwa/dl/yggchat-darwin-arm64 darwin arm64
gb pwa/dl/yggchat-freebsd-amd64 freebsd amd64
gb pwa/dl/yggchat-android-arm64 android arm64

# APK native libs: dist/jni/<abi>/libyggchat.so (android/build.sh packs every ABI found there).
# android/arm64 links internally; the other Android ABIs need cgo + the NDK's clang.
rm -rf dist/jni && mkdir -p dist/jni/arm64-v8a
cp pwa/dl/yggchat-android-arm64 dist/jni/arm64-v8a/libyggchat.so
cp pwa/dl/yggchat-android-arm64 android/libyggchat.so
SDK=${ANDROID_HOME:-$HOME/android/sdk}
NDK=${ANDROID_NDK_HOME:-${ANDROID_NDK_LATEST_HOME:-$(ls -d "$SDK"/ndk/* 2>/dev/null | sort -V | tail -1)}}
TC=$NDK/toolchains/llvm/prebuilt/linux-x86_64/bin
if [ -n "$NDK" ] && [ -x "$TC/x86_64-linux-android24-clang" ]; then
  # na <abi> <GOARCH> <clang target> [GOARM]
  na() { mkdir -p dist/jni/$1; CGO_ENABLED=1 CC=$TC/$3-clang GOOS=android GOARCH=$2 GOARM=$4 go build -trimpath -ldflags="-s -w -checklinkname=0" -o dist/jni/$1/libyggchat.so .; }
  na armeabi-v7a arm armv7a-linux-androideabi24 7
  na x86_64 amd64 x86_64-linux-android24
  na x86 386 i686-linux-android24
elif [ -n "$REQUIRE_NDK" ]; then
  echo "rel.sh: Android NDK not found (set ANDROID_NDK_HOME)" >&2; exit 1
else
  echo "rel.sh: warning: no Android NDK, the APK gets arm64-v8a only" >&2
fi
./android/build.sh >/dev/null
cp android/out/yggchat.apk pwa/dl/yggchat.apk
rm -rf pwa/twemoji && mkdir -p pwa/twemoji && unzip -q twemoji/svg.zip -d pwa/twemoji
cp README.md LICENSE THIRD_PARTY_NOTICES.txt pwa/dl/
(cd pwa/dl && rm -f SHA256SUMS && sha256sum $(ls) > SHA256SUMS && cat SHA256SUMS)
go version -m pwa/dl/yggchat-linux-amd64 | grep vcs || true

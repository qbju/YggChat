#!/bin/bash
set -e
cd ~/jobs/yggchat
export PATH=$PATH:/usr/local/go/bin
F="-trimpath -ldflags=-s -ldflags=-w"
GOOS=js GOARCH=wasm go build -buildvcs=false -trimpath -ldflags="-s -w" -o pwa/ygg.wasm ./wasm
mkdir -p pwa/dl
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w -checklinkname=0" -o pwa/dl/yggchat-linux-amd64 .
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags="-s -w -checklinkname=0" -o pwa/dl/yggchat-windows-amd64.exe .
CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -trimpath -ldflags="-s -w -checklinkname=0" -o pwa/dl/yggchat-android-arm64 .
cp pwa/dl/yggchat-android-arm64 android/libyggchat.so
./android/build.sh >/dev/null
cp android/out/yggchat.apk pwa/dl/yggchat.apk
rm -rf pwa/twemoji && mkdir -p pwa/twemoji && unzip -q twemoji/svg.zip -d pwa/twemoji
cp README.txt LICENSE THIRD_PARTY_NOTICES.txt pwa/dl/
(cd pwa/dl && sha256sum yggchat.apk yggchat-android-arm64 yggchat-linux-amd64 yggchat-windows-amd64.exe README.txt LICENSE THIRD_PARTY_NOTICES.txt > SHA256SUMS && cat SHA256SUMS)
go version -m pwa/dl/yggchat-linux-amd64 | grep vcs

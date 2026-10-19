#!/bin/sh
# Builds twemoji/svg.zip (flat *.svg) from jdecked/twemoji, which go:embed needs.
set -e
V=${TWEMOJI_VERSION:-17.0.3}
cd "$(dirname "$0")"
[ -f svg.zip ] && exit 0
T=$(mktemp -d)
curl -fsSL "https://github.com/jdecked/twemoji/archive/refs/tags/v$V.tar.gz" | tar -xz -C "$T" --wildcards '*/assets/svg/*'
(cd "$T"/twemoji-*/assets/svg && zip -q -X -9 "$OLDPWD/svg.zip" *.svg)
rm -rf "$T"
echo "twemoji/svg.zip: $(unzip -l svg.zip | tail -1)"

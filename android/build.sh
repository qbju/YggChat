#!/bin/bash
set -e
SDK=$HOME/android/sdk; BT=$SDK/build-tools/36.0.0; JAR=$SDK/platforms/android-34/android.jar
cd "$(dirname "$0")"; rm -rf out; mkdir -p out/classes out/apk/lib/arm64-v8a
$BT/aapt2 compile --dir res -o out/res.zip
$BT/aapt2 link -o out/base.apk -I $JAR --manifest AndroidManifest.xml out/res.zip --java out/gen
javac --release 11 -nowarn -classpath $JAR -d out/classes $(find src out/gen -name '*.java') 2>&1 | grep -v "^warning\|^Note" || true
$BT/d8 --min-api 24 --lib $JAR --output out $(find out/classes -name '*.class')
cp out/base.apk out/unsigned.apk
cp libyggchat.so out/apk/lib/arm64-v8a/
(cd out && zip -q unsigned.apk classes.dex && cd apk && zip -q -0 ../unsigned.apk lib/arm64-v8a/libyggchat.so)
$BT/zipalign -f -p 4 out/unsigned.apk out/aligned.apk
[ -f ~/android/yggchat.jks ] || keytool -genkeypair -keystore ~/android/yggchat.jks -alias ygg -keyalg RSA -keysize 2048 -validity 10000 -storepass yggchat -keypass yggchat -dname "CN=YggChat"
$BT/apksigner sign --ks ~/android/yggchat.jks --ks-pass pass:yggchat --out out/yggchat.apk out/aligned.apk
$BT/apksigner verify out/yggchat.apk && ls -la out/yggchat.apk

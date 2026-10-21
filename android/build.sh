#!/bin/bash
set -e
SDK=${ANDROID_HOME:-$HOME/android/sdk}; BT=$SDK/build-tools/${BUILD_TOOLS:-36.0.0}; JAR=$SDK/platforms/android-34/android.jar
KS=${YGG_KEYSTORE:-$HOME/android/yggchat.jks}; export KP=${YGG_KS_PASS:-yggchat}
cd "$(dirname "$0")"; rm -rf out; mkdir -p out/classes out/apk/lib
$BT/aapt2 compile --dir res -o out/res.zip
$BT/aapt2 link -o out/base.apk -I $JAR --manifest AndroidManifest.xml out/res.zip --java out/gen
# A javac error must stop the build (it used to be filtered away by grep || true).
if ! javac --release 11 -nowarn -classpath $JAR -d out/classes $(find src out/gen -name '*.java') > out/javac.log 2>&1; then
  cat out/javac.log >&2; echo "build.sh: javac failed" >&2; exit 1
fi
grep -v "^warning\|^Note" out/javac.log >&2 || true
$BT/d8 --min-api 24 --lib $JAR --output out $(find out/classes -name '*.class')
cp out/base.apk out/unsigned.apk
# Native libs: every ABI in ${YGG_JNI:-../dist/jni}/<abi>/libyggchat.so, else ./libyggchat.so as arm64-v8a.
JNI=${YGG_JNI:-../dist/jni}
if ls "$JNI"/*/libyggchat.so >/dev/null 2>&1; then cp -r "$JNI"/. out/apk/lib/
else mkdir -p out/apk/lib/arm64-v8a; cp libyggchat.so out/apk/lib/arm64-v8a/; fi
# extractNativeLibs="true", so the libs may be deflated (the installer extracts them).
(cd out && zip -q unsigned.apk classes.dex && cd apk && zip -q -9 -r ../unsigned.apk lib)
echo "ABIs: $(ls out/apk/lib | tr '\n' ' ')" >&2
$BT/zipalign -f -p 4 out/unsigned.apk out/aligned.apk
[ -f "$KS" ] || { mkdir -p "$(dirname "$KS")"; keytool -genkeypair -keystore "$KS" -alias ygg -keyalg RSA -keysize 2048 -validity 10000 -storepass "$KP" -keypass "$KP" -dname "CN=YggChat"; }
$BT/apksigner sign --ks "$KS" --ks-pass env:KP --out out/yggchat.apk out/aligned.apk
$BT/apksigner verify out/yggchat.apk && ls -la out/yggchat.apk

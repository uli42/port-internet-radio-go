#!/usr/bin/env bash
# Builds the Android APK:
#   1. Cross-compiles the Go server for Android ARM64
#   2. Invokes Gradle to assemble the debug APK
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

echo "==> Building Go server for Android (arm64)"
make -C "$REPO_ROOT" build-android

echo "==> Assembling Android APK"
cd "$REPO_ROOT/android"
if [ -x ./gradlew ]; then
  ./gradlew assembleDebug
else
  gradle assembleDebug
fi

echo "==> APK ready:"
ls -lh "$REPO_ROOT/android/app/build/outputs/apk/debug/app-debug.apk"

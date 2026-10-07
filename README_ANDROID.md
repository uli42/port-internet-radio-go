# Android app — SlimProto Server

Bundles the Go server as an Android binary and runs it in a foreground
service. Player apps on the same device (e.g. **SqueezeCtrl**) then
connect to the local server at `localhost:3483` (SlimProto), with the
web/CometD UI on `localhost:9000` and the CLI on `localhost:9090`.

## Requirements

**Host machine**
- Linux or macOS (the `make` step uses GNU make; on Windows use WSL or
  build the APK in Android Studio)
- [Go](https://go.dev/dl/) 1.19+ — only needed to (re)build the server binary
- [JDK 17](https://adoptium.net/) — required by Android Gradle Plugin 8.5
- [Android SDK](https://developer.android.com/studio#command-tools):
  `cmdline-tools` (latest), `platforms;android-35`, `build-tools;35.0.0`
- [Gradle](https://gradle.org/install/) 8.9 — or just use Android Studio,
  which bundles Gradle

**Device**
- Android 11 (API 30) or newer — e.g. LineageOS 18
- Architecture: arm64-v8a

## Setup (one-time)

```bash
# Point to the SDK and add its tools to PATH
export ANDROID_HOME=$HOME/Android/Sdk
export PATH=$PATH:$ANDROID_HOME/cmdline-tools/latest/bin:$ANDROID_HOME/platform-tools

# Accept licenses (required before Gradle can build)
yes | sdkmanager --licenses

# Install SDK packages (if not already present)
sdkmanager "platforms;android-35" "build-tools;35.0.0" "cmdline-tools;latest"

# Generate the Gradle wrapper (only needed if android/gradlew is missing
# its wrapper jar — opening android/ in Android Studio does this for you)
cd android && gradle wrapper && cd ..
```

## Build

```bash
# One command (runs both steps):
./build-android.sh

# ...or step by step:

# 1. Cross-compile the Go server for Android ARM64
make build-android
#    -> bin/server_android_arm64   (~5.5 MB, statically linked)

# 2. Assemble the APK
#    (the Gradle preBuild task automatically copies the binary and
#     presets.json into the app's assets)
cd android
./gradlew assembleDebug        # debug APK (fastest, self-signed)
# ./gradlew assembleRelease    # release APK (needs a signing key)
cd ..

# Output:
#   android/app/build/outputs/apk/debug/app-debug.apk
```

## Install / run

```bash
adb install android/app/build/outputs/apk/debug/app-debug.apk
```

On the device:
1. Open **"SlimProto Server"** and tap **Start Server** (a persistent
   notification appears; the server now listens on
   `localhost:3483 / 9000 / 9090`).
2. In **SqueezeCtrl** (or another LMS player), add a server at
   host=`localhost`, port=`3483`.
3. Optionally enable **Auto-start on boot** (OFF by default).

## Notes

- The server binary is statically linked (CGO disabled) and has no
  external shared-library dependencies, so it runs on stock Android.
- UPX compression is skipped for the Android target (UPX cannot pack this
  Go binary), so the binary ships uncompressed (~5.5 MB).
- `presets.json` is copied into the app's private storage on first launch
  and preserved across app updates (so station presets survive).

## Troubleshooting

- `sdkmanager: command not found` — install Android cmdline-tools and add
  `$ANDROID_HOME/cmdline-tools/latest/bin` to PATH.
- `Unsupported class file major version` — install/use JDK 17
  (`export JAVA_HOME=/path/to/jdk17`).
- Gradle wrapper jar missing — open `android/` in Android Studio, or run
  `gradle wrapper` inside `android/`.
- `make: python3: command not found` — the `generate` step needs python3
  (`apt install python3`).
- Bundled binary is ~5.5 MB — expected; see the UPX note above.

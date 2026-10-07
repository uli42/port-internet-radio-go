plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
}

android {
    namespace = "slimproto.server.android"
    compileSdk = 35

    defaultConfig {
        applicationId = "slimproto.server.android"
        minSdk = 30
        targetSdk = 34
        versionCode = 1
        versionName = "1.0"
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            proguardFiles(getDefaultProguardFile("proguard-android-optimize.txt"), "proguard-rules.pro")
        }
    }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    kotlinOptions {
        jvmTarget = "17"
    }
}

// Path to the repository root (one level above this android/ project).
val repoRoot = rootProject.projectDir.parentFile

// Copy the cross-compiled Go server binary into the APK assets before building.
val serverBinary = repoRoot.resolve("bin/server_android_arm64")
tasks.register<Copy>("copyServerBinary") {
    description = "Copies the Go server binary from bin/ into app assets"
    from(serverBinary)
    into(layout.projectDirectory.dir("src/main/assets"))
    onlyIf { serverBinary.exists() }
    doLast {
        if (serverBinary.exists()) {
            println("Bundled server binary: ${serverBinary.name}")
        } else {
            println("WARNING: $serverBinary not found. Run `make build-android` first.")
        }
    }
}

// Copy the default presets.json into the APK assets.
val presetsJson = repoRoot.resolve("presets.json")
tasks.register<Copy>("copyPresetsJson") {
    description = "Copies presets.json from the repository root into app assets"
    from(presetsJson)
    into(layout.projectDirectory.dir("src/main/assets"))
    onlyIf { presetsJson.exists() }
}

tasks.named("preBuild") {
    dependsOn("copyServerBinary", "copyPresetsJson")
}

dependencies {
    implementation("androidx.core:core-ktx:1.13.1")
    implementation("androidx.appcompat:appcompat:1.7.0")
    implementation("com.google.android.material:material:1.12.0")
}

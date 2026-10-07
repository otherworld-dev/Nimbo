// Build script for the :app module — the whole Nimbo Android application.
//
// Notes that matter:
//  * The Go sync engine ships as a local `.aar` (`../core/nimbo-core.aar`,
//    generated Java package `dev.otherworld.mobile`). It is arm64-only, so the
//    APK is restricted to `arm64-v8a`; adding another ABI would produce an APK
//    that installs and then dies on the first JNI call. An x86_64 core is not a
//    quick fix for x86 devices either: modernc's SQLite issues legacy x86_64
//    syscalls (lstat, from musl's fstatat) that Android's seccomp filter kills,
//    so it crashes as soon as a folder is added. Play distribution to ChromeOS
//    is switched off until that is sorted.
//  * minSdk 26 matches `gomobile bind -androidapi 26`. targetSdk 36 is Google
//    Play's floor for new apps and updates since 31 Aug 2026.
//  * The `play` release build is signed with the Play upload key from the
//    gitignored `keystore.properties` + `upload-keystore.jks` (Google holds the
//    app signing key). Without them it is unsigned, so CI and other checkouts
//    still build. The `direct` build never uses the upload key.
//  * Minification is off for the prototype: R8 plus a JNI boundary needs keep
//    rules we are not writing yet (see proguard-rules.pro for the ones we would
//    need when minification is switched on).
//  * The dependency list below is the complete, pinned set from the
//    implementation contract — do not add to it, apart from the Play Billing Library on playImplementation.

import java.io.FileInputStream
import java.util.Properties

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.compose")
    id("org.jetbrains.kotlin.plugin.serialization")
}

android {
    namespace = "dev.otherworld.nimbo"
    compileSdk = 36

    val keystorePropertiesFile = rootProject.file("keystore.properties")
    val keystoreProperties = Properties()
    if (keystorePropertiesFile.exists()) {
        keystoreProperties.load(FileInputStream(keystorePropertiesFile))
    }

    signingConfigs {
        create("release") {
            if (keystorePropertiesFile.exists()) {
                storeFile = rootProject.file(keystoreProperties["storeFile"] as String)
                storePassword = keystoreProperties["storePassword"] as String
                keyAlias = keystoreProperties["keyAlias"] as String
                keyPassword = keystoreProperties["keyPassword"] as String
            }
        }
    }

    defaultConfig {
        applicationId = "dev.otherworld.nimbo"
        minSdk = 26
        targetSdk = 36
        versionCode = 2
        versionName = "0.1.0"

        // The Go core's libgojni.so is built for arm64 only.
        ndk {
            abiFilters += "arm64-v8a"
        }
    }

    buildTypes {
        debug {
            isMinifyEnabled = false
        }
        release {
            isMinifyEnabled = false
            isShrinkResources = false
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro",
            )
        }
    }

    // Where the app is distributed decides how supporters pay. Google Play's
    // payment policy allows only Play Billing in the Play build; everywhere
    // else (GitHub releases, the self-hosted F-Droid repo) supporters pay
    // through Otherworld's own checkout and get a key. Each flavour carries
    // only its own payment code, so the direct build has no Google libraries
    // and the Play build can never show an outside payment link.
    //
    // Both keep the same applicationId. They are signed with different keys
    // (Play App Signing vs our own), so one can't update the other; switching
    // means uninstalling first, which also stops a sideloaded build silently
    // replacing a Play install.
    flavorDimensions += "distribution"
    productFlavors {
        create("play") {
            dimension = "distribution"
            // Debug keeps the debug key: a build type's signing config wins
            // over a flavour's, so this only reaches playRelease.
            if (keystorePropertiesFile.exists()) {
                signingConfig = signingConfigs.getByName("release")
            }
        }
        create("direct") { dimension = "distribution" }
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    kotlinOptions {
        jvmTarget = "17"
    }

    buildFeatures {
        compose = true
    }

    packaging {
        resources {
            excludes += "/META-INF/{AL2.0,LGPL2.1}"
        }
    }
}

dependencies {
    // The Go sync engine (gomobile bind output). Path is relative to this module.
    implementation(files("../core/nimbo-core.aar"))

    implementation("androidx.core:core-ktx:1.13.1")
    implementation("androidx.lifecycle:lifecycle-runtime-ktx:2.8.7")
    implementation("androidx.lifecycle:lifecycle-runtime-compose:2.8.7")
    implementation("androidx.lifecycle:lifecycle-viewmodel-compose:2.8.7")
    implementation("androidx.lifecycle:lifecycle-service:2.8.7")
    implementation("androidx.activity:activity-compose:1.9.3")
    implementation("androidx.browser:browser:1.8.0")
    implementation("androidx.work:work-runtime-ktx:2.9.1")
    implementation("org.jetbrains.kotlinx:kotlinx-serialization-json:1.7.3")

    implementation(platform("androidx.compose:compose-bom:2024.10.01"))
    implementation("androidx.compose.ui:ui")
    implementation("androidx.compose.ui:ui-graphics")
    implementation("androidx.compose.ui:ui-tooling-preview")
    implementation("androidx.compose.material3:material3")
    implementation("androidx.compose.material:material-icons-extended")

    // Thumbnails. Coil is fed by a custom Fetcher that calls the Go facade, so
    // previews use the engine's authenticated transport rather than opening a
    // second HTTP path with its own copy of the credentials.
    implementation("io.coil-kt:coil-compose:2.7.0")

    // Pulled in transitively by compose-ui, but the BOM's 1.0.1 ships a
    // libandroidx.graphics.path.so that is not 16 KB page-aligned, which Android
    // 15+ devices flag at install time (and Play requires). 1.1.0 is aligned.
    implementation("androidx.graphics:graphics-path:1.1.0")

    // Google Play Billing, Play build only (see productFlavors). The one
    // exception to the pinned list above: Play's payment policy makes it
    // mandatory for in-app support in the Play build, and the direct build
    // must not contain it.
    "playImplementation"("com.android.billingclient:billing:8.0.0")

    // Play Billing brings in androidx.fragment 1.1.0 (via play-services-basement).
    // The ActivityResult API needs Fragment 1.3.0+ wherever Fragment is on the
    // classpath, and release lint fails the build otherwise. A constraint only
    // raises the version Billing already pulls in; it adds no library, and the
    // direct build still has no Fragment at all.
    constraints {
        add("playImplementation", "androidx.fragment:fragment:1.8.5")
    }

    debugImplementation("androidx.compose.ui:ui-tooling")

    // Plain JVM unit tests for the pure logic in core/ (no Android framework
    // types involved, so these run on the host without Robolectric).
    testImplementation("junit:junit:4.13.2")
}

// Build script for the :app module — the whole Nimbo Android application.
//
// Notes that matter:
//  * The Go sync engine ships as a local `.aar` (`../core/nimbo-core.aar`,
//    generated Java package `dev.otherworld.mobile`). It is arm64-only, so the
//    APK is restricted to `arm64-v8a`; adding another ABI would produce an APK
//    that installs and then dies on the first JNI call.
//  * minSdk 26 matches `gomobile bind -androidapi 26`.
//  * Minification is off for the prototype: R8 plus a JNI boundary needs keep
//    rules we are not writing yet (see proguard-rules.pro for the ones we would
//    need when minification is switched on).
//  * The dependency list below is the complete, pinned set from the
//    implementation contract — do not add to it.

plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.compose")
    id("org.jetbrains.kotlin.plugin.serialization")
}

android {
    namespace = "dev.otherworld.nimbo"
    compileSdk = 35

    defaultConfig {
        applicationId = "dev.otherworld.nimbo"
        minSdk = 26
        targetSdk = 35
        versionCode = 1
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

    debugImplementation("androidx.compose.ui:ui-tooling")

    // Plain JVM unit tests for the pure logic in core/ (no Android framework
    // types involved, so these run on the host without Robolectric).
    testImplementation("junit:junit:4.13.2")
}

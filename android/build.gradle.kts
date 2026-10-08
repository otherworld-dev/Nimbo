// Root build script.
//
// Only pins the plugin versions used by the :app module; the root project
// itself applies nothing. Versions are locked by the implementation contract:
// AGP 8.9.3 (the oldest that compiles against API 36, which Google Play
// requires apps to target since 31 Aug 2026), Kotlin 2.0.21 (compiler +
// compose + serialization plugins), and Gradle 8.11.1 (set by the checked-in
// wrapper).

plugins {
    id("com.android.application") version "8.9.3" apply false
    id("org.jetbrains.kotlin.android") version "2.0.21" apply false
    id("org.jetbrains.kotlin.plugin.compose") version "2.0.21" apply false
    id("org.jetbrains.kotlin.plugin.serialization") version "2.0.21" apply false
}

# ProGuard/R8 rules for :app.
#
# Minification is currently DISABLED (see app/build.gradle.kts) because the
# gomobile JNI boundary is reflection-heavy and mis-shrinking it fails at
# runtime rather than at build time. These rules are kept here so that turning
# `isMinifyEnabled = true` on later is a one-line change rather than a debugging
# session.
#
# Why each rule exists:
#  * `go.**` and `dev.otherworld.mobile.**` are looked up by name from native
#    code; renaming or removing any of it breaks the bind at runtime.
#  * Our Kotlin implementations of the generated `Listener` / `SecretStore`
#    interfaces are only ever invoked from Go, so R8 sees no Java caller and
#    would happily strip the methods.
#  * kotlinx.serialization generates synthetic `Companion.serializer()` members
#    that are also only reached reflectively.

# --- gomobile runtime + generated bindings ---------------------------------
-keep class go.** { *; }
-keep class dev.otherworld.mobile.** { *; }
-keepclasseswithmembernames class * {
    native <methods>;
}

# --- our implementations of the Go-facing callback interfaces --------------
-keep class * implements dev.otherworld.mobile.Listener { *; }
-keep class * implements dev.otherworld.mobile.SecretStore { *; }

# --- kotlinx.serialization -------------------------------------------------
-keepattributes *Annotation*, InnerClasses, Signature
-dontnote kotlinx.serialization.**
-keepclassmembers class dev.otherworld.nimbo.** {
    *** Companion;
}
-keepclasseswithmembers class dev.otherworld.nimbo.** {
    kotlinx.serialization.KSerializer serializer(...);
}

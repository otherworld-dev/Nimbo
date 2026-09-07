// Gradle settings for the Nimbo Android app.
//
// Declares where plugins and dependencies are resolved from and which modules
// make up the build. `PREFER_SETTINGS` (rather than FAIL_ON_PROJECT_REPOS) is
// deliberate: the :app module also consumes the Go core as a local file
// dependency (`../core/nimbo-core.aar`) and we do not want repository-mode
// enforcement getting in the way of that.

pluginManagement {
    repositories {
        google {
            content {
                includeGroupByRegex("com\\.android.*")
                includeGroupByRegex("com\\.google.*")
                includeGroupByRegex("androidx.*")
            }
        }
        mavenCentral()
        gradlePluginPortal()
    }
}

dependencyResolutionManagement {
    repositoriesMode.set(RepositoriesMode.PREFER_SETTINGS)
    repositories {
        google()
        mavenCentral()
    }
}

rootProject.name = "Nimbo"
include(":app")

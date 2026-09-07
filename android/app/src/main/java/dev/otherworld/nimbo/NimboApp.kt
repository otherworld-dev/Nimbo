/*
 * NimboApp.kt — the Application entry point (android:name=".NimboApp").
 *
 * Does the things that must happen exactly once per process and before any
 * screen or service runs: create the Go core client, register the notification
 * channels, and supply the image loader that knows how to fetch thumbnails
 * through the engine. All cheap and offline — no network, no disk scan.
 */
package dev.otherworld.nimbo

import android.app.Application
import coil.ImageLoader
import coil.ImageLoaderFactory
import dev.otherworld.nimbo.core.NimboCore
import dev.otherworld.nimbo.core.nimboImageLoader
import dev.otherworld.nimbo.service.Notifications

class NimboApp : Application(), ImageLoaderFactory {

    override fun onCreate() {
        super.onCreate()
        NimboCore.init(this)
        Notifications.ensureChannels(this)
    }

    /** Coil's singleton loader, taught to resolve PreviewRequest via the facade. */
    override fun newImageLoader(): ImageLoader = nimboImageLoader(this)
}

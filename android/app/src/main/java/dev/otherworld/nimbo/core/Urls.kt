/*
 * Urls.kt — resolving server-relative URLs handed to us by the Nextcloud API.
 *
 * The OCS navigation endpoint returns each app's href (and icon) relative to the
 * server root, e.g. "/index.php/apps/files/". Those have to be joined to the
 * account's server URL before anything can open them; the desktop client does the
 * same in its own consumer layer (cmd/nimbo-gui/service.go absURL) rather than in
 * the transport, so the raw API value stays raw.
 */
package dev.otherworld.nimbo.core

/**
 * Resolves [href] against [serverUrl].
 *
 * Already-absolute hrefs (anything carrying a scheme) and blank ones are returned
 * untouched, as is any href we cannot resolve because no server URL is known —
 * better to hand back what the server said than to invent a wrong URL.
 */
fun absoluteUrl(serverUrl: String, href: String): String {
    val target = href.trim()
    if (target.isEmpty() || target.contains("://")) return target
    val base = serverUrl.trim().trimEnd('/')
    if (base.isEmpty()) return target
    return base + "/" + target.trimStart('/')
}

/**
 * Whether [url] is an absolute http(s) link, the only kind a server-supplied link
 * (a notification's WEB action) is allowed to hand the system to open: never an
 * intent:, file: or other app's scheme (GitHub #20). Same rule as the desktop's
 * isWebLink.
 */
fun isWebLink(url: String): Boolean {
    val scheme = url.substringBefore("://", missingDelimiterValue = "")
    if (!scheme.equals("https", ignoreCase = true) && !scheme.equals("http", ignoreCase = true)) return false
    return url.substringAfter("://").substringBefore('/').isNotEmpty()
}

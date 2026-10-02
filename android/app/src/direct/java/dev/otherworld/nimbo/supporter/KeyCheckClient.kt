/*
 * KeyCheckClient.kt — the only code in the direct build that talks to
 * Otherworld's servers, and only ever with a key the user added.
 */
package dev.otherworld.nimbo.supporter

import android.util.Log
import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.jsonObject
import kotlinx.serialization.json.jsonPrimitive
import java.net.HttpURLConnection
import java.net.URL

interface KeyChecker {
    suspend fun check(key: String): CheckOutcome

    /** A short-lived billing-portal URL for this key, or null. */
    suspend fun portalUrl(key: String): String?
}

class KeyCheckClient(private val baseUrl: String = "https://api.otherworld.dev") : KeyChecker {

    override suspend fun check(key: String): CheckOutcome = withContext(Dispatchers.IO) {
        try {
            request("GET", "/v1/nimbo/supporter", key) { code, body -> outcomeOf(code, body) }
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            // Offline, DNS, TLS, timeout: no answer, so nothing changes.
            Log.i(TAG, "supporter check unreachable: ${e.javaClass.simpleName}")
            CheckOutcome.Unreachable
        }
    }

    override suspend fun portalUrl(key: String): String? = withContext(Dispatchers.IO) {
        try {
            request("POST", "/billing/portal", key) { code, body ->
                if (code != 200) {
                    Log.i(TAG, "portal refused: $code")
                    null
                } else {
                    runCatching {
                        Json.parseToJsonElement(body.orEmpty()).jsonObject["url"]?.jsonPrimitive?.content
                    }.getOrNull()
                }
            }
        } catch (e: CancellationException) {
            throw e
        } catch (e: Exception) {
            Log.i(TAG, "portal unreachable: ${e.javaClass.simpleName}")
            null
        }
    }

    private fun <T> request(method: String, path: String, key: String, read: (Int, String?) -> T): T {
        val conn = URL(baseUrl + path).openConnection() as HttpURLConnection
        try {
            conn.requestMethod = method
            conn.connectTimeout = TIMEOUT_MS
            conn.readTimeout = TIMEOUT_MS
            conn.setRequestProperty("Authorization", "Bearer $key")
            conn.setRequestProperty("Accept", "application/json")
            if (method == "POST") {
                // An explicit empty body: some proxies answer 411 to a POST
                // with no Content-Length.
                conn.doOutput = true
                conn.setFixedLengthStreamingMode(0)
                conn.outputStream.close()
            }
            val code = conn.responseCode
            val stream = if (code < 400) conn.inputStream else conn.errorStream
            val body = stream?.bufferedReader()?.use { it.readText() }
            return read(code, body)
        } finally {
            conn.disconnect()
        }
    }

    private companion object {
        const val TAG = "NimboSupporter"
        const val TIMEOUT_MS = 15_000
    }
}

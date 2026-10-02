/*
 * KeyListStore.kt — supporter keys at rest, encrypted with the same
 * non-exportable Keystore key as account passwords.
 */
package dev.otherworld.nimbo.supporter

import dev.otherworld.nimbo.core.KeystoreSecretStore
import kotlinx.serialization.decodeFromString
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json

interface KeyListStore {
    fun load(): List<StoredKey>
    fun save(keys: List<StoredKey>)
}

class EncryptedKeyListStore(private val secrets: KeystoreSecretStore) : KeyListStore {

    /** Absent, undecryptable (a restored backup) or unreadable all mean "no keys". */
    override fun load(): List<StoredKey> =
        runCatching { json.decodeFromString<List<StoredKey>>(secrets.get(SLOT)) }.getOrDefault(emptyList())

    override fun save(keys: List<StoredKey>) {
        if (keys.isEmpty()) secrets.delete(SLOT) else secrets.set(SLOT, json.encodeToString(keys))
    }

    private companion object {
        /** Can't collide with an account id, which is never wrapped in underscores. */
        const val SLOT = "__supporter_keys__"
        val json = Json { ignoreUnknownKeys = true }
    }
}

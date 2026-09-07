/*
 * KeystoreSecretStore.kt — the SecretStore the Go core calls to persist app
 * passwords. AES-256/GCM with a non-exportable AndroidKeyStore key; ciphertext
 * lives in a private SharedPreferences file as Base64(iv || ciphertext).
 *
 * HARD RULE (MOBILE_API.md): no method may throw. gomobile emits no exception
 * check for these callbacks, so an escaping Kotlin exception kills the process.
 * Every body is wrapped in runCatching: get() degrades to "" (absent), and
 * set()/delete() swallow. Absence is success for delete().
 */
package dev.otherworld.nimbo.core

import android.content.Context
import android.content.SharedPreferences
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.util.Base64
import android.util.Log
import dev.otherworld.mobile.SecretStore
import java.security.KeyStore
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

class KeystoreSecretStore(context: Context) : SecretStore {

    private val prefs: SharedPreferences =
        context.applicationContext.getSharedPreferences(PREFS_NAME, Context.MODE_PRIVATE)

    // -- SecretStore ---------------------------------------------------------

    /** Returns the stored secret, or "" when absent or undecryptable. Never throws. */
    override fun get(accountID: String): String {
        return runCatching {
            val key = prefKey(accountID)
            val blob = prefs.getString(key, null)
            if (blob.isNullOrEmpty()) return@runCatching ""
            val decrypted: String? = runCatching { decrypt(blob) }.getOrElse { e ->
                // A corrupt or un-decryptable entry (e.g. restored to a device
                // whose Keystore key did not come with it) must not brick
                // sign-in: drop it and report "absent".
                Log.w(TAG, "dropping undecryptable secret", e)
                runCatching { prefs.edit().remove(key).commit() }
                null
            }
            decrypted ?: ""
        }.getOrElse { e ->
            Log.e(TAG, "get failed", e)
            ""
        }
    }

    /** Stores (encrypts) the secret. Never throws; failures are logged only. */
    override fun set(accountID: String, secret: String) {
        runCatching {
            val blob = encrypt(secret)
            prefs.edit().putString(prefKey(accountID), blob).commit()
        }.onFailure { e ->
            Log.e(TAG, "set failed", e)
        }
    }

    /** Removes the secret. An absent entry is success. Never throws. */
    override fun delete(accountID: String) {
        runCatching {
            prefs.edit().remove(prefKey(accountID)).commit()
        }.onFailure { e ->
            Log.e(TAG, "delete failed", e)
        }
    }

    // -- crypto --------------------------------------------------------------

    private fun encrypt(plain: String): String {
        val cipher = Cipher.getInstance(TRANSFORMATION)
        cipher.init(Cipher.ENCRYPT_MODE, secretKey())
        val iv = cipher.iv
        val ciphertext = cipher.doFinal(plain.toByteArray(Charsets.UTF_8))
        val out = ByteArray(iv.size + ciphertext.size)
        System.arraycopy(iv, 0, out, 0, iv.size)
        System.arraycopy(ciphertext, 0, out, iv.size, ciphertext.size)
        return Base64.encodeToString(out, Base64.NO_WRAP)
    }

    private fun decrypt(blob: String): String {
        val raw = Base64.decode(blob, Base64.NO_WRAP)
        require(raw.size > IV_LENGTH) { "secret blob too short" }
        val iv = raw.copyOfRange(0, IV_LENGTH)
        val ciphertext = raw.copyOfRange(IV_LENGTH, raw.size)
        val cipher = Cipher.getInstance(TRANSFORMATION)
        cipher.init(Cipher.DECRYPT_MODE, secretKey(), GCMParameterSpec(GCM_TAG_BITS, iv))
        return String(cipher.doFinal(ciphertext), Charsets.UTF_8)
    }

    /** Loads the AndroidKeyStore key, creating it on first use. */
    private fun secretKey(): SecretKey {
        val keyStore = KeyStore.getInstance(ANDROID_KEYSTORE).apply { load(null) }
        val existing = keyStore.getKey(KEY_ALIAS, null)
        if (existing is SecretKey) return existing

        val generator = KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, ANDROID_KEYSTORE)
        val spec = KeyGenParameterSpec.Builder(
            KEY_ALIAS,
            KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT,
        )
            .setBlockModes(KeyProperties.BLOCK_MODE_GCM)
            .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
            .setKeySize(256)
            // The engine must work with the screen off / device locked.
            .setUserAuthenticationRequired(false)
            .setRandomizedEncryptionRequired(true)
            .build()
        generator.init(spec)
        return generator.generateKey()
    }

    private fun prefKey(accountID: String): String = PREF_KEY_PREFIX + accountID

    private companion object {
        const val TAG = "NimboSecrets"
        const val PREFS_NAME = "nimbo_secrets"
        const val PREF_KEY_PREFIX = "acct_"
        const val ANDROID_KEYSTORE = "AndroidKeyStore"
        const val KEY_ALIAS = "nimbo_secrets_v1"
        const val TRANSFORMATION = "AES/GCM/NoPadding"
        const val IV_LENGTH = 12
        const val GCM_TAG_BITS = 128
    }
}

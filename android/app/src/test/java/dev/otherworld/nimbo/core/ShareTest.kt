package dev.otherworld.nimbo.core

import kotlinx.serialization.json.Json
import org.junit.Assert.assertEquals
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The share payload is the server's own OCS shape (snake_case), and its
 * `id` is a string on some Nextcloud versions and a number on others — the
 * Go side normalises that to a string, so this only has to survive the rest.
 */
class ShareTest {

    private val json = NimboJson

    @Test
    fun `decodes the servers snake_case share fields`() {
        val payload = """
            {"own":[{"id":"12","share_type":3,"path":"/Documents/report.pdf",
                     "permissions":17,"share_with":"","url":"https://cloud/s/abc",
                     "token":"abc","expiration":"2026-09-01","uid_owner":"adam",
                     "displayname_owner":"Adam"}],
             "received":[]}
        """.trimIndent()
        val shares = json.decodeFromString<SharesPayload>(payload)
        val s = shares.own.single()
        assertEquals("12", s.id)
        assertEquals(3, s.shareType)
        assertEquals("/Documents/report.pdf", s.path)
        assertEquals("https://cloud/s/abc", s.url)
        assertEquals("Adam", s.ownerDisplay)
    }

    @Test
    fun `a payload missing either half still decodes`() {
        // Never throw on shape drift: an absent key must read as "none", not
        // as a failure to load the screen.
        val shares = json.decodeFromString<SharesPayload>("""{"own":[]}""")
        assertTrue(shares.own.isEmpty())
        assertTrue(shares.received.isEmpty())
    }

    @Test
    fun `share type is named, and an unknown type is not claimed to be anything`() {
        assertEquals("Link", Share(shareType = 3).kindLabel)
        assertEquals("Person", Share(shareType = 0).kindLabel)
        assertEquals("Group", Share(shareType = 1).kindLabel)
        assertEquals("Email", Share(shareType = 4).kindLabel)
        // A share type this app has never heard of is still a share.
        assertEquals("Shared", Share(shareType = 99).kindLabel)
    }

    @Test
    fun `who it is shared with falls back through the fields the server fills`() {
        assertEquals("bob", Share(shareType = 0, shareWith = "bob").recipientLabel)
        // A public link has no recipient; saying "Anyone with the link" is the
        // honest description of who can reach it.
        assertEquals("Anyone with the link", Share(shareType = 3, url = "https://x/s/a").recipientLabel)
        assertEquals("Someone", Share(shareType = 0).recipientLabel)
    }

    @Test
    fun `the name shown is the file, not the whole path`() {
        assertEquals("report.pdf", Share(path = "/Documents/report.pdf").name)
        assertEquals("Documents", Share(path = "/Documents/").name)
        // A share of the account root has no leaf to show.
        assertEquals("Files", Share(path = "/").name)
    }

    @Test
    fun `sharedBy names a person, never an empty string`() {
        assertEquals("Adam", Share(owner = "adam", ownerDisplay = "Adam").sharedBy)
        assertEquals("adam", Share(owner = "adam").sharedBy)
        assertEquals("Someone", Share().sharedBy)
    }
}

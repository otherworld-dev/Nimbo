package dev.otherworld.nimbo.core

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

/**
 * The notification payload, including the actions a notification arrives with.
 *
 * Actions are the part no live server has produced for us yet — every
 * notification on the test account has an empty actions array — so the decode
 * is pinned here rather than assumed from a screen that has never shown one.
 */
class NcNotificationTest {

    @Test
    fun `decodes the servers snake_case notification fields`() {
        val json = """
            [{"notification_id":42,"app":"files_sharing","subject":"Bob shared a file",
              "message":"report.pdf","link":"/apps/files/?dir=/","object_type":"remote_share",
              "datetime":"2026-08-20T14:05:00+00:00","actions":[]}]
        """.trimIndent()
        val n = NimboJson.decodeFromString<List<NcNotification>>(json).single()
        assertEquals(42, n.id)
        assertEquals("files_sharing", n.app)
        assertEquals("Bob shared a file", n.subject)
        assertEquals("report.pdf", n.message)
        assertEquals("remote_share", n.objectType)
        assertTrue(n.actions.isEmpty())
    }

    @Test
    fun `decodes actions with their label, link, method and primary flag`() {
        val json = """
            [{"notification_id":7,"app":"files_sharing","subject":"Pending share",
              "datetime":"2026-08-20T14:05:00Z",
              "actions":[
                {"label":"Accept","link":"/ocs/v2.php/apps/files_sharing/api/v1/shares/9","type":"POST","primary":true},
                {"label":"Decline","link":"/ocs/v2.php/apps/files_sharing/api/v1/shares/9","type":"DELETE","primary":false}
              ]}]
        """.trimIndent()
        val n = NimboJson.decodeFromString<List<NcNotification>>(json).single()
        assertEquals(2, n.actions.size)

        val accept = n.actions[0]
        assertEquals("Accept", accept.label)
        assertEquals("POST", accept.type)
        assertTrue(accept.primary)
        // The link is passed back verbatim; nothing may reconstruct it.
        assertEquals("/ocs/v2.php/apps/files_sharing/api/v1/shares/9", accept.link)

        val decline = n.actions[1]
        assertEquals("DELETE", decline.type)
        assertFalse(decline.primary)
    }

    @Test
    fun `an action with no method decodes to empty, which the core reads as GET`() {
        val json = """[{"notification_id":1,"actions":[{"label":"Open","link":"/x"}]}]"""
        val n = NimboJson.decodeFromString<List<NcNotification>>(json).single()
        assertEquals("", n.actions.single().type)
    }

    @Test
    fun `a notification missing every optional field still decodes`() {
        // Shape drift must never throw: a blank row beats a screen that won't load.
        val n = NimboJson.decodeFromString<List<NcNotification>>("""[{"notification_id":3}]""").single()
        assertEquals(3, n.id)
        assertEquals("", n.subject)
        assertTrue(n.actions.isEmpty())
        assertEquals("", n.whenLabel)
    }

    @Test
    fun `an unparseable date yields no date rather than the epoch`() {
        assertEquals("", NcNotification(time = "").whenLabel)
        assertEquals("", NcNotification(time = "whenever").whenLabel)
        assertTrue(NcNotification(time = "2026-08-20T14:05:00Z").whenLabel.contains("Aug"))
    }
}

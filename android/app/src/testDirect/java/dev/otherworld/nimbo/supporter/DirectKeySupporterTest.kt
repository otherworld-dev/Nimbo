package dev.otherworld.nimbo.supporter

import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Job
import kotlinx.coroutines.job
import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Test

class DirectKeySupporterTest {

    private val day = 24L * 60 * 60 * 1000
    private val now = 100 * day
    private val keyA = "owr_live_" + "a".repeat(20)
    private val keyB = "owr_live_" + "b".repeat(20)

    private class FakeStore(var keys: List<StoredKey> = emptyList()) : KeyListStore {
        override fun load() = keys
        override fun save(keys: List<StoredKey>) { this.keys = keys }
    }

    private class FakeChecker(val answers: Map<String, CheckOutcome> = emptyMap()) : KeyChecker {
        val asked = mutableListOf<String>()
        override suspend fun check(key: String): CheckOutcome {
            asked += key
            return answers[key] ?: CheckOutcome.Unreachable
        }
        override suspend fun portalUrl(key: String): String? = "https://billing.example/portal"
    }

    private class FakeCache(var status: SupporterStatus = SupporterStatus()) : StatusCache {
        override fun read() = status
        override fun write(status: SupporterStatus) { this.status = status }
    }

    private fun supporter(
        store: FakeStore,
        checker: FakeChecker,
        cache: FakeCache = FakeCache(),
        scope: CoroutineScope = CoroutineScope(Job()),
    ) = DirectKeySupporter(store, checker, cache, scope, clock = { now })

    private val patronCache get() = FakeCache(SupporterStatus(SupporterTier.PATRON, SupporterSource.KEY, now - 5 * day))

    @Test
    fun `a network failure never takes a badge away`() = runBlocking {
        val repo = supporter(FakeStore(listOf(StoredKey(keyA, SupporterTier.PATRON, KeyState.ACTIVE))), FakeChecker(), patronCache)
        repo.refresh(force = true)
        assertEquals(SupporterTier.PATRON, repo.status.value.tier)
        assertTrue(repo.notice.value!!.text.contains("Couldn't check"))
    }

    @Test
    fun `an ended key loses its perks`() = runBlocking {
        val repo = supporter(
            FakeStore(listOf(StoredKey(keyA, SupporterTier.PATRON, KeyState.ACTIVE))),
            FakeChecker(mapOf(keyA to CheckOutcome.Ended)),
            patronCache,
        )
        repo.refresh(force = true)
        assertEquals(SupporterTier.NONE, repo.status.value.tier)
    }

    @Test
    fun `a paused key keeps its perks and says why`() = runBlocking {
        val repo = supporter(
            FakeStore(listOf(StoredKey(keyA, SupporterTier.PATRON, KeyState.ACTIVE))),
            FakeChecker(mapOf(keyA to CheckOutcome.Paused)),
            patronCache,
        )
        repo.refresh(force = true)
        assertEquals(SupporterTier.PATRON, repo.status.value.tier)
        assertTrue(repo.notice.value!!.isError)
        assertTrue(repo.notice.value!!.text.contains("Payment problem"))
    }

    @Test
    fun `the highest active key wins`() = runBlocking {
        val repo = supporter(
            FakeStore(listOf(StoredKey(keyA), StoredKey(keyB))),
            FakeChecker(mapOf(keyA to CheckOutcome.Active(SupporterTier.ONE_OFF), keyB to CheckOutcome.Active(SupporterTier.BACKER))),
        )
        repo.refresh(force = true)
        assertEquals(SupporterTier.BACKER, repo.status.value.tier)
        assertEquals(now, repo.status.value.checkedAt)
    }

    @Test
    fun `a recent check is not repeated at start-up`() = runBlocking {
        val checker = FakeChecker(mapOf(keyA to CheckOutcome.Active(SupporterTier.PATRON)))
        val cache = FakeCache(SupporterStatus(SupporterTier.PATRON, SupporterSource.KEY, now - day))
        supporter(FakeStore(listOf(StoredKey(keyA, SupporterTier.PATRON, KeyState.ACTIVE))), checker, cache).refresh(force = false)
        assertTrue(checker.asked.isEmpty())
    }

    @Test
    fun `an unchecked key is checked at start-up even after a recent check`() = runBlocking {
        val checker = FakeChecker(mapOf(keyA to CheckOutcome.Active(SupporterTier.PATRON)))
        val cache = FakeCache(SupporterStatus(SupporterTier.NONE, SupporterSource.KEY, now - day))
        val repo = supporter(FakeStore(listOf(StoredKey(keyA))), checker, cache)
        repo.refresh(force = false)
        assertEquals(listOf(keyA), checker.asked)
        assertEquals(SupporterTier.PATRON, repo.status.value.tier)
    }

    @Test
    fun `an old check is repeated at start-up`() = runBlocking {
        val checker = FakeChecker(mapOf(keyA to CheckOutcome.Active(SupporterTier.PATRON)))
        supporter(FakeStore(listOf(StoredKey(keyA, SupporterTier.PATRON, KeyState.ACTIVE))), checker, patronCache).refresh(force = false)
        assertEquals(listOf(keyA), checker.asked)
    }

    @Test
    fun `no keys means no supporter, and no request to anyone`() = runBlocking {
        // Also what a restored backup looks like: the cached tier came back,
        // the Keystore-encrypted keys did not.
        val checker = FakeChecker()
        val repo = supporter(FakeStore(), checker, patronCache)
        repo.refresh(force = false)
        assertEquals(SupporterTier.NONE, repo.status.value.tier)
        assertTrue(checker.asked.isEmpty())
    }

    @Test
    fun `a malformed key never reaches the network`() = runBlocking {
        val checker = FakeChecker()
        val repo = supporter(FakeStore(), checker)
        assertFalse(repo.addKey("owr_live_nope"))
        assertTrue(checker.asked.isEmpty())
        assertTrue(repo.notice.value!!.isError)
    }

    @Test
    fun `an ended key is not kept`() = runBlocking {
        val store = FakeStore()
        val repo = supporter(store, FakeChecker(mapOf(keyA to CheckOutcome.Ended)))
        assertFalse(repo.addKey(keyA))
        assertTrue(store.keys.isEmpty())
    }

    @Test
    fun `a key that can't be checked yet is saved for later`() = runBlocking {
        val store = FakeStore()
        val repo = supporter(store, FakeChecker())
        assertTrue(repo.addKey(keyA))
        assertEquals(listOf(StoredKey(keyA)), store.keys)
        assertEquals(SupporterTier.NONE, repo.status.value.tier)
    }

    @Test
    fun `the add link adds the key`() = runBlocking {
        val store = FakeStore()
        val repo = supporter(store, FakeChecker(mapOf(keyA to CheckOutcome.Active(SupporterTier.ONE_OFF))), scope = this)
        assertTrue(repo.handleLink("nimbo-supporter://add?key=$keyA"))
        assertFalse(repo.handleLink("https://example.com"))
        coroutineContext.job.children.forEach { it.join() }
        assertEquals(SupporterTier.ONE_OFF, repo.status.value.tier)
        assertEquals(1, store.keys.size)
    }

    @Test
    fun `removing the only key ends supporter status`() = runBlocking {
        val repo = supporter(FakeStore(listOf(StoredKey(keyA, SupporterTier.PATRON, KeyState.ACTIVE))), FakeChecker(), patronCache)
        repo.removeKey(keyA)
        assertEquals(SupporterTier.NONE, repo.status.value.tier)
    }

    @Test
    fun `manage opens the portal for the subscription key`() = runBlocking {
        val repo = supporter(FakeStore(listOf(StoredKey(keyA, SupporterTier.BACKER, KeyState.ACTIVE))), FakeChecker())
        assertNotNull(repo.manageUrl())
    }
}

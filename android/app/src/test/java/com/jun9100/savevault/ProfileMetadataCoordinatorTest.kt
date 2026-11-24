package com.jun9100.savevault

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class ProfileMetadataCoordinatorTest {
    @Test
    fun mergesRemoteTitlesAndExclusionsAndFormatsSummary() {
        var appliedTitles: Map<String, String> = emptyMap()
        val result = ProfileMetadataCoordinator.sync(
            localTitles = mapOf("switch:01007EF00011E000" to "旷野之息"),
            localExcluded = setOf("switch:0100000000010000"),
            remoteMerge = { titles, excluded ->
                Result.success(
                    GameSaveProfileMetadata(
                        revision = 4,
                        customTitles = titles + ("3ds:00040000001B5000" to "精灵宝可梦 究极之日"),
                        excludedScopes = excluded + "psp:ULUS10566",
                    ),
                )
            },
            applyRemoteTitles = { remote ->
                appliedTitles = remote
                1
            },
            applyRemoteExcluded = { it },
        ).getOrThrow()

        assertNotNull(result)
        assertEquals(2, appliedTitles.size)
        assertEquals(1, result!!.addedLocalTitles)
        assertEquals(setOf("switch:0100000000010000", "psp:ULUS10566"), result.mergedExcludedScopes)
        assertTrue(result.summaryText().contains("2 条译名"))
        assertTrue(result.summaryText().contains("2 条排除"))
        assertTrue(result.summaryText().contains("本机新增 1 条"))
    }

    @Test
    fun returnsNullWhenServerDoesNotSupportProfileMetadata() {
        val result = ProfileMetadataCoordinator.sync(
            localTitles = emptyMap(),
            localExcluded = emptySet(),
            remoteMerge = { _, _ -> Result.success(null) },
            applyRemoteTitles = { 0 },
            applyRemoteExcluded = { it },
        ).getOrThrow()
        assertNull(result)
    }
}

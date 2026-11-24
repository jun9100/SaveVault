package com.jun9100.savevault

import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertTrue
import org.junit.Test

class StoragePermissionPolicyTest {
    @Test
    fun android8To10UsesLegacyRuntimePermissions() {
        for (api in listOf(26, 27, 28, 29)) {
            assertEquals(StoragePermissionFlow.LEGACY_RUNTIME_PERMISSIONS, StoragePermissionPolicy.flowForSdk(api))
            assertTrue(StoragePermissionPolicy.canRequestStorageAccess(api))
            val perms = StoragePermissionPolicy.legacyPermissionsForSdk(api)
            assertEquals(2, perms.size)
            assertTrue(perms.any { it.endsWith("READ_EXTERNAL_STORAGE") })
            assertTrue(perms.any { it.endsWith("WRITE_EXTERNAL_STORAGE") })
        }
    }

    @Test
    fun android11AndNewerUsesManageAllFilesSettings() {
        for (api in listOf(30, 31, 33, 34, 35)) {
            assertEquals(StoragePermissionFlow.MANAGE_ALL_FILES_SETTINGS, StoragePermissionPolicy.flowForSdk(api))
            assertTrue(StoragePermissionPolicy.canRequestStorageAccess(api))
            assertEquals(0, StoragePermissionPolicy.legacyPermissionsForSdk(api).size)
        }
        assertFalse(StoragePermissionPolicy.canRequestStorageAccess(22))
    }
}

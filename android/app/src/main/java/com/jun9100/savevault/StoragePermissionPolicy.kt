package com.jun9100.savevault

import android.Manifest

/**
 * 跨 Android 版本（API 26–35+）的共享存储权限策略，纯 JVM 可测。
 *
 * - Android 11+（API 30+）：通过系统设置页授予 `MANAGE_EXTERNAL_STORAGE`（「所有文件访问」）。
 * - Android 8–10（API 26–29）：通过运行时弹窗动态请求 `READ_EXTERNAL_STORAGE` / `WRITE_EXTERNAL_STORAGE`。
 */
enum class StoragePermissionFlow {
    MANAGE_ALL_FILES_SETTINGS,
    LEGACY_RUNTIME_PERMISSIONS,
}

object StoragePermissionPolicy {
    const val ANDROID_R_API = 30
    const val ANDROID_M_API = 23

    fun flowForSdk(sdkInt: Int): StoragePermissionFlow =
        if (sdkInt >= ANDROID_R_API) {
            StoragePermissionFlow.MANAGE_ALL_FILES_SETTINGS
        } else {
            StoragePermissionFlow.LEGACY_RUNTIME_PERMISSIONS
        }

    fun canRequestStorageAccess(sdkInt: Int): Boolean = sdkInt >= ANDROID_M_API

    fun legacyPermissionsForSdk(sdkInt: Int): Array<String> =
        if (sdkInt in ANDROID_M_API until ANDROID_R_API) {
            arrayOf(
                Manifest.permission.READ_EXTERNAL_STORAGE,
                Manifest.permission.WRITE_EXTERNAL_STORAGE,
            )
        } else {
            emptyArray()
        }
}

package com.jun9100.savevault.ui.settings

import com.jun9100.savevault.AUTH_BLOCKED_SHARED_TOKEN_DISABLED
import com.jun9100.savevault.AUTH_BLOCKED_TOKEN_REVOKED
import com.jun9100.savevault.GameSaveAuthUi
import com.jun9100.savevault.GameSaveWhoami
import org.junit.Assert.assertEquals
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test

class GameSaveAuthTextTest {
    @Test
    fun anUnconfiguredOrUnknownCredentialHasNoStatusLine() {
        assertNull(GameSaveAuthText.status(GameSaveAuthUi()))
        assertNull(GameSaveAuthText.status(GameSaveAuthUi(whoamiLoaded = true)))
        assertNull(GameSaveAuthText.warning(GameSaveAuthUi()))
    }

    @Test
    fun aRevokedDeviceSaysSyncStoppedAndWinsOverAStaleWhoami() {
        val status = GameSaveAuthText.status(
            GameSaveAuthUi(
                blocked = AUTH_BLOCKED_TOKEN_REVOKED,
                whoami = whoami("device", "客厅的 Android 平板"),
                whoamiLoaded = true,
            ),
        )

        assertEquals(
            "此设备已被 SaveVault 云端移除，自动同步已停止。请用配对码重新连接。" to GameSaveAuthText.Level.ERROR,
            status,
        )
        assertNull(GameSaveAuthText.warning(GameSaveAuthUi(blocked = AUTH_BLOCKED_TOKEN_REVOKED)))
    }

    @Test
    fun aDisabledSharedKeyAsksForPairing() {
        assertEquals(
            "SaveVault 云端已停用共享同步密钥，请用配对码连接本设备。" to GameSaveAuthText.Level.ERROR,
            GameSaveAuthText.status(GameSaveAuthUi(blocked = AUTH_BLOCKED_SHARED_TOKEN_DISABLED)),
        )
    }

    @Test
    fun aDeviceTokenShowsItsNameAndFallsBackToThisDevice() {
        assertEquals(
            "已用设备令牌连接：客厅的 Android 平板" to GameSaveAuthText.Level.NORMAL,
            GameSaveAuthText.status(GameSaveAuthUi(whoami = whoami("device", "客厅的 Android 平板"))),
        )
        assertEquals(
            "已用设备令牌连接：本设备" to GameSaveAuthText.Level.NORMAL,
            GameSaveAuthText.status(GameSaveAuthUi(whoami = whoami("device", " "))),
        )
    }

    @Test
    fun aSharedKeySaysSoWithoutAWarningUntilTheServerIsInWarnMode() {
        val shared = GameSaveAuthUi(whoami = whoami("shared"))
        assertEquals(
            "正在使用共享同步密钥" to GameSaveAuthText.Level.NORMAL,
            GameSaveAuthText.status(shared),
        )
        assertNull(GameSaveAuthText.warning(shared))

        assertEquals(
            "共享同步密钥即将停用，请配对本设备。",
            GameSaveAuthText.warning(shared.copy(sharedWarning = true)),
        )
        assertEquals(
            "共享同步密钥即将停用，请配对本设备。",
            GameSaveAuthText.warning(GameSaveAuthUi(whoami = whoami("shared", sharedTokenMode = "warn"))),
        )
        assertNull(GameSaveAuthText.warning(GameSaveAuthUi(whoami = whoami("device"))))
    }

    @Test
    fun quotaFormatsUsageAndRatioAndHidesWhenBlocked() {
        assertNull(GameSaveAuthText.quota(GameSaveAuthUi()))
        assertNull(GameSaveAuthText.quota(GameSaveAuthUi(whoami = whoami("device", usedBytes = 0L, quotaBytes = 0L))))
        assertNull(
            GameSaveAuthText.quota(
                GameSaveAuthUi(
                    blocked = AUTH_BLOCKED_TOKEN_REVOKED,
                    whoami = whoami("device", usedBytes = 1024L, quotaBytes = 2048L),
                ),
            ),
        )

        val bounded = GameSaveAuthText.quota(
            GameSaveAuthUi(whoami = whoami("device", usedBytes = 512L * 1024L * 1024L, quotaBytes = 1024L * 1024L * 1024L)),
        )
        assertNotNull(bounded)
        assertTrue(bounded!!.first.contains("50%"))
        assertEquals(0.5f, bounded.second!!, 0.01f)

        val unlimited = GameSaveAuthText.quota(
            GameSaveAuthUi(whoami = whoami("device", usedBytes = 2L * 1024L * 1024L, quotaBytes = 0L)),
        )
        assertNotNull(unlimited)
        assertTrue(unlimited!!.first.contains("无配额上限"))
        assertNull(unlimited.second)
    }

    private fun whoami(
        auth: String,
        name: String = "",
        sharedTokenMode: String = "allow",
        usedBytes: Long = 0L,
        quotaBytes: Long = 0L,
    ) = GameSaveWhoami(
        auth = auth,
        tokenId = "",
        deviceId = "",
        name = name,
        sharedTokenMode = sharedTokenMode,
        usedBytes = usedBytes,
        quotaBytes = quotaBytes,
    )
}

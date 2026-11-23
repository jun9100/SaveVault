package com.jun9100.savevault

import org.junit.Assert.assertEquals
import org.junit.Assert.assertNull
import org.junit.Test

class PageMessagesTest {
    @Test
    fun aSettingsResultNeverAppearsInTheLibrary() {
        val messages = PageMessages()
            .with(SaveVaultPage.LIBRARY, action = "批量备份完成", error = "游戏 A：失败")
            .with(SaveVaultPage.SETTINGS, action = "game-save 连接成功")

        assertEquals("game-save 连接成功", messages[SaveVaultPage.SETTINGS].action)
        assertEquals("批量备份完成", messages[SaveVaultPage.LIBRARY].action)
        assertEquals("游戏 A：失败", messages[SaveVaultPage.LIBRARY].error)
        assertNull(messages[SaveVaultPage.SETTINGS].error)
    }

    @Test
    fun startingAnOperationClearsOnlyItsOwnPage() {
        val messages = PageMessages()
            .with(SaveVaultPage.LIBRARY, action = "已备份", error = "上传失败")
            .with(SaveVaultPage.SETTINGS, action = "已保存")
            .with(SaveVaultPage.SETTINGS)

        assertEquals(PageMessage(), messages[SaveVaultPage.SETTINGS])
        assertEquals(PageMessage("已备份", "上传失败"), messages[SaveVaultPage.LIBRARY])
    }

    @Test
    fun anErrorKeepsThePageLastResult() {
        val messages = PageMessages()
            .with(SaveVaultPage.SETTINGS, action = "已启用存档来源")
            .withError(SaveVaultPage.SETTINGS, "该来源目录不存在或不可访问，不能勾选")

        assertEquals("已启用存档来源", messages[SaveVaultPage.SETTINGS].action)
        assertEquals("该来源目录不存在或不可访问，不能勾选", messages[SaveVaultPage.SETTINGS].error)
        assertEquals(PageMessage(), messages[SaveVaultPage.LIBRARY])
    }
}

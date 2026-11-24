package com.jun9100.savevault.ui

import com.jun9100.savevault.SourceAccessCapability
import com.jun9100.savevault.SourceAccessStatus
import java.text.DateFormat
import java.text.SimpleDateFormat
import java.time.Instant
import java.util.Date
import java.util.Locale

internal fun sourceAccessLabel(access: SourceAccessStatus): String = when (access.capability) {
    SourceAccessCapability.APP -> if (access.writable) "应用可访问 · 可读写" else "应用可访问 · 只读"
    SourceAccessCapability.SAF -> if (access.writable) "SAF 已授权 · 可读写" else "SAF 已授权 · 只读，可备份"
    SourceAccessCapability.ROOT -> "仅 Root 可访问 · 可读写"
    SourceAccessCapability.ALL_FILES -> "「所有文件访问」已开启 · 可读写"
    SourceAccessCapability.UNAVAILABLE -> buildString {
        append("不可访问 · ")
        append(
            when (access.requiredCapability) {
                SourceAccessCapability.APP -> "应用访问"
                SourceAccessCapability.SAF -> "需要 SAF 授权"
                SourceAccessCapability.ROOT -> "需要 Root"
                SourceAccessCapability.ALL_FILES -> "需要「所有文件访问」"
                SourceAccessCapability.UNAVAILABLE -> "来源检查"
            },
        )
        access.reason.takeIf(String::isNotBlank)?.let { append(" · $it") }
    }
}

internal fun kindLabel(kind: String): String = when (kind) {
    "yuzu-switch" -> "Yuzu/Eden Switch"
    "ryujinx-switch" -> "Ryujinx/Ryubing Switch"
    "switch-title" -> "Switch TitleID"
    "ppsspp" -> "PPSSPP"
    "retroarch" -> "RetroArch"
    "dolphin" -> "Dolphin"
    "duckstation" -> "DuckStation"
    "aethersx2" -> "AetherSX2/NetherSX2"
    "emulator" -> "模拟器"
    "android-app" -> "Android 应用数据"
    "android-game" -> "Android 游戏"
    "gamehub-pc" -> "GameHub 本地 PC"
    else -> kind
}

internal fun formatTime(millis: Long): String = DateFormat.getDateTimeInstance().format(Date(millis))

/** Month, day and time for one-line rows, e.g. "10-30 21:49". */
internal fun formatShortTime(millis: Long): String = SimpleDateFormat("MM-dd HH:mm", Locale.ROOT).format(Date(millis))

/** Formats an ISO-8601 instant into a compact local timestamp, falling back to the trimmed raw string. */
internal fun formatIsoShortTime(iso: String): String {
    val trimmed = iso.trim()
    if (trimmed.isEmpty()) return ""
    return runCatching { formatShortTime(Instant.parse(trimmed).toEpochMilli()) }.getOrDefault(trimmed)
}

internal fun formatBytes(bytes: Long): String = when {
    bytes < 1024 -> "$bytes B"
    bytes < 1024 * 1024 -> "%.1f KiB".format(bytes / 1024.0)
    bytes < 1024L * 1024 * 1024 -> "%.1f MiB".format(bytes / (1024.0 * 1024.0))
    else -> "%.2f GiB".format(bytes / (1024.0 * 1024.0 * 1024.0))
}

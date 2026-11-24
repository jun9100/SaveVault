package com.jun9100.savevault.ui

import com.jun9100.savevault.SourceAccessCapability
import com.jun9100.savevault.SourceAccessStatus
import java.text.DateFormat
import java.time.Instant
import java.time.OffsetDateTime
import java.time.ZoneId
import java.time.format.DateTimeFormatter
import java.time.format.DateTimeParseException
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
    "citra" -> "3DS (Citra / Azahar)"
    "vita3k" -> "PS Vita (Vita3K)"
    "drastic" -> "NDS (DraStic)"
    "melonds" -> "NDS (melonDS)"
    "epsxe" -> "PS1 (ePSXe)"
    "mame" -> "MAME 街机"
    "mupen64plus" -> "N64 (Mupen64Plus)"
    "lemuroid" -> "Lemuroid"
    "emulator" -> "模拟器"
    "android-app" -> "Android 应用数据"
    "android-game" -> "Android 游戏"
    "gamehub-pc" -> "GameHub 本地 PC"
    else -> kind
}

private val SHORT_TIME_FORMATTER: DateTimeFormatter = DateTimeFormatter.ofPattern("MM-dd HH:mm", Locale.ROOT)

internal fun formatTime(millis: Long): String = DateFormat.getDateTimeInstance().format(Date(millis))

/** Month, day and time for one-line rows, e.g. "10-30 21:49". */
internal fun formatShortTime(millis: Long): String =
    SHORT_TIME_FORMATTER.format(Instant.ofEpochMilli(millis).atZone(ZoneId.systemDefault()))

/** Formats an ISO-8601 instant into a compact local timestamp, falling back to the trimmed raw string. */
internal fun formatIsoShortTime(iso: String): String {
    val trimmed = iso.trim()
    if (trimmed.isEmpty()) return ""
    return try {
        formatShortTime(OffsetDateTime.parse(trimmed).toInstant().toEpochMilli())
    } catch (_: DateTimeParseException) {
        trimmed
    }
}

internal fun formatBytes(bytes: Long): String {
    val safe = bytes.coerceAtLeast(0L)
    return when {
        safe < 1024 -> "$safe B"
        safe < 1024 * 1024 -> String.format(Locale.ROOT, "%.1f KiB", safe / 1024.0)
        safe < 1024L * 1024 * 1024 -> String.format(Locale.ROOT, "%.1f MiB", safe / (1024.0 * 1024.0))
        else -> String.format(Locale.ROOT, "%.2f GiB", safe / (1024.0 * 1024.0 * 1024.0))
    }
}

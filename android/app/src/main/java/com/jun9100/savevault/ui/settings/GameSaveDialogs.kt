package com.jun9100.savevault.ui.settings

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.heightIn
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.Visibility
import androidx.compose.material.icons.outlined.VisibilityOff
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.text.input.VisualTransformation
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import com.jun9100.savevault.CloudDevicesDialogState
import com.jun9100.savevault.GameSaveConfig
import com.jun9100.savevault.GameSaveDeviceTokenInfo
import com.jun9100.savevault.GameSaveTestFailure
import com.jun9100.savevault.ImportedGameSaveConfig
import com.jun9100.savevault.SaveVaultTheme
import com.jun9100.savevault.ui.dialogs.dialogButtonFocus
import com.jun9100.savevault.ui.formatBytes
import com.jun9100.savevault.ui.formatIsoShortTime
import com.jun9100.savevault.ui.input.busyDimmed
import com.jun9100.savevault.ui.input.consumeHandheldShortcuts
import com.jun9100.savevault.ui.input.dpadLeavesTextField
import com.jun9100.savevault.ui.input.dpadScrollable
import com.jun9100.savevault.ui.input.handheldFocusRing
import com.jun9100.savevault.ui.input.rememberInitialFocus
import com.jun9100.savevault.ui.input.whenIdle
import com.jun9100.savevault.ui.preview.OdinScreenPreviews

/**
 * The game-save address and sync key. The draft lives here, so a refresh of
 * the saved config never overwrites it; "测试并保存" stores it only when the
 * connection test passes.
 */
@Composable
internal fun GameSaveEditorDialog(
    initial: GameSaveConfig,
    busy: Boolean,
    onDismiss: () -> Unit,
    onLogin: (url: String, username: String, password: String) -> Unit,
    initialUsername: String = "",
) {
    var url by rememberSaveable { mutableStateOf(initial.serviceUrl) }
    var username by rememberSaveable { mutableStateOf(initialUsername) }
    var password by remember { mutableStateOf("") }
    var visible by remember { mutableStateOf(false) }
    val submit = whenIdle(busy) {
        val secret = password
        password = ""
        onLogin(url.trim(), username.trim(), secret)
    }
    AlertDialog(
        onDismissRequest = onDismiss,
        modifier = Modifier.consumeHandheldShortcuts(),
        title = { Text("SaveVault 云端") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                // Opened on purpose, so the address takes focus and the keyboard right away.
                OutlinedTextField(
                    value = url,
                    onValueChange = { url = it },
                    label = { Text("服务地址") },
                    placeholder = { Text("http://nas.example:8788") },
                    singleLine = true,
                    keyboardOptions = KeyboardOptions(keyboardType = KeyboardType.Uri, imeAction = ImeAction.Next),
                    modifier = Modifier.fillMaxWidth().dpadLeavesTextField().focusRequester(rememberInitialFocus()),
                )
                OutlinedTextField(
                    value = username,
                    onValueChange = { username = it },
                    label = { Text("账号") },
                    singleLine = true,
                    keyboardOptions = KeyboardOptions(
                        keyboardType = KeyboardType.Text,
                        autoCorrectEnabled = false,
                        imeAction = ImeAction.Next,
                    ),
                    modifier = Modifier.fillMaxWidth().dpadLeavesTextField(),
                )
                OutlinedTextField(
                    value = password,
                    onValueChange = { password = it },
                    label = { Text("密码") },
                    singleLine = true,
                    visualTransformation = if (visible) VisualTransformation.None else PasswordVisualTransformation(),
                    keyboardOptions = KeyboardOptions(
                        keyboardType = KeyboardType.Password,
                        autoCorrectEnabled = false,
                        imeAction = ImeAction.Done,
                    ),
                    trailingIcon = {
                        IconButton(onClick = { visible = !visible }, modifier = Modifier.handheldFocusRing(CircleShape)) {
                            Icon(
                                if (visible) Icons.Outlined.VisibilityOff else Icons.Outlined.Visibility,
                                contentDescription = if (visible) "隐藏密码" else "显示密码",
                            )
                        }
                    },
                    modifier = Modifier.fillMaxWidth().dpadLeavesTextField(),
                )
                Text(
                    if (busy) {
                        "正在登录…"
                    } else {
                        "用账号密码登录，成功后把令牌用 Android Keystore 加密保存；密码本身不会存到本机。"
                    },
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        },
        confirmButton = {
            Button(onClick = submit, modifier = Modifier.dialogButtonFocus().busyDimmed(busy)) { Text("登录并保存") }
        },
        dismissButton = {
            TextButton(onClick = onDismiss, modifier = Modifier.dialogButtonFocus()) { Text("取消") }
        },
    )
}

/** The connection test failed: save anyway, go back to the draft, or leave the saved config as it is (the default). */
@Composable
internal fun GameSaveTestFailedDialog(
    failure: GameSaveTestFailure,
    busy: Boolean,
    onSaveAnyway: () -> Unit,
    onEdit: () -> Unit,
    onDismiss: () -> Unit,
) {
    AlertDialog(
        onDismissRequest = onDismiss,
        modifier = Modifier.consumeHandheldShortcuts(),
        title = { Text("连接测试未通过") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text(failure.message)
                Text("地址：${failure.config.serviceUrl}", style = MaterialTheme.typography.bodySmall)
                Text(
                    "配置还没有保存。“仍然保存”会改用这个配置，服务可连接之前上传和检查更新都会失败。",
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        },
        confirmButton = {
            Row(horizontalArrangement = Arrangement.spacedBy(4.dp)) {
                TextButton(onClick = onEdit, modifier = Modifier.dialogButtonFocus()) { Text("返回修改") }
                Button(onClick = whenIdle(busy, onSaveAnyway), modifier = Modifier.dialogButtonFocus().busyDimmed(busy)) { Text("仍然保存") }
            }
        },
        dismissButton = {
            // Saving a config that does not connect replaces a working one: focus starts on the safe choice.
            TextButton(
                onClick = onDismiss,
                modifier = Modifier.focusRequester(rememberInitialFocus()).dialogButtonFocus(),
            ) { Text("取消") }
        },
    )
}

/** A config read from the clipboard: what it would set, the key masked, before "测试并保存". */
@Composable
internal fun ConfigImportConfirmDialog(
    config: ImportedGameSaveConfig,
    replacing: Boolean,
    busy: Boolean,
    onDismiss: () -> Unit,
    onConfirm: () -> Unit,
) {
    AlertDialog(
        onDismissRequest = onDismiss,
        modifier = Modifier.consumeHandheldShortcuts(),
        title = { Text("导入 SaveVault 云端配置？") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text("服务地址：${config.serviceUrl}")
                Text("同步密钥：${config.maskedSyncKey}")
                Text(
                    buildString {
                        append("确认后先测试连接，通过后才保存")
                        if (replacing) append("，并替换当前的 SaveVault 云端配置")
                        append("。")
                    },
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        },
        confirmButton = {
            Button(onClick = whenIdle(busy, onConfirm), modifier = Modifier.dialogButtonFocus().busyDimmed(busy)) { Text("测试并保存") }
        },
        dismissButton = {
            // A config from outside the app: nothing changes unless the user moves to confirm.
            TextButton(
                onClick = onDismiss,
                modifier = Modifier.focusRequester(rememberInitialFocus()).dialogButtonFocus(),
            ) { Text("取消") }
        },
    )
}

/** Lists paired devices in the authenticated vault and lets the user revoke lost or unused ones. */
@Composable
internal fun CloudDevicesDialog(
    dialog: CloudDevicesDialogState,
    busy: Boolean,
    onRefresh: () -> Unit,
    onRequestRevoke: (GameSaveDeviceTokenInfo) -> Unit,
    onDismiss: () -> Unit,
) {
    AlertDialog(
        onDismissRequest = onDismiss,
        modifier = Modifier.consumeHandheldShortcuts(),
        title = { Text("已配对设备管理") },
        text = {
            val scroll = rememberScrollState()
            Column(
                modifier = Modifier
                    .fillMaxWidth()
                    .heightIn(max = 360.dp)
                    .dpadScrollable(scroll)
                    .verticalScroll(scroll),
                verticalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                dialog.report?.let { report ->
                    val usageText = if (report.quotaBytes > 0L) {
                        "云端空间：${formatBytes(report.usedBytes)} / ${formatBytes(report.quotaBytes)}"
                    } else {
                        "云端已用：${formatBytes(report.usedBytes)}（无配额上限）"
                    }
                    Text(
                        usageText,
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
                if (dialog.loading && dialog.report == null) {
                    Row(
                        verticalAlignment = Alignment.CenterVertically,
                        horizontalArrangement = Arrangement.spacedBy(10.dp),
                    ) {
                        CircularProgressIndicator(modifier = Modifier.size(18.dp), strokeWidth = 2.dp)
                        Text("正在读取已配对设备…", style = MaterialTheme.typography.bodySmall)
                    }
                }
                dialog.error?.let { err ->
                    Text(err, style = MaterialTheme.typography.bodySmall, color = MaterialTheme.colorScheme.error)
                }
                val report = dialog.report
                if (report != null) {
                    if (report.tokens.isEmpty()) {
                        Text(
                            "当前保险库还没有已配对的设备令牌。",
                            style = MaterialTheme.typography.bodySmall,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    } else {
                        report.tokens.forEachIndexed { index, token ->
                            if (index > 0) HorizontalDivider()
                            val isCurrent = report.currentTokenId.isNotBlank() && token.id == report.currentTokenId
                            val statusText = when {
                                token.revoked -> "已吊销"
                                token.online -> "在线"
                                else -> "已配对"
                            }
                            val subtitle = buildList {
                                add(statusText)
                                if (isCurrent) add("本设备")
                                val lastUsed = formatIsoShortTime(token.lastUsedAt)
                                if (lastUsed.isNotBlank()) add("最近使用 $lastUsed")
                                else {
                                    val created = formatIsoShortTime(token.createdAt)
                                    if (created.isNotBlank()) add("创建于 $created")
                                }
                            }.joinToString(" · ")
                            Row(
                                modifier = Modifier.fillMaxWidth().padding(vertical = 2.dp),
                                verticalAlignment = Alignment.CenterVertically,
                                horizontalArrangement = Arrangement.spacedBy(8.dp),
                            ) {
                                Column(modifier = Modifier.weight(1f)) {
                                    Text(
                                        token.label,
                                        style = MaterialTheme.typography.bodyMedium,
                                        maxLines = 1,
                                        overflow = TextOverflow.Ellipsis,
                                    )
                                    Text(
                                        subtitle,
                                        style = MaterialTheme.typography.labelSmall,
                                        color = when {
                                            token.revoked -> MaterialTheme.colorScheme.onSurfaceVariant
                                            token.online -> MaterialTheme.colorScheme.primary
                                            else -> MaterialTheme.colorScheme.onSurfaceVariant
                                        },
                                        maxLines = 2,
                                        overflow = TextOverflow.Ellipsis,
                                    )
                                }
                                if (!token.revoked) {
                                    TextButton(
                                        onClick = whenIdle(busy || dialog.loading) { onRequestRevoke(token) },
                                        colors = ButtonDefaults.textButtonColors(contentColor = MaterialTheme.colorScheme.error),
                                        modifier = Modifier.dialogButtonFocus().busyDimmed(busy || dialog.loading),
                                    ) { Text("吊销") }
                                }
                            }
                        }
                    }
                }
            }
        },
        confirmButton = {
            Button(
                onClick = whenIdle(busy || dialog.loading, onRefresh),
                modifier = Modifier.dialogButtonFocus().busyDimmed(busy || dialog.loading),
            ) { Text("刷新") }
        },
        dismissButton = {
            TextButton(
                onClick = onDismiss,
                modifier = Modifier.focusRequester(rememberInitialFocus()).dialogButtonFocus(),
            ) { Text("关闭") }
        },
    )
}

/** Confirmation before revoking a device token from the vault. */
@Composable
internal fun RevokeDeviceConfirmDialog(
    token: GameSaveDeviceTokenInfo,
    isCurrentDevice: Boolean,
    busy: Boolean,
    onDismiss: () -> Unit,
    onConfirm: () -> Unit,
) {
    AlertDialog(
        onDismissRequest = onDismiss,
        modifier = Modifier.consumeHandheldShortcuts(),
        title = { Text("吊销设备「${token.label}」？") },
        text = {
            Column(verticalArrangement = Arrangement.spacedBy(8.dp)) {
                Text(
                    if (isCurrentDevice) {
                        "这是当前设备的令牌。吊销后本设备会立即停止同步，需要重新登录账号或用配对码连接。"
                    } else {
                        "吊销后「${token.label}」将无法再上传或下载云端存档，需要重新配对才能恢复同步。"
                    },
                    style = MaterialTheme.typography.bodyMedium,
                )
                if (token.deviceId.isNotBlank()) {
                    Text(
                        "设备标识：${token.deviceId}",
                        style = MaterialTheme.typography.bodySmall,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                    )
                }
            }
        },
        confirmButton = {
            Button(
                onClick = whenIdle(busy, onConfirm),
                colors = ButtonDefaults.buttonColors(
                    containerColor = MaterialTheme.colorScheme.error,
                    contentColor = MaterialTheme.colorScheme.onError,
                ),
                modifier = Modifier.dialogButtonFocus().busyDimmed(busy),
            ) { Text("确认吊销") }
        },
        dismissButton = {
            TextButton(
                onClick = onDismiss,
                modifier = Modifier.focusRequester(rememberInitialFocus()).dialogButtonFocus(),
            ) { Text("取消") }
        },
    )
}

@OdinScreenPreviews
@Composable
private fun ConfigImportConfirmDialogPreview() {
    SaveVaultTheme {
        ConfigImportConfirmDialog(
            config = ImportedGameSaveConfig("http://192.168.1.5:8788", "super-secret-sync-key"),
            replacing = true,
            busy = false,
            onDismiss = {},
            onConfirm = {},
        )
    }
}

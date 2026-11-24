package com.jun9100.savevault.ui.settings

import android.content.Context
import android.net.wifi.WifiManager
import android.os.Build
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.outlined.Devices
import androidx.compose.material.icons.outlined.Refresh
import androidx.compose.material3.Card
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.unit.dp
import com.jun9100.savevault.LanPeer
import com.jun9100.savevault.LanPeerScanner
import com.jun9100.savevault.R
import com.jun9100.savevault.SourceUiState
import com.jun9100.savevault.ui.input.busyDimmed
import java.io.IOException
import kotlinx.coroutines.async
import kotlinx.coroutines.awaitAll
import kotlinx.coroutines.coroutineScope
import kotlinx.coroutines.launch

/**
 * 局域网 P2P 对端发现卡片：通过 UDP 8385 发现同一 Wi-Fi 下的 SaveVault 桌面端
 * （Windows / macOS / Linux / Steam Deck），并支持一键探测对端 `/api/p2p/ping` 状态与延迟。
 */
@Composable
internal fun LanPeerCard(
    state: SourceUiState,
    modifier: Modifier = Modifier,
) {
    val context = LocalContext.current
    val scope = rememberCoroutineScope()
    var scanning by remember { mutableStateOf(false) }
    var probingIds by remember { mutableStateOf(emptySet<String>()) }
    var peers by remember { mutableStateOf<List<LanPeer>>(emptyList()) }
    var statusLine by remember { mutableStateOf<String?>(null) }
    val selfName = state.gameSaveAuth.whoami?.name?.takeIf { it.isNotBlank() } ?: (Build.MODEL ?: "Android")
    val selfId = remember(selfName) {
        "android-${Build.MODEL.orEmpty().lowercase().replace(Regex("[^a-z0-9]"), "").ifBlank { "handheld" }}"
    }

    Card(modifier = modifier.fillMaxWidth()) {
        Column(modifier = Modifier.padding(18.dp)) {
            Row(
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.spacedBy(8.dp),
            ) {
                Icon(Icons.Outlined.Devices, contentDescription = null)
                Text(stringResource(R.string.lan_p2p_title), style = MaterialTheme.typography.titleMedium)
            }
            Text(
                stringResource(R.string.lan_p2p_desc),
                style = MaterialTheme.typography.bodySmall,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.padding(top = 6.dp),
            )
            OutlinedButton(
                onClick = {
                    if (scanning) return@OutlinedButton
                    scanning = true
                    statusLine = "正在扫描局域网 SaveVault 设备（约 3.8 秒）…"
                    scope.launch {
                        val wifi = context.applicationContext.getSystemService(Context.WIFI_SERVICE) as? WifiManager
                        val multicastLock = wifi?.createMulticastLock("savevault-lan-discovery")?.apply {
                            setReferenceCounted(false)
                        }
                        try {
                            multicastLock?.acquire()
                            val discovered = LanPeerScanner.discover(selfNodeId = selfId, selfDeviceName = selfName)
                            val probed = coroutineScope {
                                discovered.map { peer ->
                                    async { LanPeerScanner.probePeer(peer, fromNodeId = selfId) }
                                }.awaitAll()
                            }
                            peers = probed
                            statusLine = if (probed.isEmpty()) {
                                "未发现局域网设备；请确认 PC 或 Steam Deck 已启动 SaveVault 且处于同一 Wi-Fi。"
                            } else {
                                "发现 ${probed.size} 台局域网设备。"
                            }
                        } catch (error: IOException) {
                            statusLine = "扫描失败：${error.message ?: "无法监听局域网广播"}"
                        } finally {
                            if (multicastLock?.isHeld == true) multicastLock.release()
                            scanning = false
                        }
                    }
                },
                enabled = !scanning && !state.busy,
                modifier = Modifier
                    .fillMaxWidth()
                    .padding(top = 12.dp)
                    .busyDimmed(scanning || state.busy)
                    .settingButtonFocus(),
            ) {
                Icon(Icons.Outlined.Refresh, contentDescription = null)
                Text(
                    if (scanning) stringResource(R.string.lan_p2p_scanning) else stringResource(R.string.lan_p2p_scan),
                    modifier = Modifier.padding(start = 8.dp),
                )
            }

            statusLine?.let { msg ->
                Text(
                    msg,
                    style = MaterialTheme.typography.bodySmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.padding(top = 8.dp),
                )
            }

            peers.forEach { peer ->
                val isProbing = peer.nodeId in probingIds
                Row(
                    verticalAlignment = Alignment.CenterVertically,
                    horizontalArrangement = Arrangement.SpaceBetween,
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(top = 10.dp),
                ) {
                    Column(modifier = Modifier.weight(1f).padding(end = 8.dp)) {
                        Text(
                            "${peer.deviceName} (${peer.deviceType})",
                            style = MaterialTheme.typography.bodyMedium,
                        )
                        val detail = buildString {
                            append("${peer.address}:${peer.port}")
                            val version = peer.appVersion.trim().removePrefix("v")
                            if (version.isNotEmpty()) append(" · v$version")
                            if (peer.paired == true) append(" · 已配对")
                            when (peer.reachable) {
                                true -> {
                                    append(" · 在线")
                                    peer.latencyMs?.let { append(" ${it}ms") }
                                }
                                false -> append(" · ${peer.probeError ?: "不可达"}")
                                null -> Unit
                            }
                        }
                        Text(
                            detail,
                            style = MaterialTheme.typography.labelSmall,
                            color = if (peer.reachable == false) {
                                MaterialTheme.colorScheme.error
                            } else {
                                MaterialTheme.colorScheme.onSurfaceVariant
                            },
                        )
                    }
                    OutlinedButton(
                        onClick = {
                            if (isProbing) return@OutlinedButton
                            probingIds = probingIds + peer.nodeId
                            scope.launch {
                                try {
                                    val updated = LanPeerScanner.probePeer(peer, fromNodeId = selfId)
                                    peers = peers.map { if (it.nodeId == updated.nodeId) updated else it }
                                } finally {
                                    probingIds = probingIds - peer.nodeId
                                }
                            }
                        },
                        enabled = !scanning && !state.busy && !isProbing,
                        modifier = Modifier.settingButtonFocus(),
                    ) {
                        Text(stringResource(R.string.lan_p2p_reprobe))
                    }
                }
            }
        }
    }
}

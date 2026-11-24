package com.jun9100.savevault

import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import org.json.JSONObject
import java.io.ByteArrayOutputStream
import java.net.DatagramPacket
import java.net.DatagramSocket
import java.net.Inet4Address
import java.net.InetAddress
import java.net.InetSocketAddress
import java.net.MulticastSocket
import java.net.Socket
import java.net.SocketTimeoutException
import java.nio.charset.StandardCharsets

/**
 * 局域网 P2P 对端设备信息，与桌面端 `internal/p2p/discovery`（UDP 8385）和
 * `GET /api/p2p/ping` 协议完全互通。
 */
data class LanPeer(
    val nodeId: String,
    val deviceName: String,
    val deviceType: String,
    val address: String,
    val port: Int,
    val lastSeenMs: Long,
    val appVersion: String = "",
    val paired: Boolean? = null,
    val reachable: Boolean? = null,
    val latencyMs: Long? = null,
    val probeError: String? = null,
)

/** 纯 JVM 局域网 UDP 广播报文编解码器（兼容 savevault-ping 与历史 opensave-ping）。 */
object LanPingCodec {
    const val DEFAULT_DISCOVERY_PORT = 8385
    const val MULTICAST_GROUP = "224.0.0.1"
    const val DEFAULT_P2P_PORT = 8384
    private const val TYPE_CURRENT = "savevault-ping"
    private const val TYPE_LEGACY = "opensave-ping"

    fun encode(
        nodeId: String,
        deviceName: String,
        deviceType: String = "android",
        port: Int = DEFAULT_P2P_PORT,
    ): String {
        require(nodeId.isNotBlank()) { "nodeId 不能为空" }
        require(port in 1..65535) { "无效端口：$port" }
        return JSONObject().apply {
            put("type", TYPE_CURRENT)
            put("nodeId", nodeId.trim())
            put("deviceName", deviceName.trim().ifBlank { "Android" })
            put("deviceType", deviceType.trim().ifBlank { "android" })
            put("port", port)
        }.toString()
    }

    fun decode(
        raw: String,
        sourceAddress: String,
        selfNodeId: String = "",
        nowMs: Long = System.currentTimeMillis(),
    ): LanPeer? {
        val trimmed = raw.trim()
        if (trimmed.isEmpty() || !trimmed.startsWith("{")) return null
        val json = runCatching { JSONObject(trimmed) }.getOrNull() ?: return null
        val type = json.optString("type", "")
        if (type != TYPE_CURRENT && type != TYPE_LEGACY) return null
        val nodeId = json.optString("nodeId", "").trim()
        if (nodeId.isEmpty() || (selfNodeId.isNotBlank() && nodeId == selfNodeId)) return null
        val port = json.optInt("port", 0)
        if (port !in 1..65535) return null
        val cleanAddr = sourceAddress.trim().removePrefix("/")
        if (cleanAddr.isEmpty()) return null
        val deviceName = json.optString("deviceName", "").trim().ifBlank { nodeId.take(12) }
        val deviceType = json.optString("deviceType", "").trim().ifBlank { "unknown" }
        return LanPeer(
            nodeId = nodeId,
            deviceName = deviceName,
            deviceType = deviceType,
            address = cleanAddr,
            port = port,
            lastSeenMs = nowMs,
        )
    }

    /** 仅允许探测局域网私有/回环地址，防止明文 TCP 探测访问公网目标。 */
    fun isPrivateLanAddress(address: String): Boolean {
        val inet = runCatching { InetAddress.getByName(address.trim()) }.getOrNull() ?: return false
        if (inet.isLoopbackAddress || inet.isSiteLocalAddress || inet.isLinkLocalAddress) return true
        if (inet is Inet4Address) {
            val b = inet.address
            val first = b[0].toInt() and 0xFF
            val second = b[1].toInt() and 0xFF
            // CGNAT 100.64.0.0/10 (Tailscale / ZeroTier 虚拟局域网常用网段)
            if (first == 100 && second in 64..127) return true
        }
        return false
    }
}

/**
 * 局域网 P2P 对端扫描与连通性探测客户端。
 *
 * 1. 通过 UDP 8385 监听并发送 `savevault-ping` 广播/多播包，发现同一 Wi-Fi 下的
 *    SaveVault 桌面端（Windows/macOS/Linux）和 Steam Deck。
 * 2. 对局域网私有地址发送轻量 HTTP/1.1 `GET /api/p2p/ping` 探测对端版本、配对状态与 RTT。
 */
object LanPeerScanner {
    suspend fun discover(
        selfNodeId: String,
        selfDeviceName: String,
        listenPort: Int = LanPingCodec.DEFAULT_DISCOVERY_PORT,
        windowMs: Long = 2600L,
    ): List<LanPeer> = withContext(Dispatchers.IO) {
        val found = LinkedHashMap<String, LanPeer>()
        val payload = LanPingCodec.encode(selfNodeId, selfDeviceName).toByteArray(StandardCharsets.UTF_8)
        val socket = openDiscoverySocket(listenPort)
        socket.use { sock ->
            sock.soTimeout = 350
            // 主动发送一次广播 + 多播，让对端快速感知本机。
            runCatching {
                sock.broadcast = true
                val bcast = DatagramPacket(
                    payload,
                    payload.size,
                    InetAddress.getByName("255.255.255.255"),
                    listenPort,
                )
                sock.send(bcast)
            }
            runCatching {
                val mcast = DatagramPacket(
                    payload,
                    payload.size,
                    InetAddress.getByName(LanPingCodec.MULTICAST_GROUP),
                    listenPort,
                )
                sock.send(mcast)
            }

            val deadline = System.currentTimeMillis() + windowMs.coerceIn(500L, 10_000L)
            val buffer = ByteArray(2048)
            while (System.currentTimeMillis() < deadline) {
                val packet = DatagramPacket(buffer, buffer.size)
                try {
                    sock.receive(packet)
                    val text = String(packet.data, packet.offset, packet.length, StandardCharsets.UTF_8)
                    val peer = LanPingCodec.decode(
                        raw = text,
                        sourceAddress = packet.address?.hostAddress.orEmpty(),
                        selfNodeId = selfNodeId,
                    )
                    if (peer != null) {
                        found[peer.nodeId] = peer
                    }
                } catch (_: SocketTimeoutException) {
                    // 继续等待直到扫描窗口结束
                } catch (_: Exception) {
                    break
                }
            }
        }
        found.values.sortedBy { it.deviceName.lowercase() }
    }

    private fun openDiscoverySocket(port: Int): DatagramSocket {
        return runCatching {
            val ms = MulticastSocket(null)
            ms.reuseAddress = true
            ms.bind(InetSocketAddress(port))
            runCatching {
                @Suppress("DEPRECATION")
                ms.joinGroup(InetAddress.getByName(LanPingCodec.MULTICAST_GROUP))
            }
            ms
        }.getOrElse {
            DatagramSocket(null).apply {
                reuseAddress = true
                bind(InetSocketAddress(0))
            }
        }
    }

    suspend fun probePeer(
        peer: LanPeer,
        fromNodeId: String = "",
        timeoutMs: Int = 2500,
    ): LanPeer = withContext(Dispatchers.IO) {
        if (!LanPingCodec.isPrivateLanAddress(peer.address)) {
            return@withContext peer.copy(
                reachable = false,
                probeError = "仅允许探测局域网或虚拟局域网内网地址",
            )
        }
        val started = System.nanoTime()
        runCatching {
            val path = if (fromNodeId.isNotBlank()) {
                "/api/p2p/ping?from=${java.net.URLEncoder.encode(fromNodeId, "UTF-8")}"
            } else {
                "/api/p2p/ping"
            }
            val rawResponse = httpGetOverLanSocket(peer.address, peer.port, path, timeoutMs)
            val rttMs = ((System.nanoTime() - started) / 1_000_000L).coerceAtLeast(1L)
            val parsed = parsePingResponse(rawResponse)
            peer.copy(
                deviceName = parsed.optString("deviceName", "").ifBlank { peer.deviceName },
                deviceType = parsed.optString("deviceType", "").ifBlank { peer.deviceType },
                appVersion = parsed.optString("appVersion", peer.appVersion),
                paired = if (parsed.has("paired")) parsed.optBoolean("paired") else peer.paired,
                reachable = parsed.optString("status") == "ok",
                latencyMs = rttMs,
                probeError = null,
            )
        }.getOrElse { error ->
            peer.copy(
                reachable = false,
                latencyMs = null,
                probeError = error.message ?: "无法连接到局域网对端",
            )
        }
    }

    internal fun httpGetOverLanSocket(
        host: String,
        port: Int,
        path: String,
        timeoutMs: Int,
    ): String {
        Socket().use { socket ->
            socket.connect(InetSocketAddress(host, port), timeoutMs)
            socket.soTimeout = timeoutMs
            val req = "GET $path HTTP/1.1\r\nHost: $host:$port\r\nConnection: close\r\nUser-Agent: SaveVault-Android\r\n\r\n"
            socket.getOutputStream().apply {
                write(req.toByteArray(StandardCharsets.UTF_8))
                flush()
            }
            val out = ByteArrayOutputStream()
            val buf = ByteArray(4096)
            val input = socket.getInputStream()
            var total = 0
            while (total < 65536) {
                val n = input.read(buf)
                if (n <= 0) break
                out.write(buf, 0, n)
                total += n
            }
            return out.toString(StandardCharsets.UTF_8.name())
        }
    }

    internal fun parsePingResponse(rawHttp: String): JSONObject {
        val sep = rawHttp.indexOf("\r\n\r\n")
        require(sep > 0) { "对端响应格式异常" }
        val headerBlock = rawHttp.substring(0, sep)
        val statusLine = headerBlock.lineSequence().firstOrNull().orEmpty()
        val parts = statusLine.split(" ")
        val statusCode = parts.getOrNull(1)?.toIntOrNull() ?: 0
        require(statusCode in 200..299) { "对端返回 HTTP $statusCode" }
        val body = rawHttp.substring(sep + 4).trim()
        // 简单处理 chunked 传输编码（如果对端按分块发送）
        val jsonText = if (body.startsWith("{")) {
            body
        } else {
            val firstBrace = body.indexOf('{')
            val lastBrace = body.lastIndexOf('}')
            require(firstBrace >= 0 && lastBrace > firstBrace) { "对端未返回有效 JSON" }
            body.substring(firstBrace, lastBrace + 1)
        }
        return JSONObject(jsonText)
    }
}

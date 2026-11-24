package com.jun9100.savevault

import kotlinx.coroutines.CancellationException
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.isActive
import kotlinx.coroutines.withContext
import org.json.JSONObject
import java.io.ByteArrayOutputStream
import java.io.IOException
import java.net.DatagramPacket
import java.net.InetAddress
import java.net.InetSocketAddress
import java.net.MulticastSocket
import java.net.NetworkInterface
import java.net.Socket
import java.net.SocketTimeoutException
import java.net.URLEncoder
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

    /** 仅允许探测局域网私有/回环 IP 字面量（不做 DNS 解析，防止明文探测访问公网或 DNS 重绑定）。 */
    fun isPrivateLanAddress(address: String): Boolean {
        val trimmed = address.trim().removePrefix("[").removeSuffix("]")
        if (trimmed.isEmpty()) return false

        // IPv4 dotted-quad literal
        val octets = trimmed.split('.')
        if (octets.size == 4 && octets.all { it.isNotEmpty() && it.length <= 3 && it.all(Char::isDigit) }) {
            val a = octets[0].toInt()
            val b = octets[1].toInt()
            val c = octets[2].toInt()
            val d = octets[3].toInt()
            if (a > 255 || b > 255 || c > 255 || d > 255) return false
            return a == 127 ||
                a == 10 ||
                (a == 172 && b in 16..31) ||
                (a == 192 && b == 168) ||
                (a == 169 && b == 254) ||
                // CGNAT 100.64.0.0/10 (Tailscale / ZeroTier 虚拟局域网常用网段)
                (a == 100 && b in 64..127)
        }

        // IPv6 literal (optional zone id after '%')
        val noZone = trimmed.substringBefore('%')
        if (!noZone.contains(':') || !noZone.all { it in '0'..'9' || it in 'a'..'f' || it in 'A'..'F' || it == ':' || it == '.' }) {
            return false
        }
        val inet = try {
            InetAddress.getByName(trimmed)
        } catch (_: Exception) {
            return false
        }
        if (inet.isLoopbackAddress || inet.isLinkLocalAddress || inet.isSiteLocalAddress) return true
        val bytes = inet.address
        // IPv6 Unique Local Address (fc00::/7)
        return bytes.size == 16 && (bytes[0].toInt() and 0xFE) == 0xFC
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
    private const val MAX_PROBE_BYTES = 65_536

    suspend fun discover(
        selfNodeId: String,
        selfDeviceName: String,
        listenPort: Int = LanPingCodec.DEFAULT_DISCOVERY_PORT,
        windowMs: Long = 3800L,
    ): List<LanPeer> = withContext(Dispatchers.IO) {
        val found = LinkedHashMap<String, LanPeer>()
        val payload = LanPingCodec.encode(selfNodeId, selfDeviceName).toByteArray(StandardCharsets.UTF_8)
        openDiscoverySocket(listenPort).use { sock ->
            sock.soTimeout = 350
            sendDiscoveryBeacons(sock, payload, listenPort)

            val deadlineNs = System.nanoTime() + windowMs.coerceIn(500L, 10_000L) * 1_000_000L
            val buffer = ByteArray(2048)
            while (currentCoroutineContext().isActive && System.nanoTime() < deadlineNs) {
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
                }
            }
        }
        found.values.sortedBy { it.deviceName.lowercase() }
    }

    private fun openDiscoverySocket(port: Int): MulticastSocket {
        val ms = MulticastSocket(null)
        try {
            ms.reuseAddress = true
            ms.bind(InetSocketAddress(port))
            try {
                @Suppress("DEPRECATION")
                ms.joinGroup(InetAddress.getByName(LanPingCodec.MULTICAST_GROUP))
            } catch (_: IOException) {
                // 广播仍可正常工作，多播加入失败不阻断发现。
            }
            return ms
        } catch (e: IOException) {
            ms.close()
            throw e
        }
    }

    private fun sendDiscoveryBeacons(sock: MulticastSocket, payload: ByteArray, port: Int) {
        val targets = linkedSetOf<InetAddress>()
        try {
            sock.broadcast = true
            targets += InetAddress.getByName("255.255.255.255")
        } catch (_: IOException) {
        }
        try {
            targets += InetAddress.getByName(LanPingCodec.MULTICAST_GROUP)
        } catch (_: IOException) {
        }
        val ifaces = try {
            NetworkInterface.getNetworkInterfaces()
        } catch (_: IOException) {
            null
        }
        while (ifaces != null && ifaces.hasMoreElements()) {
            val iface = ifaces.nextElement()
            val active = try {
                iface.isUp && !iface.isLoopback
            } catch (_: IOException) {
                false
            }
            if (!active) continue
            for (addr in iface.interfaceAddresses) {
                addr.broadcast?.let { targets += it }
            }
        }
        for (target in targets) {
            try {
                sock.send(DatagramPacket(payload, payload.size, target, port))
            } catch (_: IOException) {
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
        try {
            val path = if (fromNodeId.isNotBlank()) {
                "/api/p2p/ping?from=${URLEncoder.encode(fromNodeId, "UTF-8")}"
            } else {
                "/api/p2p/ping"
            }
            val rawResponse = httpGetOverLanSocket(peer.address, peer.port, path, timeoutMs)
            val rttMs = ((System.nanoTime() - started) / 1_000_000L).coerceAtLeast(1L)
            val parsed = parsePingResponse(rawResponse)
            val ok = parsed.optString("status") == "ok"
            peer.copy(
                deviceName = parsed.optString("deviceName", "").ifBlank { peer.deviceName },
                deviceType = parsed.optString("deviceType", "").ifBlank { peer.deviceType },
                appVersion = parsed.optString("appVersion", peer.appVersion),
                paired = if (parsed.has("paired")) parsed.optBoolean("paired") else peer.paired,
                reachable = ok,
                latencyMs = if (ok) rttMs else null,
                probeError = if (ok) null else parsed.optString("error").ifBlank { "对端状态异常" },
            )
        } catch (e: CancellationException) {
            throw e
        } catch (error: Exception) {
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
        val hostHeader = if (host.contains(':') && !host.startsWith('[')) "[$host]" else host
        Socket().use { socket ->
            socket.connect(InetSocketAddress(host, port), timeoutMs)
            socket.soTimeout = timeoutMs
            val req = "GET $path HTTP/1.1\r\nHost: $hostHeader:$port\r\nConnection: close\r\nUser-Agent: SaveVault-Android\r\n\r\n"
            socket.getOutputStream().apply {
                write(req.toByteArray(StandardCharsets.UTF_8))
                flush()
            }
            val raw = ByteArrayOutputStream()
            val buf = ByteArray(4096)
            val input = socket.getInputStream()
            var headerEnd = -1
            var contentLength = -1
            var isChunked = false
            while (raw.size() < MAX_PROBE_BYTES) {
                val n = input.read(buf, 0, minOf(buf.size, MAX_PROBE_BYTES - raw.size()))
                if (n <= 0) break
                raw.write(buf, 0, n)
                if (headerEnd < 0) {
                    val bytes = raw.toByteArray()
                    headerEnd = indexOfHeaderEnd(bytes)
                    if (headerEnd >= 0) {
                        val headers = String(bytes, 0, headerEnd, StandardCharsets.ISO_8859_1)
                        for (line in headers.lineSequence().drop(1)) {
                            val colon = line.indexOf(':')
                            if (colon <= 0) continue
                            val key = line.substring(0, colon).trim().lowercase()
                            val value = line.substring(colon + 1).trim()
                            if (key == "content-length") contentLength = value.toIntOrNull() ?: -1
                            if (key == "transfer-encoding" && value.lowercase().contains("chunked")) isChunked = true
                        }
                    }
                }
                if (headerEnd >= 0) {
                    val bodyBytes = raw.size() - (headerEnd + 4)
                    if (contentLength >= 0 && bodyBytes >= contentLength) break
                    if (isChunked && raw.toString(StandardCharsets.ISO_8859_1.name()).indexOf("\r\n0\r\n", headerEnd) >= 0) break
                }
            }
            return raw.toString(StandardCharsets.UTF_8.name())
        }
    }

    internal fun parsePingResponse(rawHttp: String): JSONObject {
        val sep = rawHttp.indexOf("\r\n\r\n")
        require(sep > 0) { "对端响应格式异常" }
        val headerBlock = rawHttp.substring(0, sep)
        val statusLine = headerBlock.lineSequence().firstOrNull().orEmpty()
        val statusCode = statusLine.split(" ").getOrNull(1)?.toIntOrNull() ?: 0
        require(statusCode in 200..299) { "对端返回 HTTP $statusCode" }
        val isChunked = headerBlock.lineSequence().drop(1).any { line ->
            val colon = line.indexOf(':')
            colon > 0 &&
                line.substring(0, colon).trim().equals("transfer-encoding", ignoreCase = true) &&
                line.substring(colon + 1).lowercase().contains("chunked")
        }
        val rawBody = rawHttp.substring(sep + 4)
        val decodedBody = if (isChunked) {
            decodeChunked(rawBody.toByteArray(StandardCharsets.UTF_8)).trim()
        } else {
            rawBody.trim()
        }
        require(decodedBody.startsWith("{")) { "对端未返回有效 JSON" }
        return JSONObject(decodedBody)
    }

    private fun indexOfHeaderEnd(bytes: ByteArray): Int {
        for (i in 0..bytes.size - 4) {
            if (bytes[i] == '\r'.code.toByte() &&
                bytes[i + 1] == '\n'.code.toByte() &&
                bytes[i + 2] == '\r'.code.toByte() &&
                bytes[i + 3] == '\n'.code.toByte()
            ) {
                return i
            }
        }
        return -1
    }

    private fun decodeChunked(bodyBytes: ByteArray): String {
        val out = ByteArrayOutputStream()
        var pos = 0
        while (pos < bodyBytes.size) {
            var lineEnd = pos
            while (lineEnd + 1 < bodyBytes.size &&
                !(bodyBytes[lineEnd] == '\r'.code.toByte() && bodyBytes[lineEnd + 1] == '\n'.code.toByte())
            ) {
                lineEnd++
            }
            if (lineEnd + 1 >= bodyBytes.size) break
            val sizeLine = String(bodyBytes, pos, lineEnd - pos, StandardCharsets.ISO_8859_1)
                .substringBefore(';').trim()
            val size = sizeLine.toIntOrNull(16) ?: break
            pos = lineEnd + 2
            if (size == 0) break
            if (pos + size > bodyBytes.size) break
            out.write(bodyBytes, pos, size)
            pos += size
            if (pos + 1 < bodyBytes.size &&
                bodyBytes[pos] == '\r'.code.toByte() &&
                bodyBytes[pos + 1] == '\n'.code.toByte()
            ) {
                pos += 2
            }
        }
        return out.toString(StandardCharsets.UTF_8.name())
    }
}

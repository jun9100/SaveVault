package com.jun9100.savevault

import kotlinx.coroutines.runBlocking
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertNull
import org.junit.Assert.assertTrue
import org.junit.Test
import java.io.BufferedReader
import java.io.InputStreamReader
import java.io.OutputStreamWriter
import java.net.InetAddress
import java.net.ServerSocket
import java.nio.charset.StandardCharsets
import java.util.concurrent.Executors

class LanPeerDiscoveryTest {
    @Test
    fun lanPingCodecEncodesAndDecodesCurrentAndLegacyFormats() {
        val raw = LanPingCodec.encode(
            nodeId = "android-odin2",
            deviceName = "AYN Odin 2",
            deviceType = "android",
            port = 8384,
        )
        val peer = LanPingCodec.decode(raw, sourceAddress = "192.168.1.42", selfNodeId = "pc-desktop", nowMs = 1234L)
        assertNotNull(peer)
        assertEquals("android-odin2", peer!!.nodeId)
        assertEquals("AYN Odin 2", peer.deviceName)
        assertEquals("android", peer.deviceType)
        assertEquals("192.168.1.42", peer.address)
        assertEquals(8384, peer.port)

        // Self-ping must be ignored
        assertNull(LanPingCodec.decode(raw, sourceAddress = "192.168.1.42", selfNodeId = "android-odin2"))

        // Legacy opensave-ping remains wire-compatible
        val legacy = """{"type":"opensave-ping","nodeId":"deck-1","deviceName":"Steam Deck","deviceType":"linux","port":8384}"""
        val legacyPeer = LanPingCodec.decode(legacy, sourceAddress = "10.0.0.15", selfNodeId = "android-odin2")
        assertNotNull(legacyPeer)
        assertEquals("Steam Deck", legacyPeer!!.deviceName)

        // Invalid types or ports are rejected
        assertNull(LanPingCodec.decode("""{"type":"other","nodeId":"x","port":8384}""", "192.168.1.2"))
        assertNull(LanPingCodec.decode("""{"type":"savevault-ping","nodeId":"x","port":0}""", "192.168.1.2"))
    }

    @Test
    fun privateLanAddressPolicyBlocksPublicIps() {
        assertTrue(LanPingCodec.isPrivateLanAddress("127.0.0.1"))
        assertTrue(LanPingCodec.isPrivateLanAddress("192.168.50.10"))
        assertTrue(LanPingCodec.isPrivateLanAddress("10.0.1.9"))
        assertTrue(LanPingCodec.isPrivateLanAddress("172.16.4.2"))
        assertTrue(LanPingCodec.isPrivateLanAddress("100.64.12.3"))
        assertFalse(LanPingCodec.isPrivateLanAddress("8.8.8.8"))
    }

    @Test
    fun probePeerQueriesDesktopPingEndpointOverLanSocket() = runBlocking {
        val server = ServerSocket(0, 4, InetAddress.getByName("127.0.0.1"))
        val pool = Executors.newSingleThreadExecutor()
        try {
            pool.submit {
                server.accept().use { client ->
                    val reader = BufferedReader(InputStreamReader(client.getInputStream(), StandardCharsets.UTF_8))
                    val requestLine = reader.readLine().orEmpty()
                    while (true) {
                        val line = reader.readLine()
                        if (line.isNullOrEmpty()) break
                    }
                    val body = """{"status":"ok","paired":true,"deviceName":"Workstation","deviceType":"windows","appVersion":"1.5.0"}"""
                    val status = if (requestLine.startsWith("GET /api/p2p/ping")) "200 OK" else "404 Not Found"
                    val writer = OutputStreamWriter(client.getOutputStream(), StandardCharsets.UTF_8)
                    writer.write("HTTP/1.1 $status\r\nContent-Type: application/json\r\nContent-Length: ${body.length}\r\nConnection: close\r\n\r\n$body")
                    writer.flush()
                }
            }

            val peer = LanPeer(
                nodeId = "pc-1",
                deviceName = "PC",
                deviceType = "windows",
                address = "127.0.0.1",
                port = server.localPort,
                lastSeenMs = 0L,
            )
            val probed = LanPeerScanner.probePeer(peer, fromNodeId = "android-odin2")
            assertEquals(true, probed.reachable)
            assertEquals("Workstation", probed.deviceName)
            assertEquals("1.5.0", probed.appVersion)
            assertEquals(true, probed.paired)
            assertNotNull(probed.latencyMs)
        } finally {
            server.close()
            pool.shutdownNow()
        }
    }
}

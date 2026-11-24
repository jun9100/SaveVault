package com.jun9100.savevault

import java.io.BufferedInputStream
import java.io.BufferedWriter
import java.io.ByteArrayOutputStream
import java.io.File
import java.io.OutputStreamWriter
import java.net.ServerSocket
import java.net.Socket
import java.net.URI
import java.util.concurrent.ConcurrentHashMap
import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.assertEquals
import org.junit.Assert.assertFalse
import org.junit.Assert.assertNotNull
import org.junit.Assert.assertTrue
import org.junit.Test

class GameSaveClientTest {
    @Test
    fun serviceRootsNormalizeWithoutExposingProtocolPaths() {
        assertEquals("https://nas.example:8788", GameSaveClient.normalizeServiceUrl("https://nas.example:8788/"))
        assertEquals("https://nas.example/game-save", GameSaveClient.normalizeServiceUrl("https://nas.example/game-save/v1/savevault/"))
        assertTrue(runCatching { GameSaveClient.normalizeServiceUrl("https://user:pass@nas.example") }.isFailure)
        assertTrue(runCatching { GameSaveClient.normalizeServiceUrl("ftp://nas.example") }.isFailure)
    }

    @Test
    fun listUploadDownloadAndDeleteCarryBearerAndType() {
        val server = MiniGameSaveServer()
        server.start()
        val root = createTempDir(prefix = "savevault-game-save-")
        try {
            val client = GameSaveClient(
                GameSaveConfig(enabled = true, serviceUrl = "http://127.0.0.1:${server.port}", syncKey = "sync-key"),
                GameSaveDeviceIdentity(
                    id = "odin2-test-1",
                    name = "Odin2 Test",
                    version = "0.3.15",
                ),
            )
            val local = File(root, "mario.zip").apply { writeText("zip-content") }
            val name = "switch-0100000000010000__main__snap_1.zip"

            client.testConnection().getOrThrow()
            val uploaded = client.upload(local, GameSaveTypes.SWITCH, name).getOrThrow()
            assertEquals("switch", uploaded.type)
            assertEquals("zip-content", String(server.files["switch/$name"]!!))

            val listed = client.list().getOrThrow()
            assertEquals(1, listed.size)
            assertEquals("switch", listed.single().type)
            assertEquals(name, listed.single().name)

            val downloaded = File(root, "downloaded.zip")
            client.download(listed.single(), downloaded).getOrThrow()
            assertEquals("zip-content", downloaded.readText())

            client.delete(listed.single()).getOrThrow()
            assertFalse(server.files.containsKey("switch/$name"))
            assertTrue(server.authorizedRequests > 0)
            server.requestHeaders.forEach { headers ->
                for (family in listOf("x-savevault", "x-eden")) {
                    assertEquals("odin2-test-1", headers["$family-device-id"])
                    assertEquals("Odin2 Test", headers["$family-device-name"])
                    assertEquals("savevault-android", headers["$family-platform"])
                    assertEquals("0.3.15", headers["$family-version"])
                }
            }
        } finally {
            root.deleteRecursively()
            server.close()
        }
    }

    @Test
    fun nativeScopesUseCasRetainConflictsAndOnlyFallbackOnMissingRoute() {
        val server = MiniGameSaveServer()
        server.start()
        val root = createTempDir(prefix = "savevault-native-scope-")
        try {
            val client = GameSaveClient(
                GameSaveConfig(enabled = true, serviceUrl = "http://127.0.0.1:${server.port}", syncKey = "sync-key"),
                GameSaveDeviceIdentity("odin2", "Odin2"),
            )
            val gameId = "switch-0100000000010000"
            val firstID = "snap_1730000000000--dev-odin2"
            val firstArchive = "${gameId}__main__${firstID}.zip"
            val firstFile = File(root, firstArchive).apply { writeText("progress-1788") }

            assertEquals(null, client.getScope(GameSaveTypes.SWITCH, gameId).getOrThrow().head)
            client.upload(firstFile, GameSaveTypes.SWITCH, firstArchive).getOrThrow()
            val first = GameSaveScopeCandidate(
                id = firstID,
                archive = firstArchive,
                parent = "",
                contentDigest = Hashing.sha256(firstFile),
                createdAt = "2026-10-26T00:00:00Z",
            )
            assertEquals(firstID, client.putScopeSnapshot(GameSaveTypes.SWITCH, gameId, first, "").getOrThrow().id)

            val staleID = "snap_1730000001000--dev-desktop1"
            val staleArchive = "${gameId}__main__${staleID}.zip"
            val staleFile = File(root, staleArchive).apply { writeText("progress-1789") }
            client.upload(staleFile, GameSaveTypes.SWITCH, staleArchive).getOrThrow()
            val stale = GameSaveScopeCandidate(
                id = staleID,
                archive = staleArchive,
                parent = "",
                contentDigest = Hashing.sha256(staleFile),
                createdAt = "2026-10-26T00:01:00Z",
            )
            val race = client.putScopeSnapshot(GameSaveTypes.SWITCH, gameId, stale, "")
            assertTrue(race.exceptionOrNull() is GameSaveScopeConflictException)
            assertEquals(firstID, (race.exceptionOrNull() as GameSaveScopeConflictException).head)
            client.putScopeConflict(GameSaveTypes.SWITCH, gameId, stale).getOrThrow()

            val scope = client.getScope(GameSaveTypes.SWITCH, gameId).getOrThrow()
            assertEquals(firstID, scope.head?.id)
            assertEquals(listOf(staleID), scope.conflicts.map(GameSaveScopeSnapshot::id))

            server.scopeWritesEnabled = false
            val write404 = client.putScopeSnapshot(
                GameSaveTypes.SWITCH,
                gameId,
                stale.copy(
                    id = "snap_1730000002000--dev-odin2",
                    archive = "${gameId}__main__snap_1730000002000--dev-odin2.zip",
                    parent = firstID,
                ),
                firstID,
            ).exceptionOrNull()
            assertTrue(write404 is GameSaveException)
            assertFalse(write404 is GameSaveScopesUnsupportedException)

            server.scopeRoutesEnabled = false
            assertTrue(client.getScope(GameSaveTypes.SWITCH, gameId).exceptionOrNull() is GameSaveScopesUnsupportedException)
        } finally {
            root.deleteRecursively()
            server.close()
        }
    }

    @Test
    fun whoamiQuotaDevicesRevokeAndSnapshotLockDeleteWorkEndToEnd() {
        val server = MiniGameSaveServer()
        server.start()
        val root = createTempDir(prefix = "savevault-p0-")
        try {
            val client = GameSaveClient(
                GameSaveConfig(enabled = true, serviceUrl = "http://127.0.0.1:${server.port}", syncKey = "sync-key"),
                GameSaveDeviceIdentity("odin2", "Odin2"),
            )

            val who = client.whoami().getOrThrow()
            assertNotNull(who)
            assertEquals("device", who!!.auth)
            assertEquals("aa11223344556677", who.tokenId)
            assertEquals("alice", who.username)
            assertEquals("u-alice", who.vaultId)
            assertEquals(512L * 1024L * 1024L, who.usedBytes)
            assertEquals(2L * 1024L * 1024L * 1024L, who.quotaBytes)

            val devices = client.listDevices().getOrThrow()
            assertEquals("aa11223344556677", devices.currentTokenId)
            assertEquals(2, devices.tokens.size)
            assertEquals("aa11223344556677", devices.tokens.first().id)
            assertFalse(devices.tokens[1].revoked)

            client.revokeDevice("bb11223344556677").getOrThrow()
            val afterRevoke = client.listDevices().getOrThrow()
            assertTrue(afterRevoke.tokens.first { it.id == "bb11223344556677" }.revoked)

            val gameId = "switch-0100000000020000"
            val snap1ID = "snap_1730000010000--dev-odin2"
            val snap1Archive = "${gameId}__main__${snap1ID}.zip"
            val snap1File = File(root, snap1Archive).apply { writeText("v1") }
            client.upload(snap1File, GameSaveTypes.SWITCH, snap1Archive).getOrThrow()
            client.putScopeSnapshot(
                GameSaveTypes.SWITCH,
                gameId,
                GameSaveScopeCandidate(snap1ID, snap1Archive, "", Hashing.sha256(snap1File), "2026-10-26T00:00:00Z"),
                "",
            ).getOrThrow()

            val snap2ID = "snap_1730000020000--dev-odin2"
            val snap2Archive = "${gameId}__main__${snap2ID}.zip"
            val snap2File = File(root, snap2Archive).apply { writeText("v2") }
            client.upload(snap2File, GameSaveTypes.SWITCH, snap2Archive).getOrThrow()
            client.putScopeSnapshot(
                GameSaveTypes.SWITCH,
                gameId,
                GameSaveScopeCandidate(snap2ID, snap2Archive, snap1ID, Hashing.sha256(snap2File), "2026-10-26T00:05:00Z"),
                snap1ID,
            ).getOrThrow()

            // Lock snap1, verify scope reports locked=true and delete fails with 409 non-retryable.
            client.setScopeSnapshotLock(GameSaveTypes.SWITCH, gameId, snap1ID, locked = true, note = "boss").getOrThrow()
            val lockedScope = client.getScope(GameSaveTypes.SWITCH, gameId).getOrThrow()
            assertTrue(lockedScope.snapshots.first { it.id == snap1ID }.locked)

            val deleteLocked = client.deleteScopeSnapshot(GameSaveTypes.SWITCH, gameId, snap1ID).exceptionOrNull()
            assertTrue(deleteLocked is GameSaveException)
            assertEquals(409, (deleteLocked as GameSaveException).httpStatus)
            assertFalse(deleteLocked.retryable)

            // Deleting current head fails with 409.
            val deleteHead = client.deleteScopeSnapshot(GameSaveTypes.SWITCH, gameId, snap2ID).exceptionOrNull()
            assertTrue(deleteHead is GameSaveException)
            assertEquals(409, (deleteHead as GameSaveException).httpStatus)

            // Unlock snap1 and delete succeeds.
            client.setScopeSnapshotLock(GameSaveTypes.SWITCH, gameId, snap1ID, locked = false).getOrThrow()
            client.deleteScopeSnapshot(GameSaveTypes.SWITCH, gameId, snap1ID).getOrThrow()
            val finalScope = client.getScope(GameSaveTypes.SWITCH, gameId).getOrThrow()
            assertEquals(listOf(snap2ID), finalScope.snapshots.map(GameSaveScopeSnapshot::id))

            // Cross-device profile metadata GET & PUT merge.
            val merged = client.mergeProfileMetadata(
                customTitles = mapOf("3ds:0004000000055D00" to "宝可梦 X"),
                excludedScopes = setOf("android:com.example.temp"),
            ).getOrThrow()
            assertNotNull(merged)
            assertEquals(1L, merged!!.revision)
            assertEquals("宝可梦 X", merged.customTitles["3ds:0004000000055D00"])
            assertEquals(setOf("android:com.example.temp"), merged.excludedScopes)

            val fetched = client.getProfileMetadata().getOrThrow()
            assertNotNull(fetched)
            assertEquals(1L, fetched!!.revision)
            assertEquals("宝可梦 X", fetched.customTitles["3ds:0004000000055D00"])
        } finally {
            root.deleteRecursively()
            server.close()
        }
    }
}

/** Small HTTP server keeps the native contract test independent of WebDAV or a real NAS. */
private class MiniGameSaveServer : AutoCloseable {
    val files = ConcurrentHashMap<String, ByteArray>()
    private val socket = ServerSocket(0)
    private var thread: Thread? = null
    var authorizedRequests = 0
        private set
    val requestHeaders = mutableListOf<Map<String, String>>()
    @Volatile
    var scopeRoutesEnabled = true
    @Volatile
    var scopeWritesEnabled = true
    val port: Int get() = socket.localPort

    fun start() {
        thread = Thread {
            while (!socket.isClosed) runCatching { socket.accept().use(::handle) }
        }.apply {
            isDaemon = true
            start()
        }
    }

    private fun handle(client: Socket) {
        val input = BufferedInputStream(client.getInputStream())
        val requestLine = readAsciiLine(input) ?: return
        val parts = requestLine.split(' ')
        if (parts.size < 2) return
        val method = parts[0]
        val path = URI(parts[1]).path
        val headers = mutableMapOf<String, String>()
        while (true) {
            val line = readAsciiLine(input) ?: return
            if (line.isEmpty()) break
            val separator = line.indexOf(':')
            if (separator > 0) headers[line.substring(0, separator).lowercase()] = line.substring(separator + 1).trim()
        }
        val body = readRequestBody(input, headers)
        if (headers["authorization"] != "Bearer sync-key") {
            respond(client, 401, ByteArray(0))
            return
        }
        authorizedRequests++
        requestHeaders += headers
        if (method == "GET" && path == "/v1/auth/whoami") {
            val payload = JSONObject()
                .put("auth", "device")
                .put("token_id", "aa11223344556677")
                .put("device_id", "odin2")
                .put("name", "Odin2")
                .put("shared_token_mode", "allow")
                .put("vault_id", "u-alice")
                .put("username", "alice")
                .put("used_bytes", 512L * 1024L * 1024L)
                .put("quota_bytes", 2L * 1024L * 1024L * 1024L)
                .toString()
                .toByteArray()
            respond(client, 200, payload, "Content-Type: application/json")
            return
        }
        if (method == "GET" && path == "/v1/auth/devices") {
            val arr = JSONArray()
            synchronized(deviceTokens) {
                deviceTokens.values.forEach { tok ->
                    arr.put(
                        JSONObject()
                            .put("id", tok.id)
                            .put("name", tok.name)
                            .put("device_id", tok.deviceId)
                            .put("display_name", tok.displayName)
                            .put("online", tok.online)
                            .put("created_at", tok.createdAt)
                            .put("last_used_at", tok.lastUsedAt)
                            .apply { if (tok.revokedAt.isNotBlank()) put("revoked_at", tok.revokedAt) },
                    )
                }
            }
            val payload = JSONObject()
                .put("tokens", arr)
                .put("current_token_id", "aa11223344556677")
                .put("current_device_id", "odin2")
                .put("shared_token_mode", "allow")
                .put("used_bytes", 512L * 1024L * 1024L)
                .put("quota_bytes", 2L * 1024L * 1024L * 1024L)
                .toString()
                .toByteArray()
            respond(client, 200, payload, "Content-Type: application/json")
            return
        }
        if (method == "POST" && path.startsWith("/v1/auth/devices/") && path.endsWith("/revoke")) {
            val tokenId = path.removePrefix("/v1/auth/devices/").removeSuffix("/revoke")
            synchronized(deviceTokens) {
                val existing = deviceTokens[tokenId]
                if (existing == null) {
                    respond(client, 404, ByteArray(0))
                    return
                }
                deviceTokens[tokenId] = existing.copy(revokedAt = "2026-10-26T00:10:00Z")
            }
            respond(client, 204, ByteArray(0))
            return
        }
        if (path == "/v1/savevault/profile/metadata") {
            synchronized(profileTitles) {
                if (method == "PUT") {
                    val reqJson = JSONObject(String(body))
                    reqJson.optJSONObject("custom_titles")?.let { obj ->
                        val keys = obj.keys()
                        while (keys.hasNext()) {
                            val k = keys.next()
                            profileTitles[k] = obj.optString(k)
                        }
                    }
                    reqJson.optJSONArray("remove_titles")?.let { arr ->
                        for (i in 0 until arr.length()) profileTitles.remove(arr.optString(i))
                    }
                    reqJson.optJSONArray("excluded_scopes")?.let { arr ->
                        for (i in 0 until arr.length()) profileExcluded += arr.optString(i)
                    }
                    reqJson.optJSONArray("remove_excluded")?.let { arr ->
                        for (i in 0 until arr.length()) profileExcluded.remove(arr.optString(i))
                    }
                    profileRevision++
                }
                val payload = JSONObject()
                    .put("revision", profileRevision)
                    .put("updated_at", "2026-11-24T06:00:00Z")
                    .put("custom_titles", JSONObject().apply { profileTitles.forEach { (k, v) -> put(k, v) } })
                    .put("excluded_scopes", JSONArray().apply { profileExcluded.forEach { put(it) } })
                    .toString()
                    .toByteArray()
                respond(client, 200, payload, "Content-Type: application/json")
            }
            return
        }
        if (method == "GET" && path == "/v1/savevault/files") {
            val payload = JSONObject().put("files", JSONArray().apply {
                files.entries.sortedBy { it.key }.forEach { (key, value) ->
                    val type = key.substringBefore('/')
                    val name = key.substringAfter('/')
                    put(
                        JSONObject()
                            .put("type", type)
                            .put("name", name)
                            .put("sizeBytes", value.size)
                            .put("createdTime", "2026-10-24T00:00:00Z"),
                    )
                }
            }).toString().toByteArray()
            respond(client, 200, payload, "Content-Type: application/json")
            return
        }

        if (path.startsWith("/v1/savevault/scopes/")) {
            handleScope(client, method, path, headers, body)
            return
        }

        val segments = path.removePrefix("/v1/savevault/files/").split('/')
        if (segments.size != 2 || !path.startsWith("/v1/savevault/files/")) {
            respond(client, 404, ByteArray(0))
            return
        }
        val key = "${segments[0]}/${segments[1]}"
        when (method) {
            "PUT" -> {
                files[key] = body
                respond(client, 201, ByteArray(0))
            }
            "GET" -> {
                val value = files[key]
                if (value == null) respond(client, 404, ByteArray(0))
                else respond(client, 200, value, "Content-Type: application/zip")
            }
            "DELETE" -> {
                if (files.remove(key) == null) respond(client, 404, ByteArray(0)) else respond(client, 204, ByteArray(0))
            }
            else -> respond(client, 405, ByteArray(0))
        }
    }

    private data class DeviceTokenStub(
        val id: String,
        val name: String,
        val deviceId: String,
        val displayName: String,
        val online: Boolean,
        val createdAt: String,
        val lastUsedAt: String,
        val revokedAt: String = "",
    )

    private val deviceTokens = linkedMapOf(
        "bb11223344556677" to DeviceTokenStub(
            id = "bb11223344556677",
            name = "Steam Deck",
            deviceId = "deck-1",
            displayName = "Steam Deck",
            online = false,
            createdAt = "2026-10-25T00:00:00Z",
            lastUsedAt = "2026-10-25T12:00:00Z",
        ),
        "aa11223344556677" to DeviceTokenStub(
            id = "aa11223344556677",
            name = "Odin2",
            deviceId = "odin2",
            displayName = "Odin2",
            online = true,
            createdAt = "2026-10-26T00:00:00Z",
            lastUsedAt = "2026-10-26T00:05:00Z",
        ),
    )

    private data class ScopeRecord(
        val id: String,
        val archive: String,
        val parent: String,
        val digest: String,
        val createdAt: String,
        val receivedAt: String,
        val conflict: Boolean = false,
        val conflictOf: String = "",
        val locked: Boolean = false,
    )

    private data class ScopeState(
        var head: String = "",
        val records: LinkedHashMap<String, ScopeRecord> = linkedMapOf(),
    )

    private val scopes = linkedMapOf<String, ScopeState>()
    private var profileRevision = 0L
    private val profileTitles = linkedMapOf<String, String>()
    private val profileExcluded = linkedSetOf<String>()

    private fun handleScope(
        client: Socket,
        method: String,
        path: String,
        headers: Map<String, String>,
        body: ByteArray,
    ) {
        if (!scopeRoutesEnabled) {
            respond(client, 404, ByteArray(0))
            return
        }
        val segments = path.removePrefix("/v1/savevault/scopes/").split('/')
        if (segments.size !in setOf(2, 4, 5)) {
            respond(client, 404, ByteArray(0))
            return
        }
        val type = segments[0]
        val gameId = segments[1]
        val key = "$type/$gameId"
        synchronized(scopes) {
            val state = scopes.getOrPut(key, ::ScopeState)
            if (segments.size == 2 && method == "GET") {
                respond(client, 200, scopeJson(type, gameId, state).toString().toByteArray(), "Content-Type: application/json")
                return
            }
            if (segments.size == 5 && segments[2] == "snapshots" && segments[4] == "lock") {
                val snapId = segments[3]
                val existing = state.records[snapId]
                if (existing == null) {
                    respond(client, 404, ByteArray(0))
                    return
                }
                when (method) {
                    "PUT" -> {
                        state.records[snapId] = existing.copy(locked = true)
                        respond(client, 200, JSONObject().put("locked", true).toString().toByteArray(), "Content-Type: application/json")
                    }
                    "DELETE" -> {
                        state.records[snapId] = existing.copy(locked = false)
                        respond(client, 204, ByteArray(0))
                    }
                    else -> respond(client, 405, ByteArray(0))
                }
                return
            }
            if (segments.size == 4 && segments[2] == "snapshots" && method == "DELETE") {
                val snapId = segments[3]
                val existing = state.records[snapId]
                if (existing == null) {
                    respond(client, 404, ByteArray(0))
                    return
                }
                if (state.head == snapId) {
                    respond(
                        client,
                        409,
                        JSONObject().put("error", "cannot delete the current head snapshot").toString().toByteArray(),
                        "Content-Type: application/json",
                    )
                    return
                }
                if (existing.locked) {
                    respond(
                        client,
                        409,
                        JSONObject().put("error", "cannot delete a locked snapshot; unlock it first").toString().toByteArray(),
                        "Content-Type: application/json",
                    )
                    return
                }
                state.records.remove(snapId)
                respond(client, 200, JSONObject().put("deleted", snapId).toString().toByteArray(), "Content-Type: application/json")
                return
            }
            if (segments.size != 4 || method != "PUT") {
                respond(client, 405, ByteArray(0))
                return
            }
            if (!scopeWritesEnabled) {
                respond(client, 404, ByteArray(0))
                return
            }
            val payload = runCatching { JSONObject(String(body)) }.getOrNull()
            if (payload == null || payload.optString("id") != segments[3]) {
                respond(client, 400, ByteArray(0))
                return
            }
            val candidate = ScopeRecord(
                id = payload.getString("id"),
                archive = payload.getString("archive"),
                parent = payload.optString("parent"),
                digest = payload.getString("contentDigest"),
                createdAt = payload.getString("createdAt"),
                receivedAt = "2026-10-26T00:02:00Z",
            )
            when (segments[2]) {
                "snapshots" -> {
                    val expected = headers["if-match"]?.removePrefix("\"")?.removeSuffix("\"")
                    if (expected != state.head || candidate.parent != state.head) {
                        respond(client, 409, JSONObject().put("head", state.head).toString().toByteArray(), "Content-Type: application/json")
                        return
                    }
                    state.records[candidate.id] = candidate
                    state.head = candidate.id
                    respond(client, 201, recordJson(candidate).toString().toByteArray(), "Content-Type: application/json")
                }

                "conflicts" -> {
                    if (state.head.isBlank() || candidate.parent == state.head) {
                        respond(client, 409, JSONObject().put("head", state.head).toString().toByteArray(), "Content-Type: application/json")
                        return
                    }
                    val retained = candidate.copy(conflict = true, conflictOf = state.head)
                    state.records[retained.id] = retained
                    respond(client, 201, recordJson(retained).toString().toByteArray(), "Content-Type: application/json")
                }

                else -> respond(client, 404, ByteArray(0))
            }
        }
    }

    private fun scopeJson(type: String, gameId: String, state: ScopeState): JSONObject {
        val snapshots = JSONArray()
        val conflicts = JSONArray()
        state.records.values.forEach { record ->
            snapshots.put(recordJson(record))
            if (record.conflict) conflicts.put(recordJson(record))
        }
        return JSONObject()
            .put("type", type)
            .put("gameId", gameId)
            .put("head", state.records[state.head]?.let(::recordJson) ?: JSONObject.NULL)
            .put("snapshots", snapshots)
            .put("conflicts", conflicts)
    }

    private fun recordJson(record: ScopeRecord): JSONObject = JSONObject()
        .put("id", record.id)
        .put("archive", record.archive)
        .put("parent", record.parent)
        .put("contentDigest", record.digest)
        .put("createdAt", record.createdAt)
        .put("receivedAt", record.receivedAt)
        .put("device", "test-device")
        .put("deviceName", "Test device")
        .put("platform", "savevault-android")
        .put("conflict", record.conflict)
        .put("conflictOf", record.conflictOf)
        .put("locked", record.locked)

    private fun readAsciiLine(input: BufferedInputStream): String? {
        val line = ByteArrayOutputStream()
        while (true) {
            val value = input.read()
            if (value < 0) return if (line.size() == 0) null else line.toString(Charsets.ISO_8859_1.name())
            if (value == '\n'.code) return line.toString(Charsets.ISO_8859_1.name()).trimEnd('\r')
            line.write(value)
        }
    }

    private fun readRequestBody(input: BufferedInputStream, headers: Map<String, String>): ByteArray {
        val length = headers["content-length"]?.toIntOrNull() ?: 0
        val body = ByteArray(length)
        var offset = 0
        while (offset < length) {
            val read = input.read(body, offset, length - offset)
            if (read < 0) return body.copyOf(offset)
            offset += read
        }
        return body
    }

    private fun respond(client: Socket, code: Int, body: ByteArray, extraHeader: String = "") {
        val writer = BufferedWriter(OutputStreamWriter(client.getOutputStream(), Charsets.ISO_8859_1))
        writer.write("HTTP/1.1 $code ${statusText(code)}\r\n")
        writer.write("Connection: close\r\n")
        if (extraHeader.isNotEmpty()) writer.write("$extraHeader\r\n")
        writer.write("Content-Length: ${body.size}\r\n\r\n")
        writer.flush()
        client.getOutputStream().write(body)
        client.getOutputStream().flush()
    }

    private fun statusText(code: Int): String = when (code) {
        200 -> "OK"
        201 -> "Created"
        204 -> "No Content"
        401 -> "Unauthorized"
        404 -> "Not Found"
        405 -> "Method Not Allowed"
        else -> "Error"
    }

    override fun close() {
        socket.close()
        thread?.join(1_000)
    }
}

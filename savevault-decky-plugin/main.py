"""SaveVault Decky Loader backend.

Bridges the Game Mode panel to the SaveVault daemon's local HTTP API, and can
start the daemon itself — in Game Mode the desktop app isn't running, so
without this the panel could only ever report that nothing is available.
"""

import asyncio
import json
import os
import shutil
import subprocess
import urllib.error
import urllib.request

import decky

# aiohttp ships with Decky Loader itself, but don't hard-depend on it: if it
# ever isn't importable the panel must still work, just without live progress.
try:
    import aiohttp

    HAVE_AIOHTTP = True
except Exception:  # pragma: no cover - environment dependent
    HAVE_AIOHTTP = False

# Sync events forwarded from the daemon's dashboard WebSocket to the panel.
SYNC_EVENT_TYPES = ("sync-start", "sync-progress", "sync-complete", "sync-error")
# The Decky event the frontend listens on.
SYNC_EVENT = "savevault_sync"

# Where the daemon publishes the address it actually bound. The configured
# port can be taken, in which case the daemon falls back to an ephemeral one,
# so this file — not the setting — is the source of truth.
ADDR_FILE = os.path.join(decky.DECKY_USER_HOME, ".savevault", "daemon.addr")
LEGACY_ADDR_FILE = os.path.join(decky.DECKY_USER_HOME, ".opensave", "daemon.addr")
TOKEN_FILE = os.path.join(decky.DECKY_USER_HOME, ".savevault", "daemon.token")
FALLBACK_URL = "http://127.0.0.1:8383"


def _daemon_token() -> str:
    """Loopback bearer token published by the running SaveVault daemon."""
    try:
        with open(TOKEN_FILE, "r", encoding="utf-8") as fh:
            return fh.read().strip()
    except OSError:
        return ""

FLATPAK_APP_ID = "io.github.jun9100.SaveVault"

# How long to wait for a freshly started daemon to answer.
DAEMON_START_TIMEOUT = 20.0


def _daemon_url() -> str:
    """Base URL of the running daemon, preferring the address it published."""
    for path in (ADDR_FILE, LEGACY_ADDR_FILE):
        try:
            with open(path, "r", encoding="utf-8") as fh:
                addr = fh.read().strip()
            if addr:
                return f"http://{addr}"
        except OSError:
            continue
    return FALLBACK_URL


class DaemonRefused(Exception):
    """The daemon answered, and said no: its message, and — for a sync —
    the reason it gave ("paused", "held", "offline"), for the panel to act on
    rather than parse the words for."""

    def __init__(self, message: str, reason: str = "", status: int = 0):
        super().__init__(message)
        self.reason = reason
        self.status = status


def _request(path: str, method: str = "GET", body=None, timeout: float = 10.0):
    headers = {"Content-Type": "application/json"}
    token = _daemon_token()
    if token:
        headers["Authorization"] = f"Bearer {token}"
    req = urllib.request.Request(
        _daemon_url() + path,
        method=method,
        data=json.dumps(body).encode() if body is not None else None,
        headers=headers,
    )
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
            return json.loads(raw) if raw else None
    except urllib.error.HTTPError as e:
        message, reason = f"{method} {path} failed ({e.code})", ""
        try:
            payload = json.loads(e.read() or b"{}")
            message = payload.get("error") or message
            reason = payload.get("reason") or ""
        except (ValueError, AttributeError):
            pass
        finally:
            try:
                e.close()
            except Exception:
                pass
        raise DaemonRefused(message, reason, e.code) from None


async def _request_async(path: str, method: str = "GET", body=None, timeout: float = 10.0):
    """Run blocking urllib HTTP calls off Decky's asyncio event loop."""
    return await asyncio.to_thread(_request, path, method, body, timeout)


def reason_of(message: str, reason: str = "") -> str:
    """Why a sync did not happen: the daemon's reason, or — from a daemon
    older than its reasons — the words it used."""
    if reason and reason != "error":
        return reason
    m = (message or "").lower()
    if "paused" in m:
        return "paused"
    if "no online peers" in m:
        return "offline"
    if "deleted here" in m:
        return "held"
    return "error"


# What happened with each device in a sync, by what it means here.
CHANGED = ("updated", "updated_bidirectional", "deletions_synced", "triggered_peer_pull")


def game_outcome(entry) -> str:
    """One game's outcome in a sync-all answer: changed, in-sync, conflict,
    queued, skipped, or why it did not sync (paused, held, offline, error)."""
    if not isinstance(entry, dict):
        return "error"
    status = entry.get("status")
    if isinstance(status, str):
        if status == "error":
            return reason_of(entry.get("error", ""), entry.get("reason", ""))
        if status in ("queued", "skipped"):
            return status
    peers = [p for p in entry.values() if isinstance(p, dict)]
    if any(p.get("status") == "conflict" for p in peers):
        return "conflict"
    if any(p.get("status") in CHANGED for p in peers):
        return "changed"
    if any(p.get("status") == "holding" for p in peers):
        return "held"
    if any(p.get("status") == "paused" for p in peers):
        return "paused"
    if any(p.get("status") == "error" for p in peers):
        err_peer = next(p for p in peers if p.get("status") == "error")
        return reason_of(err_peer.get("error", ""), err_peer.get("reason", ""))
    if any(p.get("status") in ("busy", "awaiting_folder") for p in peers):
        return "queued"
    return "in-sync"


def summarize_sync_all(results: dict) -> dict:
    """A sync of everything, counted and said in one line."""
    counts = {}
    for entry in (results or {}).values():
        kind = game_outcome(entry)
        counts[kind] = counts.get(kind, 0) + 1
    total = sum(counts.values())
    considered = total - counts.get("skipped", 0)
    synced = counts.get("changed", 0) + counts.get("in-sync", 0)

    def n(count, one, many):
        return f"{count} {one if count == 1 else many}"

    if considered and counts.get("paused", 0) == considered:
        message, success = "Syncing is paused — nothing was synced", False
    elif considered and counts.get("offline", 0) + counts.get("paused", 0) == considered:
        message, success = "No other device is online — saves sync when one is", False
    elif not total:
        message, success = "Nothing is tracked yet", True
    elif considered == 0:
        message, success = f"Skipped {n(counts.get('skipped', 0), 'game', 'games')}", True
    else:
        parts = []
        if counts.get("changed"):
            parts.append(n(counts["changed"], "updated", "updated"))
        if counts.get("in-sync"):
            parts.append(n(counts["in-sync"], "already in sync", "already in sync"))
        if counts.get("queued"):
            parts.append(n(counts["queued"], "queued", "queued"))
        message = f"Synced {n(synced, 'game', 'games')}"
        if parts:
            message += ": " + ", ".join(parts)
        extra = []
        if counts.get("conflict"):
            extra.append(n(counts["conflict"], "needs a decision", "need a decision"))
        if counts.get("held"):
            extra.append(n(counts["held"], "held back", "held back"))
        if counts.get("error"):
            extra.append(n(counts["error"], "failed", "failed"))
        if extra:
            message += " — " + ", ".join(extra)
        success = not counts.get("error")
    return {"success": success, "message": message, "counts": counts}


def user_bin_candidates(home: str) -> list:
    """Where savevault-cli may be besides PATH. Decky's service is started with
    a minimal PATH that leaves out ~/.local/bin, which is where install.sh puts
    it — so an install that works in any terminal was invisible from here."""
    dirs = [os.path.join(home, ".local", "bin"), "/usr/local/bin", "/usr/bin"]
    extra = os.environ.get("SAVEVAULT_INSTALL_DIR")
    if extra:
        dirs.insert(0, extra)
    return [os.path.join(d, "savevault-cli") for d in dirs]


def daemon_command(home: str, which=shutil.which, is_file=os.path.isfile):
    """How to run the headless daemon here, or None: a native savevault-cli —
    on PATH, or where install.sh and the usual places put it — else the
    Flatpak, which ships the same binary."""
    native = which("savevault-cli")
    if native:
        return [native, "daemon"]
    for candidate in user_bin_candidates(home):
        if is_file(candidate):
            return [candidate, "daemon"]
    flatpak = which("flatpak") or ("/usr/bin/flatpak" if is_file("/usr/bin/flatpak") else None)
    if flatpak:
        return [flatpak, "run", "--command=savevault-cli", FLATPAK_APP_ID, "daemon"]
    return None


class Plugin:
    # ── Status ───────────────────────────────────────────────────────────

    async def get_daemon_status(self) -> dict:
        """Whether the daemon is reachable, plus its status payload."""
        try:
            return {"running": True, "data": await _request_async("/api/status", timeout=3)}
        except Exception as e:
            return {"running": False, "error": str(e)}

    async def get_daemon_base_url(self) -> str:
        """Base URL of the daemon, so the panel can load cover art from it."""
        return _daemon_url()

    async def get_daemon_token(self) -> str:
        """Loopback auth token for cover image URLs loaded directly by the panel."""
        return _daemon_token()

    async def get_games(self) -> dict:
        try:
            return await _request_async("/api/games", timeout=5) or {}
        except Exception as e:
            decky.logger.warning(f"get_games failed: {e}")
            return {}

    # ── Actions ──────────────────────────────────────────────────────────

    async def sync_all(self) -> dict:
        """Sync every game, and say what happened — which used to be "Sync
        started" whatever came back, including when nothing could sync."""
        try:
            answer = await _request_async("/api/games/sync-all", method="POST", body={}, timeout=120) or {}
            return summarize_sync_all(answer.get("results") or {})
        except Exception as e:
            decky.logger.error(f"sync_all failed: {e}")
            return {"success": False, "message": str(e), "counts": {}}

    async def sync_game(self, game_id: str) -> dict:
        """Sync one game. Used by the panel and by the launch/exit hooks.

        A sync that did not happen carries its reason — "paused", "held",
        "offline" — so the hooks can stay quiet about a PC that is simply
        switched off instead of reporting a failure at every launch."""
        try:
            answer = await _request_async(f"/api/games/{game_id}/sync", method="POST", body={}, timeout=120) or {}
            if answer.get("queued"):
                return {"success": True, "outcome": "queued"}
            outcome = game_outcome(answer.get("results") or {})
            if outcome in ("error", "paused", "held", "offline"):
                return {"success": False, "reason": outcome, "error": f"sync {outcome}"}
            return {"success": True, "outcome": outcome}
        except DaemonRefused as e:
            reason = reason_of(str(e), e.reason)
            if reason == "error":
                decky.logger.error(f"sync_game({game_id}) failed: {e}")
            return {"success": False, "reason": reason, "error": str(e)}
        except Exception as e:
            decky.logger.error(f"sync_game({game_id}) failed: {e}")
            return {"success": False, "reason": "error", "error": str(e)}

    # ── Pause ────────────────────────────────────────────────────────────

    async def pause_sync(self, minutes: int = 0) -> dict:
        """Pause syncing for a number of minutes, or until resumed (0)."""
        body = {"minutes": int(minutes)} if minutes else {"untilRestart": True}
        try:
            return {"success": True, "status": await _request_async("/api/sync/pause", method="POST", body=body)}
        except Exception as e:
            return {"success": False, "error": str(e)}

    async def resume_sync(self) -> dict:
        try:
            return {"success": True, "status": await _request_async("/api/sync/resume", method="POST", body={})}
        except Exception as e:
            return {"success": False, "error": str(e)}

    # ── Things waiting on a decision ─────────────────────────────────────

    async def answer_emptied(self, game_id: str, answer: str) -> dict:
        """Every save file of a game was deleted here, and the game is held
        back until someone says whether that was meant: "restore" puts them
        back, "delete" lets the deletion reach the other devices. Without this
        a held game stayed held until Desktop Mode."""
        if answer not in ("restore", "delete"):
            return {"success": False, "error": f"unknown answer {answer}"}
        try:
            await _request_async(f"/api/games/{game_id}/emptied", method="POST", body={"answer": answer}, timeout=60)
            return {"success": True}
        except Exception as e:
            decky.logger.error(f"answer_emptied({game_id}, {answer}) failed: {e}")
            return {"success": False, "error": str(e)}

    async def resolve_location_conflict(self, game_id: str, peer_id: str, root: str, resolution: str) -> dict:
        """Settle one of a game's extra save folders that diverged. There is
        no keeping both for a folder: that parks a copy on a branch, and
        branches belong to the whole game."""
        if resolution not in ("keep-local", "keep-remote"):
            return {"success": False, "error": f"unknown resolution {resolution}"}
        try:
            await _request_async(
                f"/api/games/{game_id}/resolve-location-conflict",
                method="POST",
                body={"peerId": peer_id, "root": root, "resolution": resolution},
                timeout=30,
            )
            return {"success": True}
        except Exception as e:
            decky.logger.error(f"resolve_location_conflict({game_id}, {root}) failed: {e}")
            return {"success": False, "error": str(e)}

    async def snapshot_game(self, game_id: str, comment: str = "") -> dict:
        try:
            await _request_async(
                f"/api/games/{game_id}/snapshot",
                method="POST",
                body={"comment": comment or "Steam Deck snapshot"},
                timeout=60,
            )
            return {"success": True}
        except Exception as e:
            decky.logger.error(f"snapshot_game({game_id}) failed: {e}")
            return {"success": False, "error": str(e)}

    async def rollback_snapshot(self, game_id: str, snapshot_id: str) -> dict:
        """Restore a historical snapshot of one tracked game from Game Mode."""
        if not snapshot_id:
            return {"success": False, "error": "snapshotId is required"}
        try:
            await _request_async(
                f"/api/games/{game_id}/rollback",
                method="POST",
                body={"snapshotId": snapshot_id},
                timeout=60,
            )
            return {"success": True}
        except Exception as e:
            decky.logger.error(f"rollback_snapshot({game_id}, {snapshot_id}) failed: {e}")
            return {"success": False, "error": str(e)}

    async def switch_branch(self, game_id: str, branch: str) -> dict:
        """Switch a game's active save branch (e.g. main vs mod playthrough)."""
        if not branch:
            return {"success": False, "error": "branch name is required"}
        try:
            await _request_async(
                f"/api/games/{game_id}/branch/switch",
                method="POST",
                body={"name": branch},
                timeout=60,
            )
            return {"success": True}
        except Exception as e:
            decky.logger.error(f"switch_branch({game_id}, {branch}) failed: {e}")
            return {"success": False, "error": str(e)}

    async def get_cloud_offers(self) -> list:
        """Return cloud saves from other devices waiting for a decision."""
        try:
            res = await _request_async("/api/cloud/offers", timeout=5)
            return res if isinstance(res, list) else []
        except Exception as e:
            decky.logger.debug(f"get_cloud_offers failed: {e}")
            return []

    async def check_cloud(self) -> dict:
        """Ask the daemon to poll the configured cloud backend for newer saves."""
        try:
            await _request_async("/api/cloud/check", method="POST", body={}, timeout=60)
            return {"success": True}
        except Exception as e:
            decky.logger.error(f"check_cloud failed: {e}")
            return {"success": False, "error": str(e)}

    async def answer_cloud_offer(self, game_id: str, snapshot_id: str, accept: bool) -> dict:
        """Accept (take cloud save, keeping a local safety copy) or dismiss a cloud offer."""
        action = "accept" if accept else "dismiss"
        try:
            await _request_async(
                f"/api/cloud/offers/{action}",
                method="POST",
                body={"gameId": game_id, "snapshotId": snapshot_id},
                timeout=120,
            )
            return {"success": True}
        except Exception as e:
            decky.logger.error(f"answer_cloud_offer({game_id}, {action}) failed: {e}")
            return {"success": False, "error": str(e)}

    async def scan_new_games(self) -> dict:
        """Discover installed Steam / Proton / emulator saves not yet tracked."""
        try:
            found = await _request_async("/api/presets/scan", timeout=45) or []
            tracked = await self.get_games()
            tracked_paths = {
                os.path.normpath(g.get("savePath") or "")
                for g in tracked.values()
                if isinstance(g, dict) and g.get("savePath")
            }
            untracked = [
                item for item in found
                if isinstance(item, dict)
                and item.get("savePath")
                and os.path.normpath(item["savePath"]) not in tracked_paths
            ]
            # Prefer saves that actually contain files first, newest first.
            untracked.sort(
                key=lambda x: (1 if (x.get("fileCount") or 0) > 0 else 0, x.get("latestMtime") or 0),
                reverse=True,
            )
            return {"success": True, "games": untracked[:12]}
        except Exception as e:
            decky.logger.error(f"scan_new_games failed: {e}")
            return {"success": False, "games": [], "error": str(e)}

    async def track_game(self, name: str, save_path: str, app_id: str = "", save_type: str = "") -> dict:
        """Add a discovered game directly from Game Mode."""
        if not name or not save_path:
            return {"success": False, "error": "name and savePath are required"}
        body = {"name": name, "savePath": save_path}
        if app_id:
            body["appId"] = str(app_id)
        if save_type:
            body["saveType"] = save_type
        try:
            game = await _request_async("/api/games", method="POST", body=body, timeout=30)
            return {"success": True, "game": game}
        except Exception as e:
            decky.logger.error(f"track_game({name}) failed: {e}")
            return {"success": False, "error": str(e)}

    async def resolve_conflict(self, game_id: str, peer_id: str, resolution: str) -> dict:
        """Settle a sync conflict from Game Mode.

        Without this a conflict can only be cleared from Desktop Mode, which on
        a handheld means the game stops syncing until you find a keyboard.
        The daemon applies the resolution in the background — pulling a peer's
        whole save can take minutes — so this returns as soon as it is accepted.
        """
        if resolution not in ("keep-local", "keep-remote", "merge-branch"):
            return {"success": False, "error": f"unknown resolution {resolution}"}
        try:
            await _request_async(
                f"/api/games/{game_id}/resolve-conflict",
                method="POST",
                body={"peerId": peer_id, "resolution": resolution},
                timeout=30,
            )
            return {"success": True}
        except Exception as e:
            decky.logger.error(f"resolve_conflict({game_id}, {resolution}) failed: {e}")
            return {"success": False, "error": str(e)}

    async def find_game_by_appid(self, app_id: str) -> dict:
        """Map a Steam AppID to a tracked game, for the lifecycle hooks."""
        games = await self.get_games()
        for game in games.values():
            if str(game.get("appId") or "") == str(app_id):
                return {"found": True, "game": game}
        return {"found": False}

    # ── Daemon lifecycle ─────────────────────────────────────────────────

    async def start_daemon(self) -> dict:
        """Start a headless daemon.

        Game Mode never runs the desktop app, so the panel needs to be able to
        bring the daemon up itself. Prefers a native savevault-cli on PATH and
        falls back to the Flatpak, which ships the same binary.
        """
        status = await self.get_daemon_status()
        if status.get("running"):
            return {"success": True, "alreadyRunning": True}

        cmd = self._daemon_command()
        if cmd is None:
            return {
                "success": False,
                "error": "SaveVault isn't installed on this device — install the Flatpak from Desktop Mode first.",
            }

        try:
            decky.logger.info(f"starting daemon: {' '.join(cmd)}")
            # The daemon keeps its data under HOME. Strip PyInstaller's
            # LD_LIBRARY_PATH (restoring LD_LIBRARY_PATH_ORIG if present) so
            # host binaries like /usr/bin/flatpak do not fail with symbol mismatches.
            env = dict(os.environ, HOME=decky.DECKY_USER_HOME)
            orig_ld = env.get("LD_LIBRARY_PATH_ORIG")
            if orig_ld:
                env["LD_LIBRARY_PATH"] = orig_ld
            else:
                env.pop("LD_LIBRARY_PATH", None)
            proc = subprocess.Popen(
                cmd,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                stdin=subprocess.DEVNULL,
                env=env,
                start_new_session=True,  # survives the plugin being reloaded
            )
        except Exception as e:
            decky.logger.error(f"could not start daemon: {e}")
            return {"success": False, "error": str(e)}

        # Wait for it to answer rather than reporting success optimistically.
        deadline = asyncio.get_event_loop().time() + DAEMON_START_TIMEOUT
        while asyncio.get_event_loop().time() < deadline:
            await asyncio.sleep(0.5)
            if (await self.get_daemon_status()).get("running"):
                wake = getattr(self, "_wake_events", None)
                if wake is not None:
                    wake.set()
                return {"success": True, "alreadyRunning": False}
            if proc.poll() is not None:
                return {"success": False, "error": f"The sync service exited immediately (code {proc.returncode})."}
        return {"success": False, "error": "The daemon didn't start within 20 seconds."}

    def _daemon_command(self):
        """The best available way to run the headless daemon, or None."""
        return daemon_command(decky.DECKY_USER_HOME)

    # ── Live sync progress ───────────────────────────────────────────────

    async def _pump_sync_events(self):
        """Forward the daemon's sync events to the panel.

        The daemon already broadcasts byte counts and percentages on its
        dashboard WebSocket; without this the panel can only say a sync
        "started". Reconnects for as long as the plugin is loaded, because the
        daemon may not be running yet (or may be restarted) while we watch.
        """
        if not HAVE_AIOHTTP:
            decky.logger.warning("aiohttp unavailable — live sync progress disabled")
            return

        backoff = 2
        while True:
            url = _daemon_url().replace("http://", "ws://", 1) + "/ws"
            headers = {}
            token = _daemon_token()
            if token:
                headers["Authorization"] = f"Bearer {token}"
            try:
                async with aiohttp.ClientSession() as session:
                    async with session.ws_connect(url, headers=headers, heartbeat=30) as ws:
                        decky.logger.info("watching daemon sync events")
                        backoff = 2  # connected; reset the retry delay
                        async for msg in ws:
                            if msg.type != aiohttp.WSMsgType.TEXT:
                                continue
                            try:
                                payload = json.loads(msg.data)
                            except ValueError:
                                continue
                            if payload.get("type") in SYNC_EVENT_TYPES:
                                await decky.emit(
                                    SYNC_EVENT,
                                    payload.get("type"),
                                    payload.get("data") or {},
                                )
            except asyncio.CancelledError:
                raise
            except Exception as e:
                decky.logger.debug(f"sync event stream disconnected: {e}")

            wake = getattr(self, "_wake_events", None)
            if wake is not None:
                wake.clear()
                try:
                    await asyncio.wait_for(wake.wait(), timeout=backoff)
                    backoff = 2
                    continue
                except asyncio.TimeoutError:
                    pass
            else:
                await asyncio.sleep(backoff)
            backoff = min(backoff * 2, 30)  # daemon may simply not be running

    # ── Decky lifecycle ──────────────────────────────────────────────────

    async def _main(self):
        self.loop = asyncio.get_event_loop()
        self._wake_events = asyncio.Event()
        self._events_task = self.loop.create_task(self._pump_sync_events())
        decky.logger.info("SaveVault plugin loaded")

    async def _unload(self):
        # The daemon is deliberately left running: it is a background sync
        # service, not something that should stop when the panel closes. Only
        # our own watcher is torn down.
        task = getattr(self, "_events_task", None)
        if task:
            task.cancel()
            try:
                await task
            except asyncio.CancelledError:
                pass
        decky.logger.info("SaveVault plugin unloaded")

    async def _uninstall(self):
        decky.logger.info("SaveVault plugin uninstalled")

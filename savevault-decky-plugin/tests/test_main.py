"""Tests for the plugin's backend, without Decky Loader or a Deck.

`decky` exists only inside Decky Loader, so a stand-in is installed before
main.py is imported; the daemon is a small HTTP server answering with the
shapes the real one sends (internal/api), pinned by the Go side's
TestDeckyPluginContract.

Run: python -m unittest discover -s savevault-decky-plugin/tests
"""

import asyncio
import json
import logging
import os
import sys
import tempfile
import threading
import types
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

HOME = tempfile.mkdtemp(prefix="savevault-decky-test-")
os.makedirs(os.path.join(HOME, ".savevault"), exist_ok=True)

decky = types.ModuleType("decky")
decky.DECKY_USER_HOME = HOME
decky.logger = logging.getLogger("savevault-decky-test")


async def _emit(*args):
    return None


decky.emit = _emit
sys.modules["decky"] = decky
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

import main  # noqa: E402  (after the stand-in, on purpose)


class StubDaemon:
    """Answers "METHOD /path" with (status, body), and records requests."""

    def __init__(self, answers):
        self.answers = answers
        self.seen = []
        stub = self

        class Handler(BaseHTTPRequestHandler):
            def _answer(self):
                length = int(self.headers.get("Content-Length") or 0)
                body = json.loads(self.rfile.read(length) or b"null") if length else None
                stub.seen.append((self.command, self.path, body))
                status, payload = stub.answers.get(f"{self.command} {self.path}", (404, {"error": "no such route"}))
                raw = json.dumps(payload).encode()
                self.send_response(status)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(raw)))
                self.end_headers()
                self.wfile.write(raw)

            do_GET = do_POST = _answer

            def log_message(self, *args):
                pass

        self.server = HTTPServer(("127.0.0.1", 0), Handler)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        with open(main.ADDR_FILE, "w", encoding="utf-8") as fh:
            fh.write(f"127.0.0.1:{self.server.server_port}")

    def close(self):
        self.server.shutdown()
        self.server.server_close()


def run(coro):
    return asyncio.run(coro)


class SyncAnswers(unittest.TestCase):
    def test_a_game_that_did_not_sync_says_why(self):
        cases = {
            "offline": (409, {"error": "no online peers available", "reason": "offline"}),
            "paused": (409, {"error": "syncing is paused"}),  # a daemon from before reasons
            "held": (409, {"error": "its save files were all deleted here; ...", "reason": "held"}),
            "error": (409, {"error": "disk full", "reason": "error"}),
        }
        for want, answer in cases.items():
            stub = StubDaemon({"POST /api/games/hades/sync": answer})
            try:
                res = run(main.Plugin().sync_game("hades"))
            finally:
                stub.close()
            self.assertFalse(res["success"], want)
            self.assertEqual(res["reason"], want)

    def test_a_game_that_synced_says_what_it_did(self):
        stub = StubDaemon({"POST /api/games/hades/sync": (200, {"results": {"p": {"status": "updated"}}})})
        try:
            res = run(main.Plugin().sync_game("hades"))
        finally:
            stub.close()
        self.assertEqual(res, {"success": True, "outcome": "changed"})

    def test_sync_all_no_longer_says_started_when_nothing_could_sync(self):
        offline = {"status": "error", "error": "no online peers available", "reason": "offline"}
        paused = {"status": "error", "error": "syncing is paused", "reason": "paused"}
        self.assertEqual(
            main.summarize_sync_all({"a": offline, "b": offline})["message"],
            "No other device is online — saves sync when one is",
        )
        self.assertEqual(main.summarize_sync_all({"a": paused})["message"], "Syncing is paused — nothing was synced")
        mixed = main.summarize_sync_all({
            "a": {"p": {"status": "updated"}},
            "b": {"p": {"status": "in_sync"}},
            "c": {"p": {"status": "conflict"}},
            "d": {"status": "skipped", "reason": "autoSync disabled"},
        })
        self.assertTrue(mixed["success"])
        self.assertEqual(mixed["message"], "Synced 2 games: 1 updated, 1 already in sync — 1 needs a decision")
        self.assertFalse(main.summarize_sync_all({"a": {"status": "error", "error": "boom"}})["success"])

    def test_sync_all_end_to_end(self):
        stub = StubDaemon({"POST /api/games/sync-all": (200, {"results": {
            "a": {"status": "error", "error": "no online peers available", "reason": "offline"}}})})
        try:
            res = run(main.Plugin().sync_all())
        finally:
            stub.close()
        self.assertFalse(res["success"])
        self.assertIn("No other device is online", res["message"])


class Decisions(unittest.TestCase):
    def test_pause_resume_and_answers_reach_the_daemon(self):
        stub = StubDaemon({
            "POST /api/sync/pause": (200, {"paused": True}),
            "POST /api/sync/resume": (200, {"paused": False}),
            "POST /api/games/hades/emptied": (200, {}),
            "POST /api/games/hades/resolve-location-conflict": (200, {"accepted": True}),
        })
        try:
            p = main.Plugin()
            self.assertTrue(run(p.pause_sync(60))["success"])
            self.assertTrue(run(p.pause_sync(0))["success"])
            self.assertTrue(run(p.resume_sync())["success"])
            self.assertTrue(run(p.answer_emptied("hades", "restore"))["success"])
            self.assertFalse(run(p.answer_emptied("hades", "shrug"))["success"])
            self.assertTrue(run(p.resolve_location_conflict("hades", "peer-1", "Config", "keep-remote"))["success"])
            self.assertFalse(run(p.resolve_location_conflict("hades", "peer-1", "Config", "merge-branch"))["success"])
        finally:
            stub.close()
        bodies = {(m, path): body for m, path, body in stub.seen}
        self.assertEqual(bodies[("POST", "/api/sync/pause")], {"untilRestart": True})  # the last one
        self.assertEqual(bodies[("POST", "/api/games/hades/emptied")], {"answer": "restore"})
        self.assertEqual(
            bodies[("POST", "/api/games/hades/resolve-location-conflict")],
            {"peerId": "peer-1", "root": "Config", "resolution": "keep-remote"},
        )
        self.assertEqual(sum(1 for m, path, _ in stub.seen if path == "/api/games/hades/emptied"), 1)


class FindingTheDaemon(unittest.TestCase):
    def test_install_sh_location_is_found_without_it_on_path(self):
        home = "/home/deck"
        installed = os.path.join(home, ".local", "bin", "savevault-cli")
        cmd = main.daemon_command(home, which=lambda _: None, is_file=lambda p: p == installed)
        self.assertEqual(cmd, [installed, "daemon"])

    def test_path_first_then_flatpak(self):
        self.assertEqual(
            main.daemon_command("/home/deck", which=lambda n: "/opt/savevault-cli" if n == "savevault-cli" else None,
                                is_file=lambda p: True),
            ["/opt/savevault-cli", "daemon"],
        )
        cmd = main.daemon_command("/home/deck", which=lambda n: "/usr/bin/flatpak" if n == "flatpak" else None,
                                  is_file=lambda p: False)
        self.assertEqual(cmd[:3], ["/usr/bin/flatpak", "run", "--command=savevault-cli"])
        self.assertIsNone(main.daemon_command("/home/deck", which=lambda _: None, is_file=lambda p: False))


class GameModeFullLoop(unittest.TestCase):
    def test_rollback_branch_cloud_offers_and_scan_track(self):
        with open(main.TOKEN_FILE, "w", encoding="utf-8") as fh:
            fh.write("deck-test-token")
        stub = StubDaemon({
            "POST /api/games/hades/rollback": (200, {"id": "snap_1"}),
            "POST /api/games/hades/branch/switch": (200, {"success": True}),
            "GET /api/cloud/offers": (200, [{"gameId": "hades", "gameName": "Hades", "snapshotId": "snap_9", "diverged": True}]),
            "POST /api/cloud/check": (200, {"ok": True}),
            "POST /api/cloud/offers/accept": (200, {"success": True}),
            "POST /api/cloud/offers/dismiss": (200, {"success": True}),
            "GET /api/games": (200, {"hades": {"id": "hades", "savePath": "/saves/hades"}}),
            "GET /api/presets/scan": (200, [
                {"id": "1", "name": "Hades", "savePath": "/saves/hades", "fileCount": 5, "latestMtime": 200},
                {"id": "2", "name": "Celeste", "savePath": "/saves/celeste", "fileCount": 3, "latestMtime": 100},
                {"id": "3", "name": "Empty", "savePath": "/saves/empty", "fileCount": 0, "latestMtime": 300},
            ]),
            "POST /api/games": (201, {"id": "celeste", "name": "Celeste"}),
        })
        try:
            p = main.Plugin()
            self.assertEqual(run(p.get_daemon_token()), "deck-test-token")
            self.assertTrue(run(p.rollback_snapshot("hades", "snap_1"))["success"])
            self.assertTrue(run(p.switch_branch("hades", "mods"))["success"])
            offers = run(p.get_cloud_offers())
            self.assertEqual(len(offers), 1)
            self.assertTrue(run(p.check_cloud())["success"])
            self.assertTrue(run(p.answer_cloud_offer("hades", "snap_9", True))["success"])
            self.assertTrue(run(p.answer_cloud_offer("hades", "snap_9", False))["success"])

            scan = run(p.scan_new_games())
            self.assertTrue(scan["success"])
            # Tracked Hades is excluded; Celeste (with files) sorts before Empty (0 files).
            self.assertEqual([g["name"] for g in scan["games"]], ["Celeste", "Empty"])

            tracked = run(p.track_game("Celeste", "/saves/celeste", "504230", "pc"))
            self.assertTrue(tracked["success"])
        finally:
            stub.close()
            try:
                os.remove(main.TOKEN_FILE)
            except OSError:
                pass

        bodies = {(m, path): body for m, path, body in stub.seen}
        self.assertEqual(bodies[("POST", "/api/games/hades/rollback")], {"snapshotId": "snap_1"})
        self.assertEqual(bodies[("POST", "/api/games/hades/branch/switch")], {"name": "mods"})
        self.assertEqual(bodies[("POST", "/api/cloud/offers/accept")], {"gameId": "hades", "snapshotId": "snap_9"})
        self.assertEqual(bodies[("POST", "/api/cloud/offers/dismiss")], {"gameId": "hades", "snapshotId": "snap_9"})
        self.assertEqual(
            bodies[("POST", "/api/games")],
            {"name": "Celeste", "savePath": "/saves/celeste", "appId": "504230", "saveType": "pc"},
        )


if __name__ == "__main__":
    unittest.main()

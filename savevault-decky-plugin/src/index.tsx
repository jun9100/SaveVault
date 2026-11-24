import {
  ButtonItem,
  DropdownItem,
  PanelSection,
  PanelSectionRow,
  ToggleField,
  staticClasses,
} from "@decky/ui";
import { addEventListener, callable, definePlugin, removeEventListener, toaster } from "@decky/api";
import { useEffect, useRef, useState } from "react";
import { FaSave } from "react-icons/fa";

// ── Backend bridge ─────────────────────────────────────────────────────

interface Game {
  id: string;
  name: string;
  activeBranch: string;
  appId?: string;
  savePath?: string;
  branches?: Record<string, { snapshots?: Snapshot[] }>;
}

interface Snapshot {
  id: string;
  timestamp?: string;
  comment?: string;
}

interface CloudOffer {
  gameId: string;
  gameName: string;
  snapshotId: string;
  deviceName?: string;
  savedAt?: string;
  diverged?: boolean;
}

interface DiscoveredSave {
  id: string;
  name: string;
  savePath: string;
  appId?: string;
  type?: string;
  fileCount?: number;
}

interface SideStats {
  files: number;
  totalBytes: number;
  latestMtimeMs: number;
}

interface Conflict {
  peer: { id: string; name: string };
  localStats?: SideStats;
  remoteStats?: SideStats;
  diffTotal?: number;
}

/** One of a game's extra save folders, diverged on two devices. */
interface LocationConflict extends Conflict {
  gameId: string;
  root: string;
}

/** A game whose save files were all deleted here, held back until answered. */
interface EmptiedSave {
  gameId: string;
  name: string;
  state: "held" | "fetching";
  files: number;
}

interface SyncPause {
  paused: boolean;
  untilRestart?: boolean;
  until?: string;
}

interface DaemonStatus {
  running: boolean;
  data?: {
    settings?: { deviceName?: string };
    gameCount?: number;
    peerCount?: number;
    peersOnline?: number;
    conflicts?: Record<string, Conflict>;
    conflictCount?: number;
    locationConflicts?: LocationConflict[];
    emptied?: EmptiedSave[];
    syncPause?: SyncPause;
  };
  error?: string;
}

/** A sync of one game: what it did, or why it did not happen. */
interface SyncResult {
  success: boolean;
  outcome?: "changed" | "in-sync" | "conflict" | "queued";
  reason?: "paused" | "held" | "offline" | "error";
  error?: string;
}

type Done = { success: boolean; error?: string };

const getDaemonStatus = callable<[], DaemonStatus>("get_daemon_status");
const getGames = callable<[], Record<string, Game>>("get_games");
const syncAll = callable<[], { success: boolean; message: string }>("sync_all");
const syncGame = callable<[game_id: string], SyncResult>("sync_game");
const startDaemon =
  callable<[], { success: boolean; alreadyRunning?: boolean; error?: string }>("start_daemon");
const findGameByAppId =
  callable<[app_id: string], { found: boolean; game?: Game }>("find_game_by_appid");
const snapshotGame = callable<[game_id: string, comment: string], Done>("snapshot_game");
const rollbackSnapshot = callable<[game_id: string, snapshot_id: string], Done>("rollback_snapshot");
const switchBranch = callable<[game_id: string, branch: string], Done>("switch_branch");
const getCloudOffers = callable<[], CloudOffer[]>("get_cloud_offers");
const checkCloud = callable<[], Done>("check_cloud");
const answerCloudOffer = callable<[game_id: string, snapshot_id: string, accept: boolean], Done>("answer_cloud_offer");
const scanNewGames = callable<[], { success: boolean; games: DiscoveredSave[]; error?: string }>("scan_new_games");
const trackGame = callable<[name: string, save_path: string, app_id: string, save_type: string], Done>("track_game");
const resolveConflict = callable<[game_id: string, peer_id: string, resolution: string], Done>("resolve_conflict");
const resolveLocationConflict =
  callable<[game_id: string, peer_id: string, root: string, resolution: string], Done>("resolve_location_conflict");
const answerEmptied = callable<[game_id: string, answer: string], Done>("answer_emptied");
const pauseSync = callable<[minutes: number], Done>("pause_sync");
const resumeSync = callable<[], Done>("resume_sync");

const daemonBaseUrl = callable<[], string>("get_daemon_base_url");
const daemonToken = callable<[], string>("get_daemon_token");

/** Live sync state for one game, driven by the daemon's progress events. */
interface SyncActivity {
  state: "running" | "done" | "error";
  percentage?: number;
  peerName?: string;
  error?: string;
}

/** Newest snapshot across a game's branches, as a human "x ago" string. */
function lastSyncedLabel(game: Game): string {
  let newest = 0;
  for (const branch of Object.values(game.branches ?? {})) {
    for (const snap of branch.snapshots ?? []) {
      const t = Date.parse(snap.timestamp ?? "");
      if (!isNaN(t) && t > newest) newest = t;
    }
  }
  if (!newest) return "no snapshots yet";
  const mins = Math.floor((Date.now() - newest) / 60000);
  if (mins < 1) return "just now";
  if (mins < 60) return `${mins}m ago`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return `${hours}h ago`;
  return `${Math.floor(hours / 24)}d ago`;
}

/** A Switch game's title id, from its save folder in an emulator's NAND
 *  (…/save/<account>/<profile>/<title id>) — the same rule as the app's. */
function switchTitleId(savePath?: string): string {
  const parts = String(savePath ?? "").split(/[\\/]+/).filter(Boolean);
  if (parts.length < 4) return "";
  const id = parts[parts.length - 1];
  if (!/^[0-9a-f]{16}$/i.test(id) || parts[parts.length - 4].toLowerCase() !== "save") return "";
  return id.toUpperCase();
}

/** The cover the daemon serves for a game — by App ID, by name, and for a
 *  Switch game by title id, as the desktop app asks for it. */
function coverUrl(base: string, token: string, game: Game): string {
  if (!base) return "";
  const q = new URLSearchParams();
  if (game.appId) q.set("appId", game.appId);
  if (game.name) q.set("name", game.name);
  const titleId = switchTitleId(game.savePath);
  if (titleId) q.set("titleId", titleId);
  if (token) q.set("token", token);
  return `${base}/api/cover?${q.toString()}`;
}

/** What a sync of one game did, or why it did not, in a few words. */
function syncSaid(res: SyncResult): string {
  if (res.success) {
    switch (res.outcome) {
      case "changed":
        return "Synced";
      case "queued":
        return "Queued behind a sync already running";
      case "conflict":
        return "Changed here and on another device — choose which to keep";
      default:
        return "Already in sync";
    }
  }
  switch (res.reason) {
    case "offline":
      return "No other device is online — it syncs when one is";
    case "paused":
      return "Syncing is paused";
    case "held":
      return "Held back: every save file was deleted here";
    default:
      return `Sync failed: ${res.error ?? "unknown error"}`;
  }
}

function pausedUntil(p?: SyncPause): string {
  if (!p?.paused) return "";
  if (p.untilRestart || !p.until) return "until you resume";
  const t = new Date(p.until);
  return isNaN(t.getTime()) ? "for now" : `until ${t.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}`;
}

// ── Auto-sync preferences ──────────────────────────────────────────────
// Stored in localStorage rather than the daemon: these govern this device's
// Game Mode behaviour, and must be readable before the daemon is reachable.

const AUTO_SYNC_KEY = "savevault:autoSyncOnGameEvents";
const LEGACY_AUTO_SYNC_KEY = "opensave:autoSyncOnGameEvents";
const CLOUD_CHECK_ON_LAUNCH_KEY = "savevault:checkCloudOnLaunch";

function autoSyncEnabled(): boolean {
  return (localStorage.getItem(AUTO_SYNC_KEY) ?? localStorage.getItem(LEGACY_AUTO_SYNC_KEY)) !== "false"; // default on
}
function setAutoSyncEnabled(on: boolean) {
  localStorage.setItem(AUTO_SYNC_KEY, on ? "true" : "false");
}
function cloudCheckOnLaunchEnabled(): boolean {
  return localStorage.getItem(CLOUD_CHECK_ON_LAUNCH_KEY) !== "false";
}
function setCloudCheckOnLaunchEnabled(on: boolean) {
  localStorage.setItem(CLOUD_CHECK_ON_LAUNCH_KEY, on ? "true" : "false");
}

function formatSnapLabel(snap: Snapshot): string {
  const ts = snap.timestamp ? new Date(snap.timestamp) : null;
  const timeStr = ts && !isNaN(ts.getTime())
    ? `${ts.toLocaleDateString([], { month: "2-digit", day: "2-digit" })} ${ts.toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" })}`
    : snap.id;
  return snap.comment ? `${timeStr} · ${snap.comment}` : timeStr;
}

const PAUSE_CHOICES = [
  { data: 60, label: "For an hour" },
  { data: 180, label: "For 3 hours" },
  { data: 0, label: "Until I resume" },
];

// ── Panel ──────────────────────────────────────────────────────────────

function Content() {
  const [status, setStatus] = useState<DaemonStatus | null>(null);
  const [games, setGames] = useState<Record<string, Game>>({});
  const [busy, setBusy] = useState(false);
  const [starting, setStarting] = useState(false);
  const [autoSync, setAutoSync] = useState(autoSyncEnabled());
  const [cloudOnLaunch, setCloudOnLaunch] = useState(cloudCheckOnLaunchEnabled());
  const [cloudOffers, setCloudOffers] = useState<CloudOffer[]>([]);
  const [discovered, setDiscovered] = useState<DiscoveredSave[] | null>(null);
  const [expandedGameId, setExpandedGameId] = useState<string | null>(null);
  const [confirmRollbackKey, setConfirmRollbackKey] = useState<string | null>(null);
  const [activity, setActivity] = useState<Record<string, SyncActivity>>({});
  const [baseUrl, setBaseUrl] = useState<string>("");
  const [token, setToken] = useState<string>("");
  const clearTimers = useRef<Map<string, ReturnType<typeof setTimeout>>>(new Map());
  const wasRunning = useRef<boolean>(false);

  const refresh = async () => {
    try {
      const s = await getDaemonStatus();
      setStatus(s);
      if (s.running && !wasRunning.current) {
        daemonBaseUrl().then(setBaseUrl).catch(() => {});
        daemonToken().then(setToken).catch(() => {});
      }
      wasRunning.current = !!s.running;
      if (s.running) {
        const [g, offers] = await Promise.all([getGames(), getCloudOffers().catch(() => [])]);
        setGames(g);
        setCloudOffers(offers);
      } else {
        setGames({});
        setCloudOffers([]);
      }
    } catch (e) {
      console.error("[SaveVault] refresh failed", e);
    }
  };

  useEffect(() => {
    refresh();
    daemonBaseUrl().then(setBaseUrl).catch(() => {});
    daemonToken().then(setToken).catch(() => {});
    const timer = setInterval(refresh, 15000);
    return () => {
      clearInterval(timer);
      for (const t of clearTimers.current.values()) clearTimeout(t);
      clearTimers.current.clear();
    };
  }, []);

  // Live progress, forwarded from the daemon's WebSocket by the backend.
  useEffect(() => {
    const onSync = (type: string, data: { gameId?: string; data?: any }) => {
      const gameId = data?.gameId;
      if (!gameId) return;
      const ev = data.data ?? {};
      const prior = clearTimers.current.get(gameId);
      if (prior) {
        clearTimeout(prior);
        clearTimers.current.delete(gameId);
      }
      setActivity((prev) => {
        const next = { ...prev };
        if (type === "sync-complete") {
          next[gameId] = { state: "done", peerName: ev.peerName };
        } else if (type === "sync-error") {
          next[gameId] = { state: "error", error: ev.error, peerName: ev.peerName };
        } else {
          next[gameId] = {
            state: "running",
            percentage: ev.percentage,
            peerName: ev.peerName,
          };
        }
        return next;
      });
      if (type === "sync-complete" || type === "sync-error") {
        const expectedState = type === "sync-complete" ? "done" : "error";
        const delay = type === "sync-complete" ? 4000 : 12000;
        const t = setTimeout(() => {
          clearTimers.current.delete(gameId);
          setActivity((p) => {
            if (p[gameId]?.state !== expectedState) return p;
            const c = { ...p };
            delete c[gameId];
            return c;
          });
          if (type === "sync-complete") void refresh();
        }, delay);
        clearTimers.current.set(gameId, t);
      }
    };

    addEventListener<[string, any]>("savevault_sync", onSync);
    return () => removeEventListener<[string, any]>("savevault_sync", onSync);
  }, []);

  /** Runs an action with the panel busy, says how it went, and refreshes. */
  const act = async (run: () => Promise<Done>, said: string, title = "SaveVault") => {
    setBusy(true);
    try {
      const res = await run();
      toaster.toast({ title, body: res.success ? said : res.error ?? "That didn't work" });
      await refresh();
    } catch (e: any) {
      toaster.toast({ title, body: e?.message ?? "That didn't work" });
    } finally {
      setBusy(false);
    }
  };

  const onStartDaemon = async () => {
    setStarting(true);
    try {
      const res = await startDaemon();
      if (res.success) {
        toaster.toast({ title: "SaveVault", body: "Sync service started" });
        daemonBaseUrl().then(setBaseUrl).catch(() => {});
      } else {
        toaster.toast({ title: "SaveVault", body: res.error ?? "Couldn't start the sync service" });
      }
      await refresh();
    } catch (e: any) {
      toaster.toast({ title: "SaveVault", body: e?.message ?? "Couldn't start the sync service" });
    } finally {
      setStarting(false);
    }
  };

  const onSyncAll = async () => {
    setBusy(true);
    try {
      const res = await syncAll();
      toaster.toast({ title: "SaveVault", body: res.message });
      await refresh();
    } catch (e: any) {
      toaster.toast({ title: "SaveVault", body: e?.message ?? "Sync failed" });
    } finally {
      setBusy(false);
    }
  };

  const onSyncOne = async (game: Game) => {
    setBusy(true);
    try {
      toaster.toast({ title: game.name, body: syncSaid(await syncGame(game.id)) });
      await refresh();
    } catch (e: any) {
      toaster.toast({ title: game.name, body: e?.message ?? "Sync failed" });
    } finally {
      setBusy(false);
    }
  };

  const onScanNewGames = async () => {
    setBusy(true);
    try {
      const res = await scanNewGames();
      if (!res.success) {
        toaster.toast({ title: "SaveVault", body: res.error ?? "Scan failed" });
        return;
      }
      setDiscovered(res.games ?? []);
      toaster.toast({
        title: "SaveVault",
        body: res.games.length ? `Found ${res.games.length} untracked game${res.games.length === 1 ? "" : "s"}` : "No untracked games found",
      });
    } catch (e: any) {
      toaster.toast({ title: "SaveVault", body: e?.message ?? "Scan failed" });
    } finally {
      setBusy(false);
    }
  };

  const onTrackDiscovered = async (item: DiscoveredSave) => {
    setBusy(true);
    try {
      const saveType = item.type === "emulator" ? "emulator-generic" : "pc";
      const res = await trackGame(item.name, item.savePath, item.appId ?? "", saveType);
      if (res.success) {
        toaster.toast({ title: item.name, body: "Now tracked by SaveVault" });
        setDiscovered((prev) => (prev ? prev.filter((x) => x.savePath !== item.savePath) : prev));
        await refresh();
      } else {
        toaster.toast({ title: item.name, body: res.error ?? "Could not track game" });
      }
    } catch (e: any) {
      toaster.toast({ title: item.name, body: e?.message ?? "Could not track game" });
    } finally {
      setBusy(false);
    }
  };

  // Initial probe in flight: avoid flashing ONLINE + 0 games before first status reply.
  if (status === null) {
    return (
      <PanelSection title="SaveVault">
        <PanelSectionRow>
          <div style={{ fontSize: "0.9em", opacity: 0.7 }}>Checking sync service…</div>
        </PanelSectionRow>
      </PanelSection>
    );
  }

  // Daemon unreachable: offer to start it rather than showing a dead label.
  if (!status.running) {
    return (
      <PanelSection title="SaveVault">
        <PanelSectionRow>
          <div style={{ fontSize: "0.9em", opacity: 0.8 }}>
            The sync service isn't running, so saves aren't being synced.
          </div>
        </PanelSectionRow>
        <PanelSectionRow>
          <ButtonItem layout="below" onClick={onStartDaemon} disabled={starting}>
            {starting ? "Starting…" : "Start sync service"}
          </ButtonItem>
        </PanelSectionRow>
      </PanelSection>
    );
  }

  const list = Object.values(games);
  const data = status?.data;
  const conflicts = Object.entries(data?.conflicts ?? {});
  const locations = data?.locationConflicts ?? [];
  const emptied = data?.emptied ?? [];
  const heldIds = new Set(emptied.map((e) => e.gameId));
  const pause = data?.syncPause;
  const paused = !!pause?.paused;
  const waiting = conflicts.length + locations.length + emptied.length + cloudOffers.length;
  const nameOf = (id: string) => games[id]?.name ?? id;
  const devices =
    data?.peersOnline !== undefined
      ? `${data.peersOnline} of ${data.peerCount ?? 0} device${(data.peerCount ?? 0) === 1 ? "" : "s"} online`
      : `${data?.peerCount ?? 0} device${(data?.peerCount ?? 0) === 1 ? "" : "s"} paired`;

  const newerSide = (c: Conflict) => {
    const mine = c.localStats?.latestMtimeMs ?? 0;
    const theirs = c.remoteStats?.latestMtimeMs ?? 0;
    return mine === theirs ? "same age" : mine > theirs ? "yours is newer" : "theirs is newer";
  };

  return (
    <PanelSection title="SaveVault">
      <PanelSectionRow>
        <div style={{ display: "flex", justifyContent: "space-between", alignItems: "center" }}>
          <span>{data?.settings?.deviceName ?? "This device"}</span>
          {paused ? (
            <span style={{ color: "#ffc857", fontWeight: "bold" }}>❚❚ PAUSED</span>
          ) : (
            <span style={{ color: "#2eff76", fontWeight: "bold" }}>● ONLINE</span>
          )}
        </div>
      </PanelSectionRow>
      <PanelSectionRow>
        <div style={{ fontSize: "0.85em", opacity: 0.75 }}>
          {list.length} game{list.length === 1 ? "" : "s"} tracked · {devices}
        </div>
      </PanelSectionRow>

      {paused ? (
        <PanelSectionRow>
          <ButtonItem
            layout="below"
            disabled={busy}
            onClick={() => act(resumeSync, "Syncing resumed")}
            description={`Syncing is paused ${pausedUntil(pause)}. Saves are still kept as snapshots here.`}
          >
            Resume syncing
          </ButtonItem>
        </PanelSectionRow>
      ) : (
        <>
          <PanelSectionRow>
            <ButtonItem layout="below" onClick={onSyncAll} disabled={busy || list.length === 0}>
              {busy ? "Syncing…" : "Sync all now"}
            </ButtonItem>
          </PanelSectionRow>
          <PanelSectionRow>
            <ButtonItem
              layout="below"
              onClick={() => act(checkCloud, "Cloud checked for newer saves")}
              disabled={busy}
            >
              Check cloud updates
            </ButtonItem>
          </PanelSectionRow>
          <PanelSectionRow>
            <ButtonItem layout="below" onClick={onScanNewGames} disabled={busy}>
              Scan for new games
            </ButtonItem>
          </PanelSectionRow>
          <PanelSectionRow>
            <DropdownItem
              label="Pause syncing"
              description="Nothing is sent or taken while paused; snapshots are still kept."
              rgOptions={PAUSE_CHOICES}
              selectedOption={null}
              strDefaultLabel="Choose…"
              disabled={busy}
              onChange={(o) => act(() => pauseSync(o.data), "Syncing paused")}
            />
          </PanelSectionRow>
        </>
      )}

      {discovered && discovered.length > 0 && (
        <PanelSection title={`Found games (${discovered.length})`}>
          {discovered.slice(0, 8).map((item) => (
            <PanelSectionRow key={item.savePath}>
              <ButtonItem
                layout="below"
                disabled={busy}
                onClick={() => onTrackDiscovered(item)}
                description={`${item.fileCount ? `${item.fileCount} files · ` : ""}${item.savePath}`}
              >
                + Track {item.name}
              </ButtonItem>
            </PanelSectionRow>
          ))}
          <PanelSectionRow>
            <ButtonItem layout="below" disabled={busy} onClick={() => setDiscovered(null)}>
              Hide scan results
            </ButtonItem>
          </PanelSectionRow>
        </PanelSection>
      )}

      {/* What stops a game syncing until someone decides. Without these the
          only way out was Desktop Mode. */}
      {waiting > 0 && (
        <PanelSection title={`Needs a decision (${waiting})`}>
          {cloudOffers.map((offer) => (
            <div key={`cloud-offer-${offer.gameId}-${offer.snapshotId}`}>
              <PanelSectionRow>
                <div style={{ fontSize: "0.85em" }}>
                  <div style={{ fontWeight: "bold" }}>{offer.gameName || nameOf(offer.gameId)}</div>
                  <div style={{ opacity: 0.75 }}>
                    {offer.diverged
                      ? `Cloud conflict with ${offer.deviceName || "another device"} — choose which save to keep.`
                      : `Newer cloud save from ${offer.deviceName || "another device"} is ready to pull.`}
                  </div>
                </div>
              </PanelSectionRow>
              <PanelSectionRow>
                <ButtonItem
                  layout="below"
                  disabled={busy}
                  onClick={() =>
                    act(
                      () => answerCloudOffer(offer.gameId, offer.snapshotId, true),
                      "Applied cloud save (local safety snapshot kept)",
                      offer.gameName || nameOf(offer.gameId),
                    )
                  }
                >
                  Take cloud save
                </ButtonItem>
              </PanelSectionRow>
              <PanelSectionRow>
                <ButtonItem
                  layout="below"
                  disabled={busy}
                  onClick={() =>
                    act(
                      () => answerCloudOffer(offer.gameId, offer.snapshotId, false),
                      "Kept this device's save",
                      offer.gameName || nameOf(offer.gameId),
                    )
                  }
                >
                  Keep this device's
                </ButtonItem>
              </PanelSectionRow>
            </div>
          ))}
          {emptied.map((e) => (
            <div key={`emptied-${e.gameId}`}>
              <PanelSectionRow>
                <div style={{ fontSize: "0.85em" }}>
                  <div style={{ fontWeight: "bold" }}>{e.name || nameOf(e.gameId)}</div>
                  <div style={{ opacity: 0.75 }}>
                    {e.state === "fetching"
                      ? "Putting its save files back…"
                      : `Every save file was deleted here. Your other devices keep theirs${e.files ? ` (${e.files} file${e.files === 1 ? "" : "s"})` : ""} until you choose.`}
                  </div>
                </div>
              </PanelSectionRow>
              {e.state !== "fetching" && (
                <>
                  <PanelSectionRow>
                    <ButtonItem
                      layout="below"
                      disabled={busy}
                      onClick={() => act(() => answerEmptied(e.gameId, "restore"), "Putting the files back…", e.name)}
                      description="From the newest snapshot that has them, and anything newer from your other devices."
                    >
                      Put them back
                    </ButtonItem>
                  </PanelSectionRow>
                  <PanelSectionRow>
                    <ButtonItem
                      layout="below"
                      disabled={busy}
                      onClick={() => act(() => answerEmptied(e.gameId, "delete"), "Deleting them on your other devices", e.name)}
                      description="Each device keeps a snapshot first."
                    >
                      Delete them on my other devices too
                    </ButtonItem>
                  </PanelSectionRow>
                </>
              )}
            </div>
          ))}

          {conflicts.map(([gameId, c]) => (
            <div key={gameId}>
              <PanelSectionRow>
                <div style={{ fontSize: "0.85em" }}>
                  <div style={{ fontWeight: "bold" }}>{nameOf(gameId)}</div>
                  <div style={{ opacity: 0.75 }}>
                    Both this device and {c.peer?.name ?? "a peer"} changed this save
                    {c.diffTotal ? ` (${c.diffTotal} file${c.diffTotal === 1 ? "" : "s"} differ)` : ""} — {newerSide(c)}.
                  </div>
                </div>
              </PanelSectionRow>
              <PanelSectionRow>
                <ButtonItem
                  layout="below"
                  disabled={busy}
                  onClick={() => act(() => resolveConflict(gameId, c.peer.id, "merge-branch"), "Resolving: keeping both…")}
                  description="Safest: keeps both saves, theirs on a separate branch."
                >
                  Keep both
                </ButtonItem>
              </PanelSectionRow>
              <PanelSectionRow>
                <ButtonItem
                  layout="below"
                  disabled={busy}
                  onClick={() => act(() => resolveConflict(gameId, c.peer.id, "keep-local"), "Resolving: keeping this device's save…")}
                >
                  Keep this device's
                </ButtonItem>
              </PanelSectionRow>
              <PanelSectionRow>
                <ButtonItem
                  layout="below"
                  disabled={busy}
                  onClick={() =>
                    act(() => resolveConflict(gameId, c.peer.id, "keep-remote"), `Resolving: keeping ${c.peer?.name ?? "the peer"}'s save…`)
                  }
                >
                  Keep {c.peer?.name ?? "peer"}'s
                </ButtonItem>
              </PanelSectionRow>
            </div>
          ))}

          {locations.map((c) => (
            <div key={`${c.gameId}/${c.root}`}>
              <PanelSectionRow>
                <div style={{ fontSize: "0.85em" }}>
                  <div style={{ fontWeight: "bold" }}>
                    {nameOf(c.gameId)} · {c.root}
                  </div>
                  <div style={{ opacity: 0.75 }}>
                    This folder changed here and on {c.peer?.name ?? "a peer"} at once — {newerSide(c)}.
                  </div>
                </div>
              </PanelSectionRow>
              <PanelSectionRow>
                <ButtonItem
                  layout="below"
                  disabled={busy}
                  onClick={() =>
                    act(() => resolveLocationConflict(c.gameId, c.peer.id, c.root, "keep-local"), `Keeping this device's ${c.root}…`)
                  }
                >
                  Keep this device's
                </ButtonItem>
              </PanelSectionRow>
              <PanelSectionRow>
                <ButtonItem
                  layout="below"
                  disabled={busy}
                  onClick={() =>
                    act(
                      () => resolveLocationConflict(c.gameId, c.peer.id, c.root, "keep-remote"),
                      `Keeping ${c.peer?.name ?? "the peer"}'s ${c.root}…`,
                    )
                  }
                >
                  Keep {c.peer?.name ?? "peer"}'s
                </ButtonItem>
              </PanelSectionRow>
            </div>
          ))}
        </PanelSection>
      )}

      <PanelSectionRow>
        <ToggleField
          label="Sync around gameplay"
          description="Pull the latest save before a game starts, and push it again when you quit."
          checked={autoSync}
          onChange={(v: boolean) => {
            setAutoSync(v);
            setAutoSyncEnabled(v);
          }}
        />
      </PanelSectionRow>
      <PanelSectionRow>
        <ToggleField
          label="Check cloud on game launch"
          description="Also poll your configured cloud backend when starting a game."
          checked={cloudOnLaunch}
          onChange={(v: boolean) => {
            setCloudOnLaunch(v);
            setCloudCheckOnLaunchEnabled(v);
          }}
        />
      </PanelSectionRow>

      <PanelSection title="Tracked games">
        {list.length === 0 ? (
          <PanelSectionRow>
            <div style={{ opacity: 0.6, fontSize: "0.9em" }}>
              Nothing tracked yet — add games from Desktop Mode.
            </div>
          </PanelSectionRow>
        ) : (
          list.map((game) => {
            const live = activity[game.id];
            const subtitle = heldIds.has(game.id)
              ? "Held back: every save file was deleted here"
              : live?.state === "running"
                ? `Syncing${live.percentage ? ` ${live.percentage}%` : "…"}${live.peerName ? ` with ${live.peerName}` : ""}`
                : live?.state === "done"
                  ? "Synced just now"
                  : live?.state === "error"
                    ? `Sync failed: ${live.error ?? "unknown error"}`
                    : `Last snapshot ${lastSyncedLabel(game)} · branch ${game.activeBranch}`;
            const cover = coverUrl(baseUrl, token, game);
            return (
              <div key={game.id}>
                {cover && (
                  <PanelSectionRow>
                    <img
                      src={cover}
                      alt=""
                      style={{ width: "100%", borderRadius: "4px", display: "block" }}
                      // Not every game has art; drop the element rather than
                      // show a broken image.
                      onError={(e) => ((e.currentTarget as HTMLImageElement).style.display = "none")}
                    />
                  </PanelSectionRow>
                )}
                <PanelSectionRow>
                  <ButtonItem
                    layout="below"
                    onClick={() => onSyncOne(game)}
                    disabled={busy || paused}
                    description={subtitle}
                  >
                    {game.name}
                  </ButtonItem>
                </PanelSectionRow>
                <PanelSectionRow>
                  <ButtonItem
                    layout="below"
                    onClick={() => act(() => snapshotGame(game.id, "Snapshot from Game Mode"), "Snapshot taken", game.name)}
                    disabled={busy}
                    description="Checkpoint the current save before a risky run."
                  >
                    Snapshot
                  </ButtonItem>
                </PanelSectionRow>
                <PanelSectionRow>
                  <ButtonItem
                    layout="below"
                    disabled={busy}
                    onClick={() => {
                      setConfirmRollbackKey(null);
                      setExpandedGameId((prev) => (prev === game.id ? null : game.id));
                    }}
                  >
                    {expandedGameId === game.id ? "Hide history & branches" : "History & branches"}
                  </ButtonItem>
                </PanelSectionRow>
                {expandedGameId === game.id && (() => {
                  const branchNames = Object.keys(game.branches ?? {});
                  const activeSnaps = [...(game.branches?.[game.activeBranch]?.snapshots ?? [])].reverse().slice(0, 5);
                  return (
                    <>
                      {branchNames.length > 1 && (
                        <PanelSectionRow>
                          <DropdownItem
                            label="Active branch"
                            rgOptions={branchNames.map((b) => ({ data: b, label: b }))}
                            selectedOption={game.activeBranch}
                            disabled={busy}
                            onChange={(opt) => {
                              if (opt.data && opt.data !== game.activeBranch) {
                                void act(() => switchBranch(game.id, opt.data), `Switched to branch ${opt.data}`, game.name);
                              }
                            }}
                          />
                        </PanelSectionRow>
                      )}
                      {activeSnaps.length === 0 ? (
                        <PanelSectionRow>
                          <div style={{ fontSize: "0.8em", opacity: 0.7 }}>No snapshots on branch {game.activeBranch}.</div>
                        </PanelSectionRow>
                      ) : (
                        activeSnaps.map((snap) => {
                          const rKey = `${game.id}:${snap.id}`;
                          const confirming = confirmRollbackKey === rKey;
                          return (
                            <PanelSectionRow key={snap.id}>
                              <ButtonItem
                                layout="below"
                                disabled={busy}
                                description={confirming ? "Press again to restore (a safety backup is taken first)" : formatSnapLabel(snap)}
                                onClick={() => {
                                  if (!confirming) {
                                    setConfirmRollbackKey(rKey);
                                    return;
                                  }
                                  setConfirmRollbackKey(null);
                                  void act(() => rollbackSnapshot(game.id, snap.id), "Restored snapshot", game.name);
                                }}
                              >
                                {confirming ? "Confirm rollback?" : "Restore snapshot"}
                              </ButtonItem>
                            </PanelSectionRow>
                          );
                        })
                      )}
                    </>
                  );
                })()}
              </div>
            );
          })
        )}
      </PanelSection>
    </PanelSection>
  );
}

// ── Game lifecycle hooks ───────────────────────────────────────────────
// The point of a Game Mode plugin: sync at the moments saves actually
// matter, so nobody has to remember to open this panel.

/** What a lifecycle sync should say, or "" for nothing. A Deck away from
 *  home has no other device online at every launch; that is not news. */
function lifecycleSaid(res: SyncResult, when: "launch" | "exit"): string {
  if (res.success) {
    if (res.outcome === "changed") return when === "launch" ? "Brought the newest save in" : "Save synced";
    if (res.outcome === "queued") return when === "exit" ? "Save sync queued" : "";
    if (res.outcome === "conflict") return "Changed here and on another device — open SaveVault to choose";
    return when === "exit" ? "Save synced" : "";
  }
  switch (res.reason) {
    case "offline":
      return "";
    case "paused":
      return when === "exit" ? "Syncing is paused — this save goes over when you resume" : "";
    case "held":
      return "Held back: every save file was deleted here — open SaveVault to choose";
    default:
      return `Save sync failed: ${res.error ?? ""}`;
  }
}

function registerLifecycleHooks(): () => void {
  // SteamClient is a global declared by @decky/ui, but Game Mode is the only
  // place it exists — guard so a desktop/dev context degrades quietly.
  const client = (globalThis as any).SteamClient;
  if (!client?.GameSessions?.RegisterForAppLifetimeNotifications) {
    console.warn("[SaveVault] SteamClient.GameSessions unavailable; auto-sync disabled");
    return () => {};
  }

  // Guards against duplicate syncs, while ensuring an exit sync arriving while
  // a slow launch sync is still running is never silently dropped.
  const inFlight = new Set<string>();
  const pendingExit = new Set<string>();

  const syncFor = async (appId: string, when: "launch" | "exit") => {
    if (!autoSyncEnabled()) return;
    if (inFlight.has(appId)) {
      if (when === "exit") pendingExit.add(appId);
      return;
    }
    inFlight.add(appId);
    try {
      // Starting the daemon on demand would delay a game launch, so only act
      // when it's already up.
      if (!(await getDaemonStatus()).running) return;

      const match = await findGameByAppId(appId);
      if (!match.found || !match.game) return;

      if (when === "launch" && cloudCheckOnLaunchEnabled()) {
        await checkCloud().catch(() => {});
      }
      const said = lifecycleSaid(await syncGame(match.game.id), when);
      if (said) toaster.toast({ title: match.game.name, body: said });
    } catch (e) {
      console.error("[SaveVault] lifecycle sync failed", e);
    } finally {
      inFlight.delete(appId);
      if (pendingExit.delete(appId)) {
        void syncFor(appId, "exit");
      }
    }
  };

  const unregister = client.GameSessions.RegisterForAppLifetimeNotifications(
    (update: { unAppID: number; bRunning: boolean }) => {
      const appId = String(update.unAppID);
      // Deliberately fire-and-forget: a game launch must never wait on the
      // network. The sync runs alongside it.
      void syncFor(appId, update.bRunning ? "launch" : "exit");
    },
  );

  return () => {
    try {
      unregister?.unregister?.();
    } catch {
      /* nothing useful to do if Steam already tore it down */
    }
  };
}

export default definePlugin(() => {
  const disposeHooks = registerLifecycleHooks();

  return {
    name: "SaveVault",
    title: <div className={staticClasses.Title}>SaveVault</div>,
    content: <Content />,
    icon: <FaSave />,
    onDismount() {
      disposeHooks();
    },
  };
});

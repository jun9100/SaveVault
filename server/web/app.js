// SPDX-FileCopyrightText: Copyright 2026 Eden Emulator Project
// SPDX-License-Identifier: GPL-3.0-or-later

"use strict";

const $ = (selector, root = document) => root.querySelector(selector);
const main = $("#main");
const narrowTimeline = typeof window !== "undefined" && typeof window.matchMedia === "function"
  ? window.matchMedia("(max-width: 679px)") : null;
const timeZone = (() => {
  try { return Intl.DateTimeFormat().resolvedOptions().timeZone || ""; } catch { return ""; }
})();

const store = {
  overview: null,
  overviewSaveVaultGames: null,
  scopeDetail: null,
  events: null,
  saveVaultFiles: null,
  saveVaultScopes: null,
  saveVaultGames: null,
  saveVaultScopeDetail: null,
  saveVaultRetention: null,
  saveVaultTombstones: null,
  saveVaultRetentionPreview: null,
  metaSettings: null,
  deviceTokens: null,
  pairings: null,
  saveVaultQuery: "",
  saveVaultType: "all",
  savesQuery: "",
  savesSort: "recent",
  savesFilter: "all",
  savesUser: "",
  activityFilter: "all",
  pruneReport: null,
  verifyReport: null,
};

// ---------- utilities ----------

function esc(value) {
  return String(value ?? "").replace(/[&<>"']/g, (c) => ({
    "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;",
  })[c]);
}

function icon(name) {
  return `<svg aria-hidden="true"><use href="#i-${name}"/></svg>`;
}

function formatBytes(value) {
  const units = ["B", "KB", "MB", "GB", "TB"];
  let size = Number(value) || 0;
  let unit = 0;
  while (size >= 1024 && unit < units.length - 1) { size /= 1024; unit++; }
  return unit === 0 ? `${size} B` : `${size.toFixed(size >= 100 ? 0 : 1)} ${units[unit]}`;
}

const dateTime = new Intl.DateTimeFormat("zh-CN", {
  year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit", hour12: false,
});
const shortDate = new Intl.DateTimeFormat("zh-CN", { month: "numeric", day: "numeric" });
const longDay = new Intl.DateTimeFormat("zh-CN", { month: "long", day: "numeric", weekday: "short" });
const clockTime = new Intl.DateTimeFormat("zh-CN", { hour: "2-digit", minute: "2-digit", hour12: false });

function validDate(value) {
  if (!value) return null;
  const date = new Date(value);
  return Number.isNaN(date.getTime()) || date.getFullYear() < 2000 ? null : date;
}

function absTime(value) {
  const date = validDate(value);
  return date ? dateTime.format(date) : "—";
}

function relTime(value) {
  const date = validDate(value);
  if (!date) return "从未";
  const seconds = Math.round((Date.now() - date.getTime()) / 1000);
  if (seconds < 45) return "刚刚";
  const minutes = Math.round(seconds / 60);
  if (minutes < 60) return `${minutes} 分钟前`;
  const hours = Math.round(minutes / 60);
  if (hours < 24) return `${hours} 小时前`;
  const days = Math.round(hours / 24);
  if (days < 30) return `${days} 天前`;
  return dateTime.format(date).slice(0, 10);
}

function duration(seconds) {
  const s = Math.max(0, Math.floor(seconds));
  const d = Math.floor(s / 86400), h = Math.floor((s % 86400) / 3600), m = Math.floor((s % 3600) / 60);
  if (d) return `${d} 天 ${h} 小时`;
  if (h) return `${h} 小时 ${m} 分钟`;
  return `${m} 分钟`;
}

function hue(text) {
  let hash = 0;
  for (const c of String(text)) hash = (hash * 31 + c.codePointAt(0)) >>> 0;
  return hash % 360;
}

function shortID(id, size = 10) {
  id = String(id || "");
  return id.length > size + 2 ? `${id.slice(0, size)}…` : id;
}

function middleEllipsis(value, head = 18, tail = 14) {
  const chars = Array.from(String(value || ""));
  if (chars.length <= head + tail + 1) return chars.join("");
  return `${chars.slice(0, head).join("")}…${chars.slice(-tail).join("")}`;
}

function coverInitials(name) {
  const title = String(name || "").trim();
  if (!title) return "?";
  const first = Array.from(title)[0];
  if (/[\u3400-\u9fff\u3040-\u30ff\uac00-\ud7af]/.test(first)) return first;
  const words = title.split(/[\s\-_:·]+/)
    .map((word) => word.replace(/[^\p{L}\p{N}]/gu, ""))
    .filter(Boolean);
  if (words.length > 1) {
    return `${Array.from(words[0])[0]}${Array.from(words[1])[0]}`.toUpperCase();
  }
  const clean = Array.from((words[0] || title).replace(/[^\p{L}\p{N}]/gu, ""));
  return (clean.slice(0, 2).join("") || "?").toUpperCase();
}

function coverBlock(name, coverURL = "", size = "") {
  const title = String(name || "").trim() || "未命名存档";
  const img = coverURL
    ? `<img src="${esc(coverURL)}" alt="" loading="lazy" referrerpolicy="no-referrer">`
    : "";
  const coverHue = hue(title);
  return `<span class="cover ${size}" style="--hue:${coverHue};--hue-2:${(coverHue + 38) % 360}" aria-hidden="true">${esc(coverInitials(title))}${img}</span>`;
}

function relAbsTime(value) {
  const abs = absTime(value);
  return `<span class="rel-time" title="${esc(abs)}">${esc(relTime(value))}</span>`;
}

function statusChip(kind, text, title = "") {
  return `<span class="status-chip ${kind}"${title ? ` title="${esc(title)}"` : ""}><span class="status-dot"></span>${esc(text)}</span>`;
}

const sharedTokenModes = {
  allow: { label: "允许", description: "共享令牌正常使用，服务端不做任何提示。" },
  warn: { label: "警告", description: "共享令牌仍可使用，但每个响应都会带弃用提示，便于逐步迁移。" },
  deny: { label: "禁用", description: "拒绝共享令牌，客户端必须改用设备令牌。切换前请确认所有设备都已迁移。" },
};

function sharedTokenModeLabel(mode) {
  return sharedTokenModes[mode]?.label || mode || "未知";
}

function countdownClock(seconds) {
  const total = Math.max(0, Math.floor(seconds));
  return `${String(Math.floor(total / 60)).padStart(2, "0")}:${String(total % 60).padStart(2, "0")}`;
}

function secondsUntil(value) {
  const deadline = Date.parse(value || "");
  return Number.isNaN(deadline) ? 0 : Math.max(0, (deadline - Date.now()) / 1000);
}

function normalizePairServer(value) {
  const server = String(value || "").trim().replace(/\/+$/, "");
  if (!/^https?:\/\//i.test(server)) return null;
  try {
    const url = new URL(server);
    return url.protocol === "http:" || url.protocol === "https:" ? server : null;
  } catch {
    return null;
  }
}

function pairURI(server, code) {
  return `savevault://pair?v=1&server=${encodeURIComponent(server)}&code=${encodeURIComponent(code)}`;
}

function pairQRCodeSVG(text) {
  if (typeof qrcode !== "function") return "";
  try {
    const qr = qrcode(0, "M");
    qr.addData(text);
    qr.make();
    const margin = 4;
    const modules = qr.getModuleCount();
    const size = modules + margin * 2;
    let path = "";
    for (let row = 0; row < modules; row++) {
      for (let col = 0; col < modules; col++) {
        if (qr.isDark(row, col)) path += `M${col + margin} ${row + margin}h1v1h-1z`;
      }
    }
    return `<svg class="pair-qr" viewBox="0 0 ${size} ${size}" role="img" aria-label="设备配对二维码" shape-rendering="crispEdges">
      <rect width="${size}" height="${size}" fill="#ffffff"/>
      <path d="${path}" fill="#000000"/>
    </svg>`;
  } catch {
    return "";
  }
}

function closeOnEscape(close) {
  const onKey = (event) => { if (event.key === "Escape") close(); };
  document.addEventListener("keydown", onKey);
  return () => document.removeEventListener("keydown", onKey);
}

function syncTimelineDetails() {
  if (!narrowTimeline) return;
  document.querySelectorAll(".os-snap-tech-details").forEach((details) => {
    details.open = !narrowTimeline.matches;
  });
}
if (narrowTimeline?.addEventListener) narrowTimeline.addEventListener("change", syncTimelineDetails);

function storageGet(key) {
  try { return localStorage.getItem(key); } catch { return null; }
}
function storageSet(key, value) {
  try { value == null ? localStorage.removeItem(key) : localStorage.setItem(key, value); } catch { /* ignore */ }
}

// ---------- lookup helpers ----------

function scopeByID(id) {
  return store.overview?.scopes.find((s) => s.id === id);
}
function saveVaultGameNameForScope(type, gameId) {
  const games = store.saveVaultGames?.length ? store.saveVaultGames : (store.overviewSaveVaultGames || []);
  const game = games.find((item) => (item.scopes || []).some((member) =>
    member.type === type && member.gameId === gameId));
  return game ? gameTitle(game) : "";
}

function gameTitle(game, fallback = "") {
  return game?.meta?.name || game?.name || game?.key || fallback;
}

function gameCoverURL(game) {
  return game?.meta?.cover?.url || "";
}

function gameKeyPath(gameKey, suffix = "") {
  const encoded = String(gameKey || "").split("/").map((part) => encodeURIComponent(part)).join("/");
  return `savevault/games/${encoded}${suffix}`;
}

function gameKeyIsFetchable(gameKey) {
  return /^[^/]+\/steam:\d{1,12}$/.test(String(gameKey || "")) ||
    /^switch\/title:[0-9a-fA-F]{16}$/.test(String(gameKey || ""));
}

function gameNameSourceLabel(source) {
  return { custom: "自定义", table: "名称表", client: "客户端", key: "游戏键" }[source] || source || "未知";
}

function allSaveVaultGames() {
  return store.saveVaultGames?.length ? store.saveVaultGames : (store.overviewSaveVaultGames || []);
}

function saveVaultGameNameForKey(gameKey) {
  const game = allSaveVaultGames().find((item) => item.key === gameKey);
  return game ? gameTitle(game, gameKey) : "";
}

function saveVaultGameKeyForScope(type, gameId) {
  const game = allSaveVaultGames().find((item) => (item.scopes || []).some((member) =>
    member.type === type && member.gameId === gameId));
  return game?.key || "";
}

function saveVaultEventScope(item) {
  const match = /^(?:savevault|opensave):([^/]+)\/(.+)$/.exec(item.scope || "");
  if (!match) return null;
  return {
    type: match[1],
    gameId: match[2],
    gameName: saveVaultGameNameForScope(match[1], match[2]) || match[2],
  };
}

function scopeTitle(id) {
  const saveVault = /^(?:savevault|opensave):([^/]+)\/(.+)$/.exec(id || "");
  if (saveVault) {
    const name = saveVaultGameNameForScope(saveVault[1], saveVault[2]);
    return `SaveVault ${saveVaultTypeLabel(saveVault[1])} · ${name || saveVault[2]}`;
  }
  const scope = scopeByID(id);
  return scope?.title || (id ? `存档 ${shortID(id, 8)}` : "未知存档");
}
function deviceByID(id) {
  return store.overview?.devices.find((d) => d.id === id);
}
function tokenByID(id) {
  return store.deviceTokens?.tokens.find((token) => token.id === id);
}
function deviceName(id) {
  if (!id) return "管理员";
  const device = deviceByID(id);
  return device ? (device.alias || device.name || `设备 ${shortID(id, 6)}`) : `设备 ${shortID(id, 6)}`;
}

function cover(scope, size = "") {
  return coverBlock(scope?.title || "未命名存档", scope?.logo || "", size);
}

function platformClass(platform) {
  const value = String(platform || "").toLowerCase();
  if (value.includes("win")) return ["windows", "WIN"];
  if (value.includes("android")) return ["android", "AND"];
  if (value.includes("mac") || value.includes("darwin")) return ["macos", "MAC"];
  if (value.includes("ios")) return ["ios", "iOS"];
  if (value.includes("linux") || value.includes("bsd")) return ["linux", "LNX"];
  return ["", value ? value.slice(0, 3).toUpperCase() : "?"];
}

function platformTag(platform) {
  const [kind, label] = platformClass(platform);
  return `<span class="platform-tag ${kind}" title="${esc(platform || "未知平台")}">${esc(label)}</span>`;
}

// ---------- API ----------

class UnauthorizedError extends Error {}

async function api(path, { method = "GET", body, raw = false, headers: extraHeaders = {}, upload, contentType } = {}) {
  const headers = { "X-Game-Save-Admin": "1", ...extraHeaders };
  if (upload !== undefined) headers["Content-Type"] = contentType || "application/octet-stream";
  else if (body !== undefined) headers["Content-Type"] = "application/json";
  const response = await fetch(`api/${path}`, {
    method, headers, credentials: "same-origin",
    body: upload !== undefined ? upload : body === undefined ? undefined : JSON.stringify(body),
  });
  if (response.status === 401) throw new UnauthorizedError();
  if (!response.ok) {
    let payload = null;
    try { payload = await response.json(); } catch { /* ignore */ }
    const message = payload?.error || `请求失败（${response.status}）`;
    const error = new Error(translateError(message));
    error.status = response.status;
    error.meta = payload && typeof payload.rev === "number" ? payload : null;
    error.head = payload && typeof payload.head === "string" ? payload.head : "";
    throw error;
  }
  if (raw || response.status === 204) return response;
  return response.json();
}

function translateError(message) {
  const map = {
    "the current snapshot cannot be deleted": "不能删除当前版本",
    "snapshot is already current": "该快照已经是当前版本",
    "too many failed attempts, try again later": "失败次数过多，请稍后再试",
    "invalid administrator token": "管理员令牌不正确",
    "logo must be an http or https URL": "Logo 必须是 http 或 https 地址",
    "verification already running": "完整性校验正在进行中",
    "invalid retention policy": "保留策略无效",
    "a game link must be a key of the scope's own type": "只能关联到同一类型的游戏",
    "invalid game link": "游戏关联无效",
    "SaveVault scope head changed": "当前版本已变化，请刷新后再试",
    "SaveVault archive is missing for this snapshot": "该版本的归档文件已缺失，无法设为当前版本",
    "keep must be between 1 and 1000": "保留数量必须在 1 到 1000 之间",
    "name is too long": "名称过长（最多 80 个字符）",
    "device token limit reached": "设备令牌数量已达上限",
    "too many active pairing codes": "待认领的配对码过多（最多 5 个）",
    "ttl_minutes must be between 1 and 60": "有效期必须在 1 到 60 分钟之间",
    "invalid shared_token_mode": "共享令牌兼容期设置无效",
    "device token not found": "设备令牌不存在",
    "pairing not found": "配对码不存在或已失效",
    "only revoked device tokens can be deleted": "只能删除已吊销的设备令牌",
    "cover fetch is disabled": "上游封面抓取已关闭",
    "cover fetch is set by GAME_SAVE_COVER_FETCH": "抓取开关由环境变量 GAME_SAVE_COVER_FETCH 控制",
    "SaveVault game key has no upstream cover": "这个游戏键没有可抓取的上游封面",
    "invalid SaveVault game key": "游戏键无效",
    "SaveVault scope not found": "SaveVault 范围不存在，可能已被删除",
    "SaveVault snapshot not found": "这个版本不存在，可能已被清理",
    "SaveVault tombstone not found": "找不到删除或合并记录",
    "If-Match must contain the current SaveVault head": "缺少当前版本信息，请刷新后重试",
    "SaveVault scope has merged members; unretire them first": "这个范围里还有已合并的存档位置，请先取消合并再删除",
    "SaveVault scope already exists": "这个范围已经存在，无法恢复",
    "SaveVault restore window has expired": "7 天恢复期已过，无法恢复",
    "only a deleted SaveVault scope can be restored": "只有已删除的范围可以恢复",
    "SaveVault trash directory not found": "回收区里找不到这个范围的数据",
    "SaveVault archive already exists in the live directory": "原目录里已经存在同名归档，无法恢复",
    "a retired scope must be unretired": "已合并的范围请使用“取消合并”",
    "SaveVault retired tombstone not found": "找不到这个范围的合并记录",
    "into must be a valid SaveVault game id": "目标游戏 ID 无效",
    "a scope cannot be merged into itself": "不能把范围合并到自己",
    "a scope can only be merged into the same type": "只能合并到同一类型的范围",
    "SaveVault scope already has a tombstone": "这个范围已被删除或合并",
    "SaveVault scope already has merged members": "这个范围已经有合并进来的存档位置",
    "target SaveVault scope was deleted": "目标范围已被删除或合并",
    "target SaveVault scope has a tombstone": "目标范围已被删除或合并",
    "target SaveVault scope not found": "目标范围不存在",
    "invalid SaveVault snapshot id": "版本 ID 无效",
    "lock note must be at most 120 characters without control characters": "备注最多 120 个字符，不能包含控制字符",
    "invalid SaveVault retention settings": "自动保留设置无效，请检查版本数和天数",
  };
  return map[message] || message;
}

async function guarded(task, success) {
  try {
    const result = await task();
    if (success) toast(success);
    return result;
  } catch (error) {
    if (error instanceof UnauthorizedError) { showLogin(); return undefined; }
    toast(error.message || String(error), true);
    return undefined;
  }
}

async function showGameMetaDialog(gameKey, refreshedMeta = null) {
  const games = store.saveVaultGames?.length ? store.saveVaultGames : (store.overviewSaveVaultGames || []);
  const game = games.find((item) => item.key === gameKey);
  if (!game) {
    toast("游戏已不存在，请刷新后再试", true);
    return;
  }
  if (!store.metaSettings) {
    store.metaSettings = await guarded(() => api("meta-settings")) || { coverFetch: false, lockedByEnv: false };
  }
  const meta = refreshedMeta || game.meta || { rev: 0, name: gameTitle(game), nameSource: "key" };
  const title = meta.name || gameTitle(game);
  const cover = meta.cover;
  const sourceLabel = gameNameSourceLabel(meta.nameSource);
  const canRefetch = Boolean(store.metaSettings?.coverFetch) && gameKeyIsFetchable(gameKey);
  const coverSource = cover?.source === "custom" ? "自定义封面" : cover?.source === "cached" ? "上游缓存封面" : "暂无封面";
  const { root, close } = modal(`
    <h2>编辑游戏信息</h2>
    <div class="meta-editor-head">
      ${coverBlock(title, cover?.url || "", "lg")}
      <div class="meta-editor-copy">
        <strong>${esc(title)}</strong>
        <code title="${esc(gameKey)}">${esc(middleEllipsis(gameKey, 32, 12))}</code>
        <span>当前名称来源：${esc(sourceLabel)} · ${esc(coverSource)} · 修订 ${esc(meta.rev)}</span>
      </div>
    </div>
    <form class="form" id="meta-name-form">
      <label class="field"><span>显示名称</span>
        <input type="text" name="name" maxlength="120" value="${meta.nameSource === "custom" ? esc(title) : ""}" placeholder="${esc(title)}" autocomplete="off">
        <small>自定义名称会同步到所有设备。留空保存表示清除自定义名称；当前来源为“${esc(sourceLabel)}”。</small>
      </label>
      <div class="form-actions"><button class="btn primary" type="submit">保存名称</button></div>
    </form>
    <div class="meta-editor-section">
      <div class="meta-editor-label">封面</div>
      <input class="file-input" id="meta-cover-file" type="file" accept="image/jpeg,image/png">
      <div class="form-actions meta-editor-actions">
        <button class="btn" type="button" id="meta-cover-upload">${icon("download")}上传封面</button>
        ${cover?.source === "custom" ? `<button class="btn danger" type="button" id="meta-cover-clear">${icon("trash")}清除自定义封面</button>` : ""}
        ${canRefetch ? `<button class="btn" type="button" id="meta-cover-refetch">${icon("refresh")}重新抓取封面</button>` : ""}
      </div>
      <small class="hint">仅支持 JPEG / PNG，最大 1 MiB；服务端会校验图片尺寸。自定义封面优先于上游缓存。</small>
    </div>
    <p class="form-error" id="meta-editor-error" role="alert"></p>
    <div class="modal-actions"><button class="btn" type="button" id="meta-editor-close">关闭</button></div>`, { className: "wide" });

  const error = $("#meta-editor-error", root);
  const ifMatch = `"${meta.rev}"`;
  const finish = async (message) => {
    close();
    if (message) toast(message);
    await refresh();
  };
  const handleError = async (caught) => {
    if (caught instanceof UnauthorizedError) { close(); showLogin(); return; }
    if (caught.status === 412 && caught.meta) {
      close();
      toast("已被其他设备修改，已刷新为最新信息", true);
      await showGameMetaDialog(gameKey, caught.meta);
      return;
    }
    error.textContent = caught.message || String(caught);
  };

  $("#meta-editor-close", root).onclick = close;
  $("#meta-name-form", root).onsubmit = async (event) => {
    event.preventDefault();
    const name = new FormData(event.target).get("name").trim();
    error.textContent = "";
    try {
      await api(gameKeyPath(gameKey, "/custom-name"), {
        method: "PUT", body: { name }, headers: { "If-Match": ifMatch },
      });
      await finish("名称已保存");
    } catch (caught) { await handleError(caught); }
  };

  $("#meta-cover-upload", root).onclick = async () => {
    const input = $("#meta-cover-file", root);
    const file = input.files?.[0];
    error.textContent = "";
    if (!file) { error.textContent = "请先选择一张 JPEG 或 PNG 图片。"; return; }
    if (file.size > 1024 * 1024) {
      error.textContent = `图片为 ${formatBytes(file.size)}，超过 1 MiB，请压缩后再上传。`;
      input.value = "";
      return;
    }
    if (file.type !== "image/jpeg" && file.type !== "image/png") {
      error.textContent = "只支持 JPEG 或 PNG 图片。";
      input.value = "";
      return;
    }
    try {
      await api(gameKeyPath(gameKey, "/custom-cover"), {
        method: "PUT", upload: file, contentType: file.type, headers: { "If-Match": ifMatch },
      });
      await finish("自定义封面已上传");
    } catch (caught) { await handleError(caught); }
  };

  const clearCover = $("#meta-cover-clear", root);
  if (clearCover) clearCover.onclick = async () => {
    error.textContent = "";
    try {
      await api(gameKeyPath(gameKey, "/custom-cover"), {
        method: "DELETE", headers: { "If-Match": ifMatch },
      });
      await finish("已清除自定义封面");
    } catch (caught) { await handleError(caught); }
  };

  const refetch = $("#meta-cover-refetch", root);
  if (refetch) refetch.onclick = async () => {
    error.textContent = "";
    refetch.disabled = true;
    try {
      await api(gameKeyPath(gameKey, "/cover/refetch"), { method: "POST" });
      await finish("已开始从上游重新抓取封面");
      setTimeout(() => { if (!$("#app").hidden) refresh({ quiet: true }); }, 1800);
    } catch (caught) {
      refetch.disabled = false;
      await handleError(caught);
    }
  };
}

async function loadOverview() {
  const query = timeZone ? `?tz=${encodeURIComponent(timeZone)}` : "";
  const [overview, games, metaSettings, events] = await Promise.all([
    api(`overview${query}`),
    api("savevault/games").catch((error) => {
      if (error instanceof UnauthorizedError) throw error;
      return null;
    }),
    api("meta-settings").catch((error) => {
      if (error instanceof UnauthorizedError) throw error;
      return null;
    }),
    api("events").catch((error) => {
      if (error instanceof UnauthorizedError) throw error;
      return null;
    }),
  ]);
  store.overview = overview;
  store.overviewSaveVaultGames = games ? (games.games || []) : null;
  store.metaSettings = metaSettings;
  if (events) store.events = events.events || [];
  $("#brand-version").textContent = overview.server.version;
  $("#nav-saves").textContent = overview.stats.scopes || "";
  $("#nav-savevault").textContent = overview.stats.savevault_files || "";
  $("#nav-devices").textContent = overview.stats.devices
    ? `${overview.stats.online_devices}/${overview.stats.devices}` : "";
}

// ---------- UI primitives ----------

function toast(message, error = false) {
  const element = document.createElement("div");
  element.className = `toast${error ? " error" : ""}`;
  element.innerHTML = `<span class="toast-icon">${icon(error ? "alert" : "check")}</span>`;
  const text = document.createElement("span");
  text.textContent = message;
  element.append(text);
  $("#toasts").append(element);
  setTimeout(() => element.remove(), error ? 5000 : 2800);
}

let activeModalClose = null;

function modal(html, { className = "", onClose = null } = {}) {
  if (activeModalClose) activeModalClose();
  const root = $("#modal-root");
  root.innerHTML = `<div class="modal ${className}" role="dialog" aria-modal="true">${html}</div>`;
  root.hidden = false;
  let closed = false;
  const close = () => {
    if (closed) return;
    closed = true;
    if (activeModalClose === close) activeModalClose = null;
    root.hidden = true;
    root.innerHTML = "";
    if (onClose) onClose();
  };
  activeModalClose = close;
  root.onclick = (event) => { if (event.target === root) close(); };
  return { root, close };
}

function confirmDialog({ title, message, confirm = "确认", danger = false }) {
  return new Promise((resolve) => {
    const { root, close } = modal(`
      <h2>${esc(title)}</h2><p>${message}</p>
      <div class="modal-actions">
        <button class="btn" type="button" data-answer="no">取消</button>
        <button class="btn ${danger ? "danger solid" : "primary"}" type="button" data-answer="yes">${esc(confirm)}</button>
      </div>`);
    const finish = (value) => { close(); document.removeEventListener("keydown", onKey); resolve(value); };
    const onKey = (event) => { if (event.key === "Escape") finish(false); };
    document.addEventListener("keydown", onKey);
    root.querySelectorAll("[data-answer]").forEach((button) =>
      button.addEventListener("click", () => finish(button.dataset.answer === "yes")));
    root.onclick = (event) => { if (event.target === root) finish(false); };
    root.querySelector('[data-answer="yes"]').focus();
  });
}

function promptDialog({ title, label, value = "", placeholder = "", hint = "", maxlength = 80 }) {
  return new Promise((resolve) => {
    const { root, close } = modal(`
      <h2>${esc(title)}</h2>
      <form class="form">
        <label class="field"><span>${esc(label)}</span>
          <input type="text" name="value" maxlength="${Number(maxlength) || 80}" value="${esc(value)}" placeholder="${esc(placeholder)}">
          ${hint ? `<small>${esc(hint)}</small>` : ""}
        </label>
        <div class="modal-actions">
          <button class="btn" type="button" data-cancel>取消</button>
          <button class="btn primary" type="submit">保存</button>
        </div>
      </form>`);
    const input = root.querySelector("input");
    const finish = (result) => { close(); resolve(result); };
    root.querySelector("[data-cancel]").onclick = () => finish(null);
    root.querySelector("form").onsubmit = (event) => { event.preventDefault(); finish(input.value.trim()); };
    root.onclick = (event) => { if (event.target === root) finish(null); };
    input.addEventListener("keydown", (event) => { if (event.key === "Escape") finish(null); });
    input.focus();
    input.select();
  });
}

function pageHead(title, subtitle, actions = "") {
  return `<header class="page-head"><div><h1>${esc(title)}</h1>${subtitle ? `<p>${subtitle}</p>` : ""}</div>
    <div class="page-actions">${actions}</div></header>`;
}

function emptyState(title, text, iconName = "") {
  return `<div class="empty">${iconName ? icon(iconName) : ""}<strong>${esc(title)}</strong>${text}</div>`;
}

// ---------- events ----------

function localDayKey(value) {
  const date = validDate(value);
  if (!date) return "";
  return `${date.getFullYear()}-${String(date.getMonth() + 1).padStart(2, "0")}-${String(date.getDate()).padStart(2, "0")}`;
}

function todayUploadSummary(items) {
  const today = localDayKey(new Date());
  const games = new Set();
  const devices = new Set();
  let uploads = 0;
  for (const item of items || []) {
    if (item.kind !== "upload" || localDayKey(item.time) !== today) continue;
    uploads++;
    const saveVault = /^(?:savevault|opensave):([^/]+)\/(.+)$/.exec(item.scope || "");
    const gameKey = saveVault ? saveVaultGameKeyForScope(saveVault[1], saveVault[2]) : "";
    if (gameKey || item.scope) games.add(gameKey || item.scope);
    if (item.device) devices.add(item.device);
  }
  return { uploads, games: games.size, devices: devices.size };
}

const uploadMergeWindow = 30 * 60 * 1000;

// Events arrive newest-first. Consecutive uploads from the same scope and
// device merge only while they stay adjacent and within 30 minutes.
function mergeActivity(items) {
  const groups = [];
  for (const item of items || []) {
    const time = validDate(item.time)?.getTime();
    if (item.kind === "upload" && item.scope && item.device) {
      const last = groups[groups.length - 1];
      const previous = last?.kind === "upload-group" ? last.items[last.items.length - 1] : null;
      const previousTime = previous ? validDate(previous.time)?.getTime() : NaN;
      if (last && last.kind === "upload-group" && last.scope === item.scope && last.device === item.device &&
        Number.isFinite(time) && Number.isFinite(previousTime) && Math.abs(previousTime - time) <= uploadMergeWindow) {
        last.items.push(item);
        continue;
      }
      groups.push({ kind: "upload-group", scope: item.scope, device: item.device, items: [item], time: item.time });
      continue;
    }
    groups.push({ kind: item.kind, item, time: item.time });
  }
  return groups;
}

function uploadGroupTime(group) {
  const newest = group.items[0];
  const oldest = group.items[group.items.length - 1];
  const end = validDate(newest.time);
  const start = validDate(oldest.time);
  if (!start || !end) return "—";
  const sameDay = localDayKey(start) === localDayKey(end);
  return sameDay
    ? `${clockTime.format(start)}–${clockTime.format(end)}`
    : `${shortDate.format(start)} ${clockTime.format(start)}–${shortDate.format(end)} ${clockTime.format(end)}`;
}

function uploadGroupRow(group, { withDate = false } = {}) {
  const newest = group.items[0];
  const info = saveVaultEventScope(newest);
  const gameName = info?.gameName || newest.scope || "未知游戏";
  const device = deviceName(group.device);
  const count = group.items.length;
  const timeLabel = uploadGroupTime(group);
  const tooltip = `${gameName} · ${device} · 上传 ${count} 次 · ${timeLabel}\n点击展开每一条上传`;
  return `<details class="event-group">
    <summary class="event-row event-group-summary" title="${esc(tooltip)}">
      <span class="event-icon upload">${icon("download")}</span>
      <span class="event-copy"><span class="event-text"><b>${esc(gameName)}</b> · ${esc(device)} · 上传 ${count} 次</span>
        <span class="event-detail">点击展开每一条</span></span>
      <span class="event-time">${esc(withDate ? `${absTime(newest.time).slice(0, 10)} ${timeLabel}` : timeLabel)}</span>
      <span class="event-group-chevron">${icon("arrow")}</span>
    </summary>
    <div class="event-group-body">${group.items.map((item) => eventRow(item, { withDate: true })).join("")}</div>
  </details>`;
}

function activityRow(group, options = {}) {
  if (group.kind === "upload-group") {
    return group.items.length > 1 ? uploadGroupRow(group, options) : eventRow(group.items[0], options);
  }
  return eventRow(group.item, options);
}

const eventIcons = {
  upload: "download", conflict: "activity", rollback: "restore", promote: "restore", delete_snapshot: "trash",
  delete_scope: "trash", restore_scope: "restore", retire_scope: "archive", unretire_scope: "restore",
  tombstone_cleared: "trash", snapshot_lock: "lock", snapshot_unlock: "lock", retention: "broom",
  prune: "broom", prune_scope: "broom", device_new: "devices", device_removed: "devices",
  device_paired: "shield", device_token_created: "shield", device_token_revoked: "shield",
  verify: "shield", settings: "tools", savevault_meta: "edit", opensave_meta: "edit",
};

// Token events store "<id> <name>"; the name is optional and may contain spaces.
function tokenEventSubject(item) {
  const detail = String(item.detail || "").trim();
  const space = detail.indexOf(" ");
  const tokenID = space === -1 ? detail : detail.slice(0, space);
  const name = space === -1 ? "" : detail.slice(space + 1).trim();
  if (name) return `<b>${esc(name)}</b>${tokenID ? ` <code>${esc(tokenID)}</code>` : ""}`;
  if (tokenID) return `<code>${esc(tokenID)}</code>`;
  if (item.device) return `<b>${esc(deviceName(item.device))}</b>`;
  return "<b>未知设备令牌</b>";
}

function describeEvent(item) {
  const scope = `<b>${esc(scopeTitle(item.scope))}</b>`;
  const device = `<b>${esc(deviceName(item.device))}</b>`;
  const saveVault = saveVaultEventScope(item);
  const saveVaultArchive = saveVault && item.detail ? `归档 ${item.detail}` : "";
  switch (item.kind) {
    case "upload":
      if (saveVault) {
        return [`${device} 上传了 ${scope}`, `${saveVault.gameName} · ${deviceName(item.device)}`,
          [`快照 ${item.snapshot || "—"}`, saveVaultArchive].filter(Boolean).join(" · ")];
      }
      return [`${device} 上传了 ${scope}`, item.detail || "", `快照 ${item.snapshot || "—"}`];
    case "conflict": return [`${device} 上传 ${scope} 时发生冲突`, "云端与冲突副本均已保留，请在快照历史中确认后设为当前版本",
      [`快照 ${item.snapshot || "—"}`, saveVaultArchive].filter(Boolean).join(" · ")];
    case "rollback": return [`${scope} 已恢复到旧版本`, "设备执行“下载最新云存档”后才会应用到本地", `快照 ${item.snapshot || "—"}${item.detail ? ` · ${item.detail}` : ""}`];
    case "promote": return [`${scope} 的当前版本已切换`, `来源 ${device}`,
      [`新当前版本 ${item.snapshot || "—"}`, saveVaultArchive].filter(Boolean).join(" · ")];
    case "delete_snapshot": return [`删除了 ${scope} 的一个快照`, "快照记录已从云端移除", `快照 ${item.snapshot || "—"}`];
    case "delete_scope": {
      const match = /^(\S+) (\d+) versions, (\d+) bytes$/.exec(item.detail || "");
      if (match) {
        return [`删除了存档范围 <b>${esc(match[1])}</b>`,
          `删除 ${match[2]} 个版本，释放 ${formatBytes(Number(match[3]))}；7 天内可在维护页恢复`,
          `范围 ${item.scope || match[1]}`];
      }
      return [`删除了存档范围 ${item.detail ? `<b>${esc(item.detail)}</b>` : scope}`, "7 天内可在维护页恢复", `范围 ${item.scope || "—"}`];
    }
    case "restore_scope": {
      const match = /^(\S+) (\d+) versions$/.exec(item.detail || "");
      return [`恢复了存档范围 <b>${esc(match?.[1] || saveVaultEventScope(item)?.gameName || item.scope || "未知范围")}</b>`,
        match ? `${match[2]} 个版本已移回原处，设备可以继续同步` : "归档与版本已恢复",
        item.scope ? `范围 ${item.scope}` : ""];
    }
    case "retire_scope": {
      const match = /^(\S+) into (\S+)$/.exec(item.detail || "");
      const target = match?.[2] || "";
      const targetName = saveVaultGameNameForKey(target) || target;
      return [`把 ${match?.[1] ? `<b>${esc(match[1])}</b>` : scope} 合并到了 <b>${esc(targetName)}</b>`,
        "合并后只读，数据保留；仍在跟踪它的设备会被提示移除重复项",
        match ? `目标 ${target}` : item.detail || ""];
    }
    case "unretire_scope": return [`取消了 <b>${esc(saveVaultEventScope(item)?.gameName || item.scope || "未知范围")}</b> 的合并`,
      "范围已恢复可写，下次同步会作为冲突副本上传，不会覆盖云端当前版本", `范围 ${item.scope || "—"}`];
    case "tombstone_cleared": return [`解除了 <b>${esc(saveVaultEventScope(item)?.gameName || item.scope || "未知范围")}</b> 的删除标记`,
      "设备可以重新上传，作为新存档从头开始", `范围 ${item.scope || "—"}`];
    case "snapshot_lock": return [`锁定了 ${scope} 的一个版本`,
      item.detail ? `备注：${item.detail}` : "自动清理和手动清理都不会删除锁定的版本",
      `快照 ${item.snapshot || "—"}`];
    case "snapshot_unlock": return [`解锁了 ${scope} 的一个版本`,
      "这个版本不再受锁定保护，可能被后续清理删除", `快照 ${item.snapshot || "—"}`];
    case "retention": {
      const match = /removed (\d+) versions, (\d+) archives, (\d+) orphan archives, freed (\d+) bytes/.exec(item.detail || "");
      if (match) {
        return [`自动保留清理了 ${match[1]} 个版本、${match[2]} 个归档、${match[3]} 个孤立归档，释放 ${formatBytes(Number(match[4]))}`,
          "当前版本、冲突副本、冲突副本指向的版本和锁定的版本始终保留", item.detail || ""];
      }
      return ["自动保留清理完成", item.detail || "", item.detail || ""];
    }
    case "prune": return [item.scope ? `按保留策略清理了 ${scope}` : "执行了存储清理",
      (item.detail || "").replace("removed", "移除").replace("snapshots", "个快照").replace("objects", "个对象"),
      item.scope ? `范围 ${item.scope}` : ""];
    case "prune_scope": {
      const match = /removed (\d+) snapshots, freed (\d+) bytes(?:, failed (\d+) archives)?/.exec(item.detail || "");
      return [`清理了 ${scope} 的旧版本`, match
        ? `移除 ${match[1]} 个版本，释放 ${formatBytes(Number(match[2]))}${match[3] ? `，${match[3]} 个归档删除失败` : ""}`
        : item.detail, item.detail || ""];
    }
    case "device_new": return [`新设备接入：${device}`, item.detail || "", `设备 ${item.device || "—"}`];
    case "device_removed": return [`移除了设备 <b>${esc(item.detail || shortID(item.device, 8))}</b>`, "", `设备 ${item.device || "—"}`];
    case "device_paired": return [`设备通过配对码接入：${tokenEventSubject(item)}`,
      item.device ? `绑定到设备 ${deviceName(item.device)}` : "配对成功的设备已获得独立令牌", item.detail || ""];
    case "device_token_created": return [`生成了设备令牌 ${tokenEventSubject(item)}`,
      item.device ? `等待或已绑定设备 ${deviceName(item.device)}` : "可在客户端首次连接时绑定设备", item.detail || ""];
    case "device_token_revoked": return [`吊销了设备令牌 ${tokenEventSubject(item)}`,
      item.device ? `设备 ${deviceName(item.device)} 的访问已立即失效` : "该令牌的访问已立即失效", item.detail || ""];
    case "verify": return ["完成完整性校验", (item.detail || "").replace(/checked (\d+) objects, (\d+) problems/, "检查 $1 个对象，发现 $2 个问题"), ""];
    case "savevault_meta":
    case "opensave_meta": {
      const detail = String(item.detail || "");
      const cleared = /^(.*) custom-cover cleared$/.exec(detail);
      const split = detail.lastIndexOf(" ");
      const gameKey = cleared ? cleared[1] : split > 0 ? detail.slice(0, split) : detail;
      const action = cleared ? "cover-cleared" : split > 0 ? detail.slice(split + 1) : "";
      const game = [...(store.saveVaultGames || []), ...(store.overviewSaveVaultGames || [])]
        .find((candidate) => candidate.key === gameKey);
      const name = game ? gameTitle(game, gameKey) : gameKey;
      if (action === "custom-name") return [`修改了 <b>${esc(name)}</b> 的名称`, "新的自定义名称会同步到所有设备", gameKey];
      if (action === "custom-cover") return [`修改了 <b>${esc(name)}</b> 的封面`, "自定义封面会同步到所有设备", gameKey];
      if (action === "cover-cleared") return [`清除了 <b>${esc(name)}</b> 的自定义封面`, "已恢复使用上游缓存封面（如有）", gameKey];
      return [`更新了 <b>${esc(name)}</b> 的游戏信息`, detail, gameKey];
    }
    case "settings": {
      const detail = String(item.detail || "");
      const modeMatch = /^shared_token_mode=(allow|warn|deny)$/.exec(detail.trim());
      if (modeMatch) {
        return [`共享令牌阶段切换为 <b>${esc(sharedTokenModeLabel(modeMatch[1]))}</b>`, "共享令牌兼容期设置已更新", detail];
      }
      const coverMatch = /^cover_fetch=(on|off)$/.exec(detail.trim());
      if (coverMatch) {
        return [coverMatch[1] === "on" ? "上游封面抓取已开启" : "上游封面抓取已关闭",
          coverMatch[1] === "on" ? "服务端会从 Steam、Nintendo 和 tinfoil 缓存封面" : "服务端不会再发起新的上游封面请求", detail];
      }
      if (/^(?:savevault|opensave)_retention=/.test(detail.trim())) {
        return ["更新了 SaveVault 自动保留设置", "新的自动保留规则会在下一次维护任务执行时生效", detail];
      }
      return ["更新了保留策略", detail.replace(/retention: keep (\d+) snapshots, (\d+) days/, "保留 $1 个快照 / $2 天（0 表示不限）"), ""];
    }
    default: return [esc(item.kind), item.detail || "", ""];
  }
}

function eventRow(item, { withDate = false } = {}) {
  const [text, detail, technical] = describeEvent(item);
  const saveVault = /^(?:savevault|opensave):([^/]+)\/(.+)$/.exec(item.scope || "");
  const link = saveVault
    ? `href="#/savevault/${encodeURIComponent(saveVault[1])}/${encodeURIComponent(saveVault[2])}"`
    : item.scope && scopeByID(item.scope) ? `href="#/saves/${encodeURIComponent(item.scope)}"` : "";
  const tag = link ? "a" : "div";
  const plainText = text.replace(/<[^>]*>/g, "");
  const tooltip = [plainText, `时间：${absTime(item.time)}`, item.scope ? `范围 ${item.scope}` : "", technical].filter(Boolean).join(" · ");
  return `<${tag} class="event-row" ${link} title="${esc(tooltip)}">
    <span class="event-icon ${esc(item.kind)}">${icon(eventIcons[item.kind] || "activity")}</span>
    <span class="event-copy"><span class="event-text">${text}</span>${detail ? `<span class="event-detail">${esc(detail)}</span>` : ""}</span>
    <span class="event-time">${withDate ? esc(absTime(item.time)) : esc(relTime(item.time))}</span>
  </${tag}>`;
}

// ---------- views ----------

function renderOverview() {
  const { stats, server, activity, scopes, events, devices } = store.overview;
  const openGames = store.overviewSaveVaultGames || [];
  const openVersions = openGames.reduce((sum, game) =>
    sum + (game.scopes || []).reduce((count, member) => count + (Number(member.snapshotCount) || 0), 0), 0);
  const totalGames = stats.scopes + openGames.length;
  const totalVersions = stats.snapshots + openVersions;
  const totalStored = stats.stored_bytes + stats.savevault_bytes;
  const hasEden = stats.scopes > 0 || stats.snapshots > 0;
  const diskUsed = server.disk_total ? (server.disk_total - server.disk_free) / server.disk_total : 0;
  const meterClass = diskUsed > 0.95 ? "danger" : diskUsed > 0.85 ? "warn" : "";
  const saved = Math.max(0, stats.logical_bytes - stats.stored_bytes);
  const onlineDevices = devices.filter((device) => device.online);
  const recent = recentBackups(scopes, openGames);
  const allEvents = store.events || events || [];
  const today = todayUploadSummary(allEvents);
  const recentGroups = mergeActivity(allEvents).slice(0, 8);
  const versionSub = [
    hasEden ? `Eden ${stats.snapshots} 个快照` : "",
    openVersions ? `SaveVault ${openVersions} 个版本` : "",
  ].filter(Boolean).join(" · ") || "还没有版本记录";
  const gameSub = [
    stats.scopes ? `${stats.scopes} 个 Eden 存档范围` : "",
    openGames.length ? `${openGames.length} 款 SaveVault 游戏${stats.savevault_files ? ` · ${stats.savevault_files} 个归档` : ""}` : "",
    !openGames.length && stats.savevault_files ? `${stats.savevault_files} 个 SaveVault 归档` : "",
  ].filter(Boolean).join(" · ") || "等待第一次存档同步";
  const gameHint = hasEden
    ? `${stats.named_scopes} 个 Eden 范围已识别 · ${Math.max(0, stats.scopes - stats.named_scopes)} 个待命名`
    : "SaveVault 游戏按同步范围自动分组";
  const versionHint = `近 7 天上传 ${stats.uploads_7d} 次${stats.pinned_snapshots ? ` · 固定 ${stats.pinned_snapshots} 个快照` : ""}`;
  const storageHint = [
    `${stats.objects} 个加密对象`,
    saved ? `去重节省 ${formatBytes(saved)}` : "",
    server.disk_total ? `数据卷可用 ${formatBytes(server.disk_free)} / 共 ${formatBytes(server.disk_total)}` : "",
  ].filter(Boolean).join(" · ");
  const storageSub = [
    hasEden || stats.stored_bytes ? `Eden ${formatBytes(stats.stored_bytes)}` : "",
    openGames.length || stats.savevault_bytes ? `SaveVault ${formatBytes(stats.savevault_bytes)}` : "",
  ].filter(Boolean).join(" · ") || "尚无存档数据";

  main.innerHTML = `
    ${pageHead("概览", `存档同步服务已运行 ${esc(duration((Date.now() - new Date(server.started_at)) / 1000))}`,
      `<a class="btn" href="#/maintenance">${icon("tools")}维护</a>`)}
    <p class="overview-today"><span class="today-item"><span class="dot ${today.uploads ? "on" : ""}"></span>${icon("activity")}今天上传 <b>${today.uploads}</b> 次</span><span class="today-sep">·</span><span class="today-item">涉及 <b>${today.games}</b> 款游戏</span><span class="today-sep">·</span><span class="today-item"><b>${today.devices}</b> 台活跃设备</span></p>
    <section class="kpi-grid">
      <div class="card kpi kpi-games" title="${esc(gameHint)}">
        <div class="kpi-top"><span class="label">受保护的游戏</span><span class="kpi-icon">${icon("shield")}</span></div>
        <span class="value">${totalGames}</span>
        <span class="sub">${esc(gameSub)}</span>
      </div>
      <div class="card kpi kpi-versions" title="${esc(versionHint)}">
        <div class="kpi-top"><span class="label">版本总数</span><span class="kpi-icon">${icon("archive")}</span></div>
        <span class="value">${totalVersions}</span>
        <span class="sub">${esc(versionSub)}</span>
      </div>
      <div class="card kpi kpi-storage" title="${esc(storageHint)}">
        <div class="kpi-top"><span class="label">存储占用</span><span class="kpi-icon">${icon("database")}</span></div>
        <span class="value">${formatBytes(totalStored)}</span>
        <span class="sub">${esc(storageSub)}</span>
        ${server.disk_total ? `<div class="meter-wrap" title="数据卷已用 ${(diskUsed * 100).toFixed(1)}%"><div class="meter-meta"><span>数据卷已用</span><b>${(diskUsed * 100).toFixed(1)}%</b></div><div class="meter ${meterClass}"><i style="width:${(diskUsed * 100).toFixed(1)}%"></i></div></div>` : ""}
      </div>
      <div class="card kpi kpi-devices">
        <div class="kpi-top"><span class="label"><span class="dot ${stats.online_devices ? "on" : ""}"></span>在线设备</span><span class="kpi-icon">${icon("devices")}</span></div>
        <span class="value">${stats.online_devices}<small>/ ${stats.devices} 台</small></span>
        <span class="sub">${onlineDevices.length
          ? esc(onlineDevices.slice(0, 3).map((device) => device.alias || device.name || "未命名设备").join("、"))
          : "暂无设备在线"}</span>
      </div>
    </section>
    <section class="card">
      <div class="card-head"><div><h2>最近备份</h2><p>最近同步过的游戏与存档范围</p></div><a href="#/saves">查看全部存档</a></div>
      <div class="card-body">${recent.length
        ? `<div class="recent-grid">${recent.map(recentBackupCard).join("")}</div>`
        : emptyState("还没有备份", "在 Eden 或 SaveVault 客户端完成首次同步后，游戏会出现在这里。", "gamepad")}</div>
    </section>
    <div class="overview-split">
      <section class="card">
        <div class="card-head"><div><h2>近 30 天同步</h2><p>近 7 天上传 ${stats.uploads_7d} 次 · 冲突 ${stats.conflicts_7d} 次</p></div>
          <div class="legend"><span><i style="background:var(--accent)"></i>上传</span><span><i style="background:var(--danger)"></i>冲突</span></div></div>
        <div class="card-body">${activityChart(activity)}</div>
      </section>
      <section class="card">
        <div class="card-head"><h2>最近活动</h2><a href="#/activity">查看全部</a></div>
        <div class="card-body list">${recentGroups.length ? recentGroups.map((group) => activityRow(group)).join("")
          : emptyState("暂无活动", "上传、冲突、恢复等操作会记录在这里。", "activity")}</div>
      </section>
    </div>`;
}

function recentBackups(scopes, games) {
  const items = scopes.map((scope) => ({
    title: scope.title || "未命名存档",
    kind: "Eden",
    device: deviceName(scope.latest_device),
    at: scope.latest_at,
    href: `#/saves/${encodeURIComponent(scope.id)}`,
    cover: cover(scope, "sm"),
    detail: `${scope.snapshot_count} 个快照 · 当前版本 ${formatBytes(scope.head_bytes)}`,
  }));
  for (const game of games) {
    const members = (game.scopes || []).slice().sort((left, right) =>
      new Date(right.head?.receivedAt || right.lastReceivedAt || 0) - new Date(left.head?.receivedAt || left.lastReceivedAt || 0));
    const latest = members[0];
    if (!latest) continue;
    items.push({
      title: gameTitle(game, game.key),
      kind: "SaveVault",
      device: latest.head ? saveVaultSource({ deviceName: latest.head.deviceName, device: latest.head.device }) : "未确认来源",
      at: latest.head?.receivedAt || latest.lastReceivedAt || game.lastReceivedAt,
      href: saveVaultScopeLink(latest),
      cover: coverBlock(gameTitle(game, game.key), gameCoverURL(game), "sm"),
      detail: `${latest.snapshotCount || 0} 个版本 · ${latest.head?.archive || "暂无归档"}`,
    });
  }
  return items.sort((left, right) => new Date(right.at || 0) - new Date(left.at || 0)).slice(0, 6);
}

function recentBackupCard(item) {
  return `<a class="recent-game" href="${item.href}" title="${esc(`${item.title}${item.detail ? ` · ${item.detail}` : ""}`)}">
    ${item.cover}
    <span class="recent-copy"><strong>${esc(item.title)}</strong><span>${esc(item.device)} · ${esc(item.kind)}</span></span>
    <span class="recent-time" title="${esc(absTime(item.at))}">${esc(relTime(item.at))}</span>
  </a>`;
}

function activityChart(days) {
  if (!days.some((day) => day.uploads || day.conflicts)) {
    return `<div class="chart-empty">${icon("activity")}<span><strong>近 30 天还没有同步记录</strong> · 上传和冲突会按天显示在这里</span></div>`;
  }
  const max = Math.max(4, ...days.map((day) => day.uploads + day.conflicts));
  const mid = Math.round(max / 2);
  const formatDay = (raw) => {
    const [yy, mm, dd] = String(raw || "").split("-").map(Number);
    return yy && mm && dd ? shortDate.format(new Date(yy, mm - 1, dd)) : raw;
  };
  const cols = days.map((day, index) => {
    const uploads = Number(day.uploads) || 0;
    const conflicts = Number(day.conflicts) || 0;
    const total = uploads + conflicts;
    const uploadPct = uploads ? Math.max(4, (uploads / max) * 100) : 0;
    const conflictPct = conflicts ? Math.max(4, (conflicts / max) * 100) : 0;
    const side = index < Math.ceil(days.length / 2) ? " tip-right" : " tip-left";
    const detail = total
      ? `上传 ${uploads} 次${conflicts ? ` · 冲突 ${conflicts} 次` : ""}`
      : "无同步记录";
    return `<div class="bar-col${total ? " has-data" : ""}${side}" tabindex="0" aria-label="${esc(`${day.date}：${detail}`)}">
      <div class="bar-tip" role="tooltip"><b>${esc(day.date)}</b><span>${esc(detail)}</span></div>
      <div class="bar-stack">
        ${conflicts ? `<span class="bar-seg conflict" style="height:${conflictPct.toFixed(2)}%"></span>` : ""}
        ${uploads ? `<span class="bar-seg upload" style="height:${uploadPct.toFixed(2)}%"></span>` : ""}
      </div>
    </div>`;
  }).join("");
  const labelIndices = [0, 7, 14, 21, days.length - 1].filter((v, i, arr) => v >= 0 && v < days.length && arr.indexOf(v) === i);
  const axis = labelIndices.map((idx) => `<span>${esc(formatDay(days[idx].date))}</span>`).join("");
  return `<div class="bar-chart" role="group" aria-label="近 30 天上传与冲突次数">
    <div class="bar-chart-plot">
      <div class="bar-chart-grid" aria-hidden="true">
        <div class="bar-chart-row"><span>${max}</span><i></i></div>
        <div class="bar-chart-row"><span>${mid}</span><i></i></div>
        <div class="bar-chart-row"><span>0</span><i></i></div>
      </div>
      <div class="bar-chart-cols">${cols}</div>
    </div>
    <div class="bar-chart-axis" aria-hidden="true">${axis}</div>
  </div>`;
}

function renderSaves() {
  const { scopes } = store.overview;
  const query = store.savesQuery.trim().toLowerCase();
  let items = scopes.filter((scope) => {
    if (store.savesFilter === "unnamed" && scope.named) return false;
    if (store.savesFilter === "pinned" && !scope.pinned_count) return false;
    if (store.savesUser === "-" ? scope.user : store.savesUser && scope.user !== store.savesUser) return false;
    return !query || scope.id.toLowerCase().includes(query) || (scope.title || "").toLowerCase().includes(query) ||
      (scope.user || "").toLowerCase().includes(query);
  });
  const sorters = {
    recent: (a, b) => new Date(b.latest_at) - new Date(a.latest_at),
    name: (a, b) => (a.title || "￿").localeCompare(b.title || "￿", "zh-CN"),
    size: (a, b) => b.stored_bytes - a.stored_bytes,
    count: (a, b) => b.snapshot_count - a.snapshot_count,
  };
  items = items.slice().sort(sorters[store.savesSort]);
  const unnamed = scopes.filter((s) => !s.named).length;
  const users = [...new Set(scopes.map((s) => s.user).filter(Boolean))].sort((a, b) => a.localeCompare(b, "zh-CN"));
  const unlabelled = scopes.some((s) => !s.user);

  main.innerHTML = `
    ${pageHead("存档", `${scopes.length} 个存档范围 · 每个范围对应一个游戏账号存档或设备存档`)}
    <div class="toolbar">
      <label class="search">${icon("search")}<input type="search" id="saves-search" aria-label="搜索存档" placeholder="搜索游戏名称或范围 ID" value="${esc(store.savesQuery)}"><kbd class="search-kbd">/</kbd></label>
      <div class="chips">
        <button class="chip ${store.savesFilter === "all" ? "active" : ""}" data-filter="all">全部</button>
        <button class="chip ${store.savesFilter === "unnamed" ? "active" : ""}" data-filter="unnamed">待命名${unnamed ? ` ${unnamed}` : ""}</button>
        <button class="chip ${store.savesFilter === "pinned" ? "active" : ""}" data-filter="pinned">含固定快照</button>
      </div>
      ${users.length ? `<select id="saves-user" aria-label="用户">
        <option value="">全部用户</option>${users.map((user) => `<option value="${esc(user)}">${esc(user)}</option>`).join("")}
        ${unlabelled ? `<option value="-">未标记用户</option>` : ""}
      </select>` : ""}
      <select id="saves-sort" aria-label="排序">
        <option value="recent">最近同步</option><option value="name">名称</option>
        <option value="size">占用空间</option><option value="count">快照数量</option>
      </select>
    </div>
    <section class="saves" id="saves-grid">${items.length ? items.map(saveCard).join("")
      : `<div class="card grid-empty">${emptyState(scopes.length ? "没有匹配的存档" : "还没有存档",
        scopes.length ? "尝试其他关键词或筛选条件。" : "在 Eden 中启用存档同步并关闭一次游戏后，存档会出现在这里。", "gamepad")}</div>`}</section>`;

  $("#saves-sort").value = store.savesSort;
  const userSelect = $("#saves-user");
  if (userSelect) {
    userSelect.value = store.savesUser;
    userSelect.onchange = (event) => { store.savesUser = event.target.value; renderSaves(); };
  }
  $("#saves-sort").onchange = (event) => { store.savesSort = event.target.value; renderSaves(); };
  const search = $("#saves-search");
  search.oninput = () => {
    store.savesQuery = search.value;
    const position = search.selectionStart;
    renderSaves();
    const next = $("#saves-search");
    next.focus();
    next.setSelectionRange(position, position);
  };
  main.querySelectorAll("[data-filter]").forEach((chip) => {
    chip.onclick = () => { store.savesFilter = chip.dataset.filter; renderSaves(); };
  });
}

function saveCard(scope) {
  const title = scope.title || "未命名存档";
  return `<a class="card save" style="--card-hue:${hue(title)}" href="#/saves/${encodeURIComponent(scope.id)}">
    <div class="save-top">${cover(scope)}
      <div class="save-title"><strong title="${esc(title)}">${esc(title)}</strong>
        ${scope.named ? "" : statusChip("warn", "待命名")}
        ${scope.user ? `<span class="save-user">${icon("user")}${esc(scope.user)}</span>` : ""}
        <code title="${esc(scope.id)}">${esc(scope.id)}</code></div></div>
    <div class="save-stats">
      <div><span>最近同步</span><b>${relAbsTime(scope.latest_at)}</b></div>
      <div><span>快照</span><b>${scope.snapshot_count}${scope.pinned_count ? ` · ${icon("pin")}${scope.pinned_count}` : ""}</b></div>
      <div><span>占用</span><b>${formatBytes(scope.stored_bytes)}</b></div>
    </div></a>`;
}

function saveVaultTypeLabel(type) {
  if (type === "pc") return "PC 游戏";
  if (type === "switch") return "Switch";
  if (String(type).startsWith("emulator-")) return `模拟器 · ${String(type).slice("emulator-".length)}`;
  return type || "未分类";
}

function saveVaultSource(file) {
  if (file.deviceName) return file.deviceName;
  if (file.device) return deviceName(file.device);
  return "旧归档（无来源记录）";
}

function saveVaultScopeSource(scope) {
  if (scope.headDeviceName) return scope.headDeviceName;
  if (scope.headDevice) return deviceName(scope.headDevice);
  return "未确认来源";
}

function saveVaultScopeLink(scope) {
  return `#/savevault/${encodeURIComponent(scope.type)}/${encodeURIComponent(scope.gameId)}`;
}

function saveVaultScopeBytes(type, gameId) {
  const scope = (store.saveVaultScopes || []).find((item) => item.type === type && item.gameId === gameId);
  return Number(scope?.archiveBytes) || 0;
}

const saveVaultGameLinkLabel = { manual: "手动关联", auto: "自动识别", none: "未识别" };

function saveVaultLinkStatus(members) {
  if (members.some((member) => member.link === "auto")) return ["ok", "自动识别"];
  if (members.some((member) => member.link === "manual")) return ["accent", "手动关联"];
  return ["warn", "未识别"];
}

function saveVaultGameDevices(members) {
  const seen = new Set();
  const devices = [];
  for (const member of members) {
    const head = member.head;
    if (!head) continue;
    const name = saveVaultSource({ deviceName: head.deviceName, device: head.device });
    const key = `${name}\u0000${head.platform || ""}`;
    if (seen.has(key)) continue;
    seen.add(key);
    devices.push({ name, platform: head.platform || "" });
  }
  return devices;
}

function saveVaultGameCard(game) {
  const members = game.scopes || [];
  const activeMembers = members.filter((member) => !member.retired);
  const retiredMembers = members.filter((member) => member.retired);
  const first = activeMembers[0] || members[0];
  const name = gameTitle(game, "未命名游戏");
  const conflicts = activeMembers.reduce((sum, member) => sum + (Number(member.conflictCount) || 0), 0);
  const versions = activeMembers.reduce((sum, member) => sum + (Number(member.snapshotCount) || 0), 0);
  const bytes = activeMembers.reduce((sum, member) => sum + saveVaultScopeBytes(member.type, member.gameId), 0);
  const [linkKind, linkText] = activeMembers.length ? saveVaultLinkStatus(activeMembers) : ["neutral", "已合并（只读）"];
  const devices = saveVaultGameDevices(activeMembers);
  const latestAt = activeMembers.reduce((latest, member) => {
    const value = member.head?.receivedAt || member.lastReceivedAt;
    return value && (!latest || new Date(value) > new Date(latest)) ? value : latest;
  }, "");
  const memberDescription = (member) => {
    const received = member.head?.receivedAt || member.lastReceivedAt;
    const source = member.head ? saveVaultSource({ deviceName: member.head.deviceName, device: member.head.device }) : "未确认来源";
    const memberBytes = formatBytes(saveVaultScopeBytes(member.type, member.gameId));
    if (member.retired) return `已合并（只读） · ${source} · ${relTime(received)} · ${member.snapshotCount || 0} 个版本`;
    const memberLink = saveVaultGameLinkLabel[member.link] || member.link;
    return `${source} · ${relTime(received)} · ${member.snapshotCount || 0} 个版本 · ${memberBytes} · ${memberLink}`;
  };
  return `<article class="card game-card" style="--card-hue:${hue(name)}">
    <a class="game-card-hit" href="${first ? saveVaultScopeLink(first) : "#/savevault"}" aria-label="打开 ${esc(name)}"></a>
    <div class="game-card-head">
      ${coverBlock(name, gameCoverURL(game), "game-cover")}
      <div class="game-card-copy">
        <span class="game-card-name" title="${esc(name)}">${esc(name)}</span>
        <div class="game-card-tags">
          <span class="tag">${esc(saveVaultTypeLabel(game.type))}</span>
          ${statusChip(linkKind, linkText)}
          ${retiredMembers.length ? `<span class="tag">已合并 ${retiredMembers.length}</span>` : ""}
        </div>
      </div>
      <div class="game-card-tools">
        <button class="icon-btn game-card-edit" type="button" data-action="edit-game-meta" data-key="${esc(game.key)}" title="编辑名称和封面" aria-label="编辑 ${esc(name)}">${icon("edit")}</button>
        <span class="game-card-arrow">${icon("arrow")}</span>
      </div>
    </div>
    <div class="game-devices">${devices.length ? devices.map((device) =>
      `<span class="game-device"><span title="${esc(device.name)}">${esc(device.name)}</span>${platformTag(device.platform)}</span>`).join("")
      : `<span class="game-device-more">${retiredMembers.length ? "只剩已合并的存档位置" : "尚无设备来源"}</span>`}</div>
    ${conflicts ? `<div class="game-conflict">${icon("activity")}<span>${conflicts} 个冲突副本需要确认</span></div>` : ""}
    ${retiredMembers.length ? `<div class="game-merged">${icon("archive")}<span>已合并（只读）：${retiredMembers.map((member) =>
      `<b title="${esc(member.name || member.gameId)}">${esc(member.name || member.gameId)}</b>`).join("、")}</span></div>` : ""}
    <div class="game-card-stats">
      <div><span>最近备份</span><b>${activeMembers.length ? relAbsTime(latestAt) : "—"}</b></div>
      <div><span>版本</span><b>${versions}</b></div>
      <div><span>占用</span><b>${formatBytes(bytes)}</b></div>
    </div>
    <div class="game-card-foot">
      <code title="${esc(game.key || "")}">${esc(middleEllipsis(game.key || "", 26, 10))}</code>
      <span>${activeMembers.length} 个存档位置${retiredMembers.length ? ` · ${retiredMembers.length} 个已合并` : ""}</span>
    </div>
    ${members.length > 1 ? `<div class="game-members"><span class="game-members-label">存档位置</span>
      ${members.map((member) => `<a class="game-member${member.retired ? " retired" : ""}" href="${saveVaultScopeLink(member)}">
          <strong title="${esc(member.name || member.gameId)}">${esc(member.name || member.gameId)}${member.retired ? ` <span class="tag">已合并（只读）</span>` : ""}</strong>
          <small title="${esc(memberDescription(member))}">${esc(memberDescription(member))}</small>
          <code title="${esc(member.gameId)}">${esc(middleEllipsis(member.gameId, 18, 8))}</code>
        </a>`).join("")}
    </div>` : ""}
  </article>`;
}

function archiveFileGameID(file) {
  const base = String(file.name || "").replace(/\.zip$/i, "");
  return base.split("__")[0] || "";
}

function archiveFileGroup(file) {
  const gameId = archiveFileGameID(file);
  const game = (store.saveVaultGames || []).find((item) => item.type === file.type &&
    (item.scopes || []).some((member) => member.gameId === gameId));
  return {
    key: game?.key || `${file.type || "other"}/${gameId || "other"}`,
    name: game ? gameTitle(game, gameId) : gameId || "其他归档",
    cover: gameCoverURL(game),
    type: game?.type || file.type,
  };
}

function saveVaultArchivePanel(files, query) {
  const groups = new Map();
  for (const file of files) {
    const group = archiveFileGroup(file);
    if (!groups.has(group.key)) groups.set(group.key, { ...group, files: [] });
    groups.get(group.key).files.push(file);
  }
  return `<details class="card archive-panel" ${query ? "open" : ""}>
    <summary>
      <span class="archive-summary-icon">${icon("archive")}</span>
      <span class="archive-summary-copy"><strong>高级 · 归档文件</strong>
        <span>${files.length} 个不可变归档 · 按游戏分组，完整文件名与 SHA-256 在悬停提示中</span></span>
      <span class="archive-chevron">${icon("arrow")}</span>
    </summary>
    <div class="archive-body">${files.length ? [...groups.values()].map((group) => `
      <section class="archive-group">
        <div class="archive-group-head">
          ${coverBlock(group.name, group.cover || "", "sm")}
          <div class="archive-group-copy"><strong title="${esc(group.name)}">${esc(group.name)}</strong>
            ${group.type ? `<span class="tag">${esc(saveVaultTypeLabel(group.type))}</span>` : ""}</div>
          <span class="archive-group-count">${group.files.length} 个归档</span>
        </div>
        <div class="archive-list">${group.files.map(saveVaultRow).join("")}</div>
      </section>`).join("")
      : emptyState("没有匹配的归档", "调整关键词或类型筛选后再试。", "archive")}</div>
  </details>`;
}

function saveVaultRow(file) {
  const timestamp = file.uploadedAt || file.createdTime;
  const hash = file.sha256 || "未记录";
  const tooltip = `文件名：${file.name}\nSHA-256：${hash}\n上传时间：${absTime(timestamp)}`;
  return `<div class="archive-row" title="${esc(tooltip)}">
    <span class="archive-icon">${icon("archive")}</span>
    <div class="archive-main">
      <div class="archive-line"><strong>${esc(saveVaultSource(file))}</strong><span class="sep">·</span>
        ${relAbsTime(timestamp)}<span class="sep">·</span><span>${formatBytes(file.sizeBytes)}</span></div>
      <span class="archive-name mono" title="${esc(tooltip)}">${esc(middleEllipsis(file.name, 24, 16))}</span>
    </div>
    <span class="tag accent archive-type">${esc(saveVaultTypeLabel(file.type))}</span>
    ${file.type ? saveVaultDownloadLink(file.type, file.name) : ""}
  </div>`;
}

async function renderSaveVault() {
  if (!store.saveVaultFiles || !store.saveVaultScopes || !store.saveVaultGames) {
    main.innerHTML = `<div class="skeleton"></div>`;
    const [files, scopes, games] = await Promise.all([api("savevault/files"), api("savevault/scopes"), api("savevault/games")]);
    store.saveVaultFiles = files.files || [];
    store.saveVaultScopes = scopes.scopes || [];
    store.saveVaultGames = games.games || [];
    if (currentRoute().name !== "savevault") return;
  }
  const files = store.saveVaultFiles;
  const scopes = store.saveVaultScopes;
  const games = store.saveVaultGames;
  const types = [...new Set([...files, ...games].map((item) => item.type).filter(Boolean))].sort((a, b) => a.localeCompare(b));
  if (store.saveVaultType !== "all" && !types.includes(store.saveVaultType)) store.saveVaultType = "all";
  const query = store.saveVaultQuery.trim().toLowerCase();
  const visible = files.filter((file) => {
    if (store.saveVaultType !== "all" && file.type !== store.saveVaultType) return false;
    if (!query) return true;
    return [file.name, saveVaultSource(file), archiveFileGroup(file).name].join(" ").toLowerCase().includes(query);
  });
  visible.sort((left, right) => new Date(right.uploadedAt || right.createdTime) - new Date(left.uploadedAt || left.createdTime));
  const visibleGames = games.filter((game) => {
    if (store.saveVaultType !== "all" && game.type !== store.saveVaultType) return false;
    const searchable = [gameTitle(game), game.name, game.key, ...(game.scopes || []).flatMap((member) =>
      [member.name, member.gameId, member.head?.deviceName, member.head?.device])].join(" ").toLowerCase();
    return !query || searchable.includes(query);
  });

  main.innerHTML = `
    ${pageHead("SaveVault 存档", `${games.length} 款游戏 · ${scopes.length} 个同步范围 · ${files.length} 个不可变归档`,
      `<div class="head-tools">
        <label class="search">${icon("search")}<input type="search" id="savevault-search" aria-label="搜索游戏、范围或归档" placeholder="搜索游戏、范围或设备" value="${esc(store.saveVaultQuery)}"><kbd class="search-kbd">/</kbd></label>
      </div>`)}
    ${types.length > 1 ? `<div class="toolbar"><div class="chips">
      <button class="chip ${store.saveVaultType === "all" ? "active" : ""}" type="button" data-sv-type="all">全部 ${games.length}</button>
      ${types.map((type) => {
        const count = games.filter((game) => game.type === type).length;
        return `<button class="chip ${store.saveVaultType === type ? "active" : ""}" type="button" data-sv-type="${esc(type)}">${esc(saveVaultTypeLabel(type))}${count ? ` ${count}` : ""}</button>`;
      }).join("")}
    </div></div>` : ""}
    <section class="game-grid">
      ${visibleGames.length ? visibleGames.map(saveVaultGameCard).join("")
        : `<div class="card grid-empty">${emptyState(games.length ? "没有匹配的游戏" : "还没有同步范围",
          games.length ? "尝试其他关键词或类型筛选。" : "客户端完成首次同步后会显示在这里。", "gamepad")}</div>`}
    </section>
    ${saveVaultArchivePanel(visible, query)}`;

  main.querySelectorAll("[data-sv-type]").forEach((chip) => {
    chip.onclick = () => { store.saveVaultType = chip.dataset.svType; renderSaveVault(); };
  });
  const search = $("#savevault-search");
  search.oninput = () => {
    store.saveVaultQuery = search.value;
    const position = search.selectionStart;
    renderSaveVault();
    const next = $("#savevault-search");
    next.focus();
    next.setSelectionRange(position, position);
  };
}

function saveVaultDownloadLink(type, archive) {
  return `<a class="icon-btn" href="api/savevault/files/${encodeURIComponent(type)}/${encodeURIComponent(archive)}/download" title="下载归档（ZIP）" aria-label="下载归档 ${esc(archive)}" download>${icon("download")}</a>`;
}

function saveVaultArchiveSize(archive, type = "") {
  const file = (store.saveVaultFiles || []).find((item) => item.name === archive && (!type || item.type === type));
  return file ? formatBytes(file.sizeBytes) : "大小未知";
}

function saveVaultSnapshotRow(snapshot, headID, type) {
  const timestamp = snapshot.receivedAt || snapshot.createdAt;
  const source = snapshot.deviceName || (snapshot.device ? deviceName(snapshot.device) : "未确认来源");
  const current = snapshot.id === headID && !snapshot.conflict;
  const state = snapshot.conflict ? statusChip("warn", "冲突副本", "与云端当前版本冲突，已保留")
    : current ? statusChip("accent", "当前") : statusChip("neutral", "历史版本");
  const tooltip = [
    `快照 ID：${snapshot.id}`,
    `归档：${snapshot.archive || "未记录"}`,
    `父版本：${snapshot.parent || "首次版本"}`,
    `接收时间：${absTime(timestamp)}`,
    snapshot.conflictOf ? `冲突时的当前版本：${snapshot.conflictOf}` : "",
    snapshot.promotedAt ? `设为当前版本：${absTime(snapshot.promotedAt)}` : "",
    snapshot.locked ? `已锁定：${snapshot.lockNote || "无备注"}` : "",
  ].filter(Boolean).join("\n");
  const techOpen = !narrowTimeline || !narrowTimeline.matches;
  return `<div class="os-snap ${current ? "current" : ""} ${snapshot.conflict ? "conflict" : ""} ${snapshot.locked ? "locked" : ""}" title="${esc(tooltip)}">
    <span class="os-snap-node"></span>
    <div class="os-snap-main">
      <div class="os-snap-heading">${state}<strong>${esc(source)}</strong>
        <span class="sep">·</span>${relAbsTime(timestamp)}
        <span class="sep">·</span><span class="snap-size">${esc(saveVaultArchiveSize(snapshot.archive, type))}</span>
        ${snapshot.locked ? `<span class="os-lock-note" title="${esc(snapshot.lockNote || "已锁定")}">${icon("lock")}${esc(snapshot.lockNote || "已锁定")}</span>` : ""}</div>
      <details class="os-snap-tech-details"${techOpen ? " open" : ""}>
        <summary><span>详情</span><span class="detail-chevron">${icon("arrow")}</span></summary>
        <div class="os-snap-tech">
          <span>父版本 <code title="${esc(snapshot.parent || "")}">${esc(snapshot.parent ? shortID(snapshot.parent, 22) : "首次版本")}</code></span>
          <span>快照 <code title="${esc(snapshot.id)}">${esc(shortID(snapshot.id, 22))}</code></span>
          ${snapshot.archive ? `<span class="archive-ref">归档 <code title="${esc(snapshot.archive)}">${esc(middleEllipsis(snapshot.archive, 28, 14))}</code></span>` : ""}
          ${snapshot.conflictOf ? `<span>冲突于 <code title="${esc(snapshot.conflictOf)}">${esc(shortID(snapshot.conflictOf, 16))}</code></span>` : ""}
          ${snapshot.locked ? `<span>已锁定 <code title="${esc(snapshot.lockNote || "")}">${esc(snapshot.lockNote || "无备注")}</code></span>` : ""}
        </div>
      </details>
    </div>
    <div class="os-snap-actions">
      ${snapshot.locked
        ? `<button class="icon-btn" type="button" data-action="unlock-savevault" data-id="${esc(snapshot.id)}" title="${esc(snapshot.lockNote ? `已锁定：${snapshot.lockNote}` : "已锁定")}，点击解锁" aria-label="解锁版本">${icon("lock")}</button>`
        : `<button class="icon-btn" type="button" data-action="lock-savevault" data-id="${esc(snapshot.id)}" title="锁定这个版本（清理时永远保留）" aria-label="锁定版本">${icon("lock")}</button>`}
      ${current ? "" : `<button class="icon-btn" type="button" data-action="promote-savevault" data-id="${esc(snapshot.id)}" data-head="${esc(headID || "")}" title="设为当前版本" aria-label="设为当前版本">${icon("restore")}</button>`}
      ${saveVaultDownloadLink(type, snapshot.archive)}
    </div>
  </div>`;
}

function clearSaveVaultCaches() {
  store.saveVaultScopes = null;
  store.saveVaultGames = null;
  store.saveVaultScopeDetail = null;
  store.saveVaultTombstones = null;
  store.saveVaultRetentionPreview = null;
}

function saveVaultManageCard(scope, retired, gameName) {
  const type = encodeURIComponent(scope.type);
  const gameId = encodeURIComponent(scope.gameId);
  const exportURL = `api/savevault/scopes/${type}/${gameId}/export`;
  return `<section class="card scope-side-card scope-manage">
    <div class="card-head"><div><h2>管理这个范围</h2><p>下载、合并或删除 · 所有操作都会记入活动</p></div>
      ${retired ? statusChip("neutral", "已合并（只读）") : statusChip("ok", "可写")}</div>
    <div class="card-body form">
      ${retired ? `<p class="hint manage-retired">已合并到 <b>${esc(gameName)}</b>（只读）。数据仍然保留，可以下载任何版本；仍在跟踪这个范围的设备会收到“云端已合并”的提示。</p>`
        : `<p class="hint">合并只会把这个范围标记为只读，不改动任何归档或当前版本。删除会把范围移入回收区，7 天内可以恢复。</p>`}
      <div class="form-actions manage-actions">
        <a class="btn" href="${exportURL}" download>${icon("download")}下载全部版本（ZIP）</a>
        ${retired
          ? `<button class="btn" type="button" data-action="unretire-savevault">${icon("restore")}取消合并</button>`
          : `<button class="btn" type="button" data-action="merge-savevault">${icon("archive")}合并到…</button>
             <button class="btn danger" type="button" data-action="delete-savevault">${icon("trash")}删除这个范围</button>`}
      </div>
    </div>
  </section>`;
}

async function showSaveVaultMergeDialog(scope) {
  let removeEscape = null;
  const { root, close } = modal(`
    <h2>合并到另一个范围</h2>
    <p>合并后这个范围只读，数据保留；仍在跟踪它的设备会被提示移除重复项。归档和当前版本都不会被改动。</p>
    <div class="merge-list" data-merge-list><div class="merge-loading">正在读取同类型的存档范围…</div></div>
    <p class="form-error" data-merge-error role="alert"></p>
    <div class="modal-actions">
      <button class="btn" type="button" data-merge-cancel>取消</button>
      <button class="btn primary" type="button" data-merge-confirm disabled>合并为只读</button>
    </div>`, {
    className: "wide",
    onClose: () => { if (removeEscape) removeEscape(); },
  });
  removeEscape = closeOnEscape(close);
  const list = root.querySelector("[data-merge-list]");
  const error = root.querySelector("[data-merge-error]");
  const confirm = root.querySelector("[data-merge-confirm]");
  root.querySelector("[data-merge-cancel]").onclick = close;
  confirm.onclick = async () => {
    const selected = root.querySelector('input[name="merge-target"]:checked');
    if (!selected) { error.textContent = "请选择一个目标范围。"; return; }
    error.textContent = "";
    confirm.disabled = true;
    try {
      await api(`savevault/scopes/${encodeURIComponent(scope.type)}/${encodeURIComponent(scope.gameId)}/retire`,
        { method: "POST", body: { into: selected.value } });
      close();
      toast("已合并为只读");
      clearSaveVaultCaches();
      await refresh();
    } catch (caught) {
      if (caught instanceof UnauthorizedError) { close(); showLogin(); return; }
      error.textContent = caught.message || String(caught);
      confirm.disabled = false;
    }
  };
  try {
    const [scopesData, gamesData, tombstonesData] = await Promise.all([
      api("savevault/scopes"), api("savevault/games"), api("savevault/tombstones"),
    ]);
    if (!document.contains(list)) return;
    const games = gamesData.games || [];
    const tombstoneKeys = new Set((tombstonesData.tombstones || []).map((item) => `${item.type}/${item.gameId}`));
    const candidates = (scopesData.scopes || []).filter((item) => item.type === scope.type &&
      item.gameId !== scope.gameId && !tombstoneKeys.has(`${item.type}/${item.gameId}`));
    candidates.sort((left, right) => new Date(right.lastReceivedAt || 0) - new Date(left.lastReceivedAt || 0));
    if (!candidates.length) {
      list.innerHTML = emptyState("没有可合并的目标", "同类型下没有其他可写范围。请先在客户端创建目标范围的存档。", "archive");
      return;
    }
    list.innerHTML = candidates.map((item) => {
      const game = games.find((candidate) => (candidate.scopes || []).some((member) =>
        member.type === item.type && member.gameId === item.gameId));
      const name = game ? gameTitle(game, item.gameId) : item.gameId;
      const latest = item.lastReceivedAt || item.head?.receivedAt;
      return `<label class="merge-option">
        <input type="radio" name="merge-target" value="${esc(item.gameId)}">
        <span class="merge-option-copy">
          <strong>${esc(name)}</strong>
          <code title="${esc(item.gameId)}">${esc(item.gameId)}</code>
          <small>最近上传 ${esc(latest ? relTime(latest) : "从未")} · ${item.snapshotCount || 0} 个版本</small>
        </span>
      </label>`;
    }).join("");
    list.querySelectorAll('input[name="merge-target"]').forEach((input) => {
      input.onchange = () => { confirm.disabled = false; };
    });
  } catch (caught) {
    if (caught instanceof UnauthorizedError) { close(); showLogin(); return; }
    list.innerHTML = "";
    error.textContent = caught.message || String(caught);
  }
}

function showSaveVaultDeleteDialog(scope) {
  const snapshots = scope.snapshots || [];
  const conflictCount = Number(scope.conflictCount) || snapshots.filter((item) => item.conflict).length;
  const lockedCount = snapshots.filter((item) => item.locked).length;
  const cutoff = Date.now() - 30 * 24 * 60 * 60 * 1000;
  const devices = new Map();
  for (const snapshot of snapshots) {
    const time = validDate(snapshot.receivedAt || snapshot.createdAt);
    if (!time || time.getTime() < cutoff) continue;
    const key = snapshot.device || snapshot.deviceName;
    if (!key) continue;
    const name = snapshot.deviceName || deviceName(snapshot.device);
    const previous = devices.get(key);
    if (!previous || time > previous.at) devices.set(key, { name, at: time });
  }
  const recentDevices = [...devices.values()].sort((left, right) => right.at - left.at);
  const exportURL = `api/savevault/scopes/${encodeURIComponent(scope.type)}/${encodeURIComponent(scope.gameId)}/export`;
  let headID = scope.head?.id || "";
  let removeEscape = null;
  const { root, close } = modal(`
    <h2>删除这个范围？</h2>
    <p>删除后，归档会移入回收区，7 天内可以在“维护 › 最近删除”里恢复。设备本地存档不受影响。</p>
    <div class="delete-summary">
      <div><span>版本</span><b>${Number(scope.snapshotCount) || snapshots.length}</b></div>
      <div><span>占用</span><b>${formatBytes(scope.archiveBytes)}</b></div>
      <div><span>锁定版本</span><b>${lockedCount}</b></div>
    </div>
    ${conflictCount ? `<div class="delete-conflict">${icon("activity")}<span><b>${conflictCount} 个冲突副本</b>也会随范围一起删除</span></div>` : `<p class="hint">这个范围没有冲突副本。</p>`}
    <div class="delete-devices"><strong>最近 30 天上传过的设备</strong>
      ${recentDevices.length ? `<ul>${recentDevices.map((device) => `<li><span>${esc(device.name)}</span><small>${esc(relTime(device.at))}</small></li>`).join("")}</ul>`
        : `<p class="hint">最近 30 天没有设备上传过。</p>`}
      <p class="delete-warning">这些设备仍在跟踪这个游戏。升级后的客户端会提示用户；未升级的客户端会在每次同步时报错，但不会重新上传。</p>
    </div>
    <div class="form-actions">
      <a class="btn" href="${exportURL}" download>${icon("download")}先下载全部版本（ZIP）</a>
    </div>
    <label class="field delete-confirm"><span>输入游戏 ID <code>${esc(scope.gameId)}</code> 以确认删除</span>
      <input type="text" id="savevault-delete-confirm" autocomplete="off" autocapitalize="off" spellcheck="false" placeholder="${esc(scope.gameId)}">
    </label>
    <p class="form-error" data-delete-error role="alert"></p>
    <div class="modal-actions">
      <button class="btn" type="button" data-delete-cancel>取消</button>
      <button class="btn danger solid" type="button" data-delete-confirm disabled>删除</button>
    </div>`, {
    className: "wide",
    onClose: () => { if (removeEscape) removeEscape(); },
  });
  removeEscape = closeOnEscape(close);
  const input = root.querySelector("#savevault-delete-confirm");
  const error = root.querySelector("[data-delete-error]");
  const remove = root.querySelector("[data-delete-confirm]");
  root.querySelector("[data-delete-cancel]").onclick = close;
  input.oninput = () => { remove.disabled = input.value.trim() !== scope.gameId; };
  remove.onclick = async () => {
    if (input.value.trim() !== scope.gameId) return;
    error.textContent = "";
    remove.disabled = true;
    try {
      await api(`savevault/scopes/${encodeURIComponent(scope.type)}/${encodeURIComponent(scope.gameId)}`,
        { method: "DELETE", headers: { "If-Match": `"${headID}"` } });
      close();
      toast("存档范围已删除，7 天内可恢复");
      clearSaveVaultCaches();
      location.hash = "#/savevault";
      await refresh();
    } catch (caught) {
      if (caught instanceof UnauthorizedError) { close(); showLogin(); return; }
      if (caught.status === 412) {
        error.textContent = "期间有新的上传，请刷新后重新确认。";
        if (caught.head) headID = caught.head;
        input.value = "";
        remove.disabled = true;
        return;
      }
      if (caught.status === 409) {
        error.textContent = caught.message || "这个范围里还有已合并的存档位置，请先取消合并。";
      } else {
        error.textContent = caught.message || String(caught);
      }
      remove.disabled = input.value.trim() !== scope.gameId;
    }
  };
  input.focus();
}

function saveVaultPruneCard() {
  return `<section class="card scope-side-card savevault-prune">
    <div class="card-head"><div><h2>清理旧版本</h2><p>手动执行 · 不自动清理</p></div></div>
    <div class="card-body form">
      <p class="hint">保留最近若干个普通版本；当前版本和冲突副本不会被删除，冲突副本指向的版本也会保留。锁定的版本不会被清理。删除后无法恢复，建议先预览。</p>
      <div class="inline-fields">
        <label class="field"><span>保留最近</span>
          <input type="number" id="savevault-prune-keep" min="1" max="1000" step="1" value="20">
          <small>个普通版本（1–1000）</small></label>
      </div>
      <div class="form-actions">
        <button class="btn" type="button" id="savevault-prune-preview">${icon("search")}预览</button>
        <button class="btn danger" type="button" id="savevault-prune-run">${icon("broom")}清理</button>
      </div>
      <div class="report" id="savevault-prune-report" hidden></div>
    </div>
  </section>`;
}

function bindSaveVaultPrune(scope) {
  const input = $("#savevault-prune-keep");
  const preview = $("#savevault-prune-preview");
  const run = $("#savevault-prune-run");
  const report = $("#savevault-prune-report");
  if (!input || !preview || !run || !report) return;

  const readKeep = () => {
    const keep = Number(input.value);
    if (!Number.isInteger(keep) || keep < 1 || keep > 1000) {
      toast("保留数量必须是 1 到 1000 之间的整数", true);
      input.focus();
      return null;
    }
    return keep;
  };
  const execute = async (keep, dryRun) => {
    preview.disabled = true;
    run.disabled = true;
    try {
      const result = await guarded(
        () => api(`savevault/scopes/${encodeURIComponent(scope.type)}/${encodeURIComponent(scope.gameId)}/prune`,
          { method: "POST", body: { keep, dryRun } }),
        dryRun ? "" : "旧版本已清理");
      if (!result) return;
      report.hidden = false;
      report.className = `report ${result.removed || result.failed ? "" : "ok"}`;
      report.innerHTML = `<b>${dryRun ? "预览结果" : "清理完成"}</b>
        <span>${dryRun ? "将删除" : "已删除"} ${result.removed} 个旧版本，${dryRun ? "可释放" : "已释放"} ${formatBytes(result.freedBytes)}，
        保留 ${result.kept} 个版本${result.failed ? `，${result.failed} 个归档删除失败` : ""}。</span>`;
      if (!dryRun) {
        store.saveVaultScopeDetail = null;
        store.saveVaultScopes = null;
        store.saveVaultGames = null;
        await refresh();
      }
    } finally {
      if (document.contains(preview)) preview.disabled = false;
      if (document.contains(run)) run.disabled = false;
    }
  };
  preview.onclick = () => {
    const keep = readKeep();
    if (keep !== null) execute(keep, true);
  };
  run.onclick = async () => {
    const keep = readKeep();
    if (keep === null) return;
    const ok = await confirmDialog({
      title: "清理旧版本？", danger: true, confirm: "清理",
      message: `将保留当前版本、全部冲突副本以及最近 ${keep} 个普通版本，其余旧版本及其归档会被永久删除。此操作无法撤销。`,
    });
    if (ok) await execute(keep, false);
  };
}

async function renderSaveVaultScope(type, gameId) {
  const cacheKey = `${type}\u0000${gameId}`;
  if (!store.saveVaultScopeDetail || store.saveVaultScopeDetail.cacheKey !== cacheKey) {
    main.innerHTML = `<div class="skeleton"></div>`;
    const [detail, games, files] = await Promise.all([
      api(`savevault/scopes/${encodeURIComponent(type)}/${encodeURIComponent(gameId)}`),
      store.saveVaultGames ? { games: store.saveVaultGames } : api("savevault/games"),
      store.saveVaultFiles ? { files: store.saveVaultFiles } : api("savevault/files"),
    ]);
    store.saveVaultGames = games.games || [];
    store.saveVaultFiles = files.files || [];
    store.saveVaultScopeDetail = { ...detail, cacheKey };
    if (currentRoute().name !== "savevaultScope") return;
  }
  const scope = store.saveVaultScopeDetail;
  const head = scope.head;
  const snapshots = scope.snapshots || [];
  const game = saveVaultScopeGame(scope);
  const member = game?.scopes.find((item) => item.type === scope.type && item.gameId === scope.gameId);
  const gameName = game ? gameTitle(game, member?.name || scope.gameId) : (member?.name || scope.gameId);
  const retired = Boolean(member?.retired);
  const [linkKind, linkText] = retired ? ["neutral", "已合并（只读）"] : member ? saveVaultLinkStatus([member]) : ["warn", "未识别"];
  const received = head?.receivedAt || scope.lastReceivedAt;
  main.innerHTML = `
    <a class="back" href="#/savevault">${icon("back")}返回 SaveVault 存档</a>
    <section class="card scope-hero" style="--hero-hue:${hue(gameName)}">
      ${coverBlock(gameName, gameCoverURL(game), "lg")}
      <div class="scope-hero-copy">
        <div class="scope-hero-tags">
          <span class="tag accent">${esc(saveVaultTypeLabel(scope.type))}</span>
          ${statusChip(linkKind, linkText)}
          ${retired ? statusChip("neutral", "只读，不再接受上传") : scope.conflictCount ? statusChip("danger", `${scope.conflictCount} 个冲突副本`) : statusChip("ok", "无未解决冲突")}
          ${game ? `<button class="btn sm game-meta-button" type="button" data-action="edit-game-meta" data-key="${esc(game.key)}">${icon("edit")}编辑名称与封面</button>` : ""}
        </div>
        <h1>${esc(gameName)}</h1>
        <div class="scope-hero-id">
          <code title="${esc(scope.gameId)}">${esc(middleEllipsis(scope.gameId, 36, 18))}</code>
          <button class="icon-btn" type="button" data-copy="${esc(scope.gameId)}" title="复制游戏 ID" aria-label="复制游戏 ID">${icon("copy")}</button>
        </div>
        <p class="scope-current">${icon("devices")}${head
          ? `当前版本 · <b>${esc(saveVaultScopeSource(scope))}</b> · ${relAbsTime(received)} · <code title="${esc(head.id)}">${esc(shortID(head.id, 18))}</code>`
          : "当前版本 · 尚未建立"}</p>
      </div>
      <div class="scope-hero-stats">
        <div class="scope-stat"><span>版本</span><b>${scope.snapshotCount || snapshots.length}</b></div>
        <div class="scope-stat"><span>占用</span><b>${formatBytes(scope.archiveBytes)}</b></div>
        <div class="scope-stat"><span>冲突副本</span><b>${scope.conflictCount || 0}</b></div>
      </div>
    </section>
    <section class="card scope-timeline">
      <div class="card-head"><div><h2>版本记录</h2><p>权威版本由父链与 CAS 确认 · 最新在上</p></div>
        <span class="hint">${snapshots.length} 个版本</span></div>
      <div class="card-body os-timeline">${snapshots.length
        ? snapshots.map((item) => saveVaultSnapshotRow(item, head?.id, scope.type)).join("")
        : emptyState("还没有版本", "客户端完成首次原生存档备份后会显示在这里。", "archive")}</div>
    </section>
    <div class="scope-lower">
      ${saveVaultScopeGameCard(scope)}
      ${saveVaultPruneCard()}
      ${saveVaultManageCard(scope, retired, gameName)}
    </div>`;
  bindSaveVaultScopeGameForm(scope);
  bindSaveVaultPrune(scope);
  syncTimelineDetails();
}

function saveVaultScopeGame(scope) {
  return (store.saveVaultGames || []).find((game) =>
    (game.scopes || []).some((member) => member.type === scope.type && member.gameId === scope.gameId));
}

function saveVaultScopeGameCard(scope) {
  const game = saveVaultScopeGame(scope);
  const member = game?.scopes.find((item) => item.type === scope.type && item.gameId === scope.gameId);
  const others = (game?.scopes || []).filter((item) => item !== member);
  const sameType = (store.saveVaultGames || []).filter((item) => item.type === scope.type && item.key !== game?.key);
  const [linkKind, linkText] = member ? saveVaultLinkStatus([member]) : ["warn", "未识别"];
  const gameName = game ? gameTitle(game, member?.name || scope.gameId) : (member?.name || scope.gameId);
  return `<section class="card scope-side-card">
    <div class="card-head"><div><h2>所属游戏</h2><p>只影响分组和显示，不会改动任何版本</p></div>${statusChip(linkKind, linkText)}</div>
    <div class="card-body form">
      <div class="game-link-summary">
        ${coverBlock(gameName, gameCoverURL(game), "sm")}
        <div><strong>${esc(gameName)}</strong><code title="${esc(game?.key || "")}">${esc(middleEllipsis(game?.key || "", 34, 14))}</code></div>
      </div>
      ${others.length ? `<p class="game-link-others"><span class="hint">同一游戏的其他存档位置</span>${others.map((item) =>
        `<a href="${saveVaultScopeLink(item)}" title="${esc(item.gameId)}">${esc(item.name || item.gameId)}</a>`).join("、")}</p>` : ""}
      <form id="savevault-game-form" class="form">
        <label class="field"><span>关联到游戏（同类型）</span>
          <input type="text" name="game" list="savevault-game-keys" placeholder="${esc(scope.type)}/…" value="${member?.link === "manual" ? esc(game?.key || "") : ""}">
          <datalist id="savevault-game-keys">${sameType.map((item) => `<option value="${esc(item.key)}">${esc(item.name)}</option>`).join("")}</datalist>
          <small>选择另一款同类型游戏，把这个存档位置并入它；清空后保存，恢复自动识别。</small></label>
        <div class="form-actions"><button class="btn primary" type="submit">保存关联</button></div>
      </form>
    </div>
  </section>`;
}

function bindSaveVaultScopeGameForm(scope) {
  const form = $("#savevault-game-form");
  if (!form) return;
  form.onsubmit = async (event) => {
    event.preventDefault();
    const game = new FormData(form).get("game").trim();
    const done = await guarded(() => api(`savevault/scopes/${encodeURIComponent(scope.type)}/${encodeURIComponent(scope.gameId)}/game`,
      { method: "PUT", body: { game } }), game ? "已关联到游戏" : "已恢复自动识别");
    if (done) {
      store.saveVaultGames = null;
      store.saveVaultScopeDetail = null;
      render();
    }
  };
}

async function renderScope(id) {
  if (!store.scopeDetail || store.scopeDetail.scope.id !== id) {
    main.innerHTML = `<div class="skeleton"></div>`;
  }
  const detail = await api(`scopes/${encodeURIComponent(id)}`);
  store.scopeDetail = detail;
  if (currentRoute().id !== id) return;
  const { scope, snapshots } = detail;
  const title = scope.title || "未命名存档";

  main.innerHTML = `
    <a class="back" href="#/saves">${icon("back")}存档</a>
    <section class="card">
      <div class="hero" style="--hero-hue:${hue(title)}">${cover(scope, "lg")}
        <div class="hero-text">
          <div class="hero-badges">${scope.named ? statusChip("ok", "已识别") : statusChip("warn", "待命名")}
            ${scope.user ? `<span class="save-user">${icon("user")}${esc(scope.user)}</span>` : ""}</div>
          <h1>${esc(title)}</h1>
          <div class="hero-id"><code title="${esc(scope.id)}">${esc(scope.id)}</code>
            <button class="icon-btn" type="button" data-copy="${esc(scope.id)}" title="复制范围 ID" aria-label="复制范围 ID">${icon("copy")}</button></div>
        </div>
      </div>
      <div class="hero-stats">
        <div><span>当前版本</span><b>${relAbsTime(scope.latest_at)}</b></div>
        <div><span>来自设备</span><b>${esc(deviceName(scope.latest_device))}</b></div>
        <div><span>快照</span><b>${scope.snapshot_count}${scope.pinned_count ? `（固定 ${scope.pinned_count}）` : ""}</b></div>
        <div><span>占用空间</span><b>${formatBytes(scope.stored_bytes)}</b></div>
      </div>
    </section>
    <div class="detail-grid">
      <section class="card">
        <div class="card-head"><h2>快照历史</h2><span class="hint">最新在上 · 当前版本以实心圆标记</span></div>
        <div class="card-body timeline">${snapshots.length ? snapshots.map((s) => snapshotRow(s)).join("")
          : emptyState("还没有快照", "在 Eden 中完成一次同步后，版本会出现在这里。", "archive")}</div>
      </section>
      <div class="stack">
        <section class="card">
          <div class="card-head"><h2>显示设置</h2></div>
          <form class="card-body form" id="scope-form">
            <label class="field"><span>显示名称</span>
              <input type="text" name="title" maxlength="120" value="${esc(scope.title)}" placeholder="游戏名称"></label>
            <label class="field"><span>Logo 图片地址</span>
              <input type="url" name="logo_url" maxlength="2048" value="${esc(scope.logo_url)}" placeholder="https://…">
              <small>${scope.has_client_logo ? "当前使用 Eden 客户端上传的游戏图标；填写地址将替换它。" : "可选。Eden 客户端会在下次同步时自动上传游戏图标。"}</small></label>
            ${scope.has_client_logo ? `<label class="check"><input type="checkbox" name="clear_client_logo">移除客户端上传的图标</label>` : ""}
            <button class="btn primary" type="submit">保存</button>
          </form>
        </section>
        <section class="card">
          <div class="card-head"><h2>如何应用恢复的版本</h2></div>
          <div class="card-body"><p class="hint">“设为当前版本”会在云端创建一个指向旧快照的新版本，历史不会丢失。
            设备本地已有存档时不会被自动覆盖：请在该设备的 Eden 中执行 <code>设置 › 网络 › 存档同步 › 下载最新云存档</code>。</p></div>
        </section>
        <section class="card">
          <div class="card-head"><h2>危险操作</h2></div>
          <div class="card-body form">
            <p class="hint">删除整个存档范围会移除其全部快照记录，未被其他存档引用的加密对象将在下次清理时回收。设备本地存档不受影响。</p>
            <button class="btn danger" type="button" data-action="delete-scope">${icon("trash")}删除此存档范围</button>
          </div>
        </section>
      </div>
    </div>`;

  $("#scope-form").onsubmit = async (event) => {
    event.preventDefault();
    const form = new FormData(event.target);
    const body = { title: form.get("title"), logo_url: form.get("logo_url") };
    if (form.get("clear_client_logo")) body.clear_client_logo = true;
    const result = await guarded(() => api(`scopes/${encodeURIComponent(id)}`, { method: "PATCH", body }), "已保存显示设置");
    if (result) { await refresh(); }
  };
}

function snapshotRow(item) {
  const badges = [];
  if (item.head) badges.push(`<span class="badge accent">当前版本</span>`);
  if (item.conflict) badges.push(`<span class="badge warn" title="与当前云端版本冲突，已固定保留">冲突副本</span>`);
  if (item.pinned) badges.push(`<span class="badge ok">${icon("pin")}已固定</span>`);
  if (item.restored_of) badges.push(`<span class="badge warn" title="${esc(item.restored_of)}">${icon("restore")}恢复自 ${esc(shortID(item.restored_of, 14))}</span>`);
  if (item.shared_blob) badges.push(`<span class="badge" title="与其他快照共享同一加密对象">去重</span>`);
  return `<div class="snap ${item.head ? "head" : ""}">
    <span class="snap-dot"></span>
    <div class="snap-main">
      <div class="snap-line"><strong title="${esc(absTime(item.created_at))}">${esc(absTime(item.created_at))}</strong>
        <span class="hint">${esc(relTime(item.created_at))}</span>${badges.join("")}</div>
      <div class="snap-meta"><span>${esc(deviceName(item.device))}</span><span>${formatBytes(item.size)}</span>
        <code title="${esc(item.id)}">${esc(shortID(item.id, 22))}</code>
        ${item.conflict ? `<span title="冲突时的云端当前版本：${esc(item.conflict_of || "未知")}">与云端版本冲突</span>` : item.parent_missing ? `<span title="父版本已被清理">父版本已清理</span>` : ""}</div>
    </div>
    <div class="snap-actions">
      ${item.head ? "" : `<button class="icon-btn" type="button" data-action="rollback" data-id="${esc(item.id)}" title="设为当前版本" aria-label="设为当前版本">${icon("restore")}</button>`}
      ${item.conflict ? "" : `<button class="icon-btn" type="button" data-action="pin" data-id="${esc(item.id)}" data-pinned="${item.pinned ? 1 : 0}" title="${item.pinned ? "取消固定" : "固定（不会被保留策略清理）"}">${icon("pin")}</button>`}
      <a class="icon-btn" href="api/scopes/${encodeURIComponent(store.scopeDetail.scope.id)}/snapshots/${encodeURIComponent(item.id)}/download" title="下载加密快照" aria-label="下载加密快照" download>${icon("download")}</a>
      ${item.head ? "" : `<button class="icon-btn danger" type="button" data-action="delete-snapshot" data-id="${esc(item.id)}" title="删除快照" aria-label="删除快照">${icon("trash")}</button>`}
    </div>
  </div>`;
}

function renderDevices() {
  const { devices, server } = store.overview;
  const onlineCount = devices.filter((device) => device.online).length;
  const totalUploads = devices.reduce((sum, device) => sum + (Number(device.uploads) || 0), 0);
  const subtitle = devices.length
    ? `${onlineCount} 台在线 · 共 ${devices.length} 台设备 · 累计上传 ${totalUploads} 次 · ${server.online_window_seconds} 秒内通信视为在线`
    : `${devices.length} 台设备 · ${server.online_window_seconds} 秒内有通信视为在线`;
  main.innerHTML = `
    ${pageHead("设备", subtitle)}
    ${devices.length ? `<section class="devices">${devices.map(deviceCard).join("")}</section>`
      : `<div class="card">${emptyState("尚未发现设备", "Eden 客户端连接同步服务后会自动出现在这里。", "devices")}</div>`}`;
}

function deviceCard(device) {
  const [platformCls, platformLabel] = platformClass(device.platform);
  const display = device.alias || device.name || "未命名设备";
  return `<article class="card device${device.online ? " online" : ""}">
    <div class="device-top">
      <span class="platform ${platformCls}">${esc(platformLabel)}</span>
      <div class="device-name"><strong><span title="${esc(display)}">${esc(display)}</span>
        ${device.online ? statusChip("ok", "在线") : statusChip("neutral", "离线")}</strong>
        <small>${device.alias && device.name ? `${esc(device.name)} · ` : ""}${esc(device.platform || "未知平台")}</small></div>
    </div>
    <dl class="kv">
      <dt>最后活动</dt><dd title="${esc(absTime(device.last_seen))}">${esc(relTime(device.last_seen))}</dd>
      <dt>上传快照</dt><dd>${device.uploads} 个${device.last_upload_at ? ` · ${esc(relTime(device.last_upload_at))}` : ""}</dd>
      <dt>认证方式</dt><dd>${device.auth === "device"
        ? `<span class="auth-cell">${statusChip("accent", "设备令牌")}${device.token_id ? `<code title="${esc(device.token_id)}">${esc(shortID(device.token_id, 12))}</code>` : ""}</span>`
        : statusChip("warn", "共享令牌")}</dd>
      <dt>Eden 版本</dt><dd>${esc(device.version || "—")}</dd>
      <dt>网络地址</dt><dd class="mono">${esc(device.address || "—")}</dd>
      <dt>首次接入</dt><dd>${esc(absTime(device.first_seen))}</dd>
      <dt>设备 ID</dt><dd class="mono" title="${esc(device.id)}">${esc(shortID(device.id, 18))}</dd>
    </dl>
    <div class="device-foot">
      <button class="btn sm ghost" data-action="rename-device" data-id="${esc(device.id)}">${icon("edit")}重命名</button>
      <button class="btn sm ghost danger" data-action="delete-device" data-id="${esc(device.id)}">${icon("trash")}移除</button>
    </div>
  </article>`;
}

async function reloadTokenData() {
  const [data, pairings] = await Promise.all([api("device-tokens"), api("pairings")]);
  store.deviceTokens = data;
  store.pairings = pairings.pairings || [];
}

async function reloadTokensPage() {
  try {
    await reloadTokenData();
    if (currentRoute().name === "tokens") renderTokensView();
  } catch (error) {
    if (error instanceof UnauthorizedError) showLogin();
    else toast(error.message || String(error), true);
  }
}

// Refresh the token data behind an open dialog without cancelling it.
function refreshTokenDataSoon() {
  reloadTokenData().then(() => {
    if (currentRoute().name === "tokens") renderTokensView();
  }).catch((error) => {
    if (error instanceof UnauthorizedError) showLogin();
  });
}

// ── 用户（租户）──────────────────────────────────────────────────────────
// 每个用户对应一个 vault：自己的目录、令牌、Eden 范围与 SaveVault 归档。
async function renderUsers() {
  if (!store.users) main.innerHTML = `<div class="skeleton"></div>`;
  await reloadUsers();
  if (currentRoute().name !== "users") return;
  renderUsersView();
}

async function reloadUsers() {
  const data = await guarded(() => api("users"));
  if (data) store.users = data.users || [];
  return store.users || [];
}

function userRow(user) {
  const disabled = user.status && user.status !== "active";
  const expires = user.expires_at ? absTime(user.expires_at) : "";
  const expired = user.expires_at ? new Date(user.expires_at) <= new Date() : false;
  const avatarHue = hue(user.username || "user");
  return `<div class="token-row user-row ${disabled || expired ? "revoked" : ""}">
    <span class="user-avatar" style="--hue:${avatarHue};--hue-2:${(avatarHue + 38) % 360}" aria-hidden="true">${esc(coverInitials(user.username))}</span>
    <div class="token-main">
      <div class="token-title"><strong>${esc(user.username)}</strong>
        ${expired ? statusChip("danger", "已到期") : disabled ? statusChip("danger", "已禁用") : statusChip("ok", "有效")}
        <span class="tag accent">vault ${esc(user.vault_id)}</span></div>
      <div class="token-meta">
        <span title="${esc(user.last_login_at ? absTime(user.last_login_at) : "")}">最近登录：${esc(user.last_login_at ? relTime(user.last_login_at) : "从未登录")}</span>
        <span>创建：${esc(absTime(user.created_at))}</span>
        ${expires ? `<span>到期：${esc(expires)}</span>` : ""}
        <span>配额：${user.quota_bytes ? esc(formatBytes(user.quota_bytes)) : "不限"}</span>
      </div>
      ${user.note ? `<div class="token-meta"><span>备注：${esc(user.note)}</span></div>` : ""}
      <div class="token-id"><span>登录令牌</span><code title="${esc(user.token)}">${esc(middleEllipsis(user.token, 16, 6))}</code>
        <button class="icon-btn" type="button" data-copy="${esc(user.token)}" title="复制令牌" aria-label="复制令牌">${icon("copy")}</button></div>
    </div>
    <div class="token-actions">
      <button class="btn sm ghost" type="button" data-action="reset-user-password" data-id="${esc(user.username)}">${icon("edit")}重置密码</button>
      <button class="btn sm ghost danger" type="button" data-action="delete-user" data-id="${esc(user.username)}">${icon("trash")}删除</button>
    </div>
  </div>`;
}

function renderUsersView() {
  const users = store.users || [];
  main.innerHTML = `
    <div class="page-head"><div><h1>用户</h1>
      <p>每个用户一个独立保险库：自己的目录、令牌、存档范围与归档，互相不可见。</p></div>
      <div class="page-actions"><button class="btn primary" type="button" data-action="create-user">${icon("user")}新建用户</button></div>
    </div>
    <section class="card token-section">
      <div class="card-head"><div><h2>全部用户</h2><p>共 ${users.length} 个 · 密码以 PBKDF2-SHA256（210,000 轮）保存，服务端无法还原</p></div></div>
      <div class="card-body token-list">${users.length ? users.map(userRow).join("") : emptyState("还没有用户", "点右上角「新建用户」创建第一个独立租户账号。", "user")}</div>
    </section>`;
}

function createUserDialog() {
  return new Promise((resolve) => {
    const { root, close } = modal(`
      <h2>新建用户</h2>
      <form class="form">
        <label class="field"><span>用户名</span>
          <input type="text" name="username" maxlength="64" autocomplete="off" placeholder="alice">
          <small>2–64 个字符；保险库目录会用它命名（小写、非法字符换成 -）。</small>
        </label>
        <label class="field"><span>密码</span>
          <input type="password" name="password" maxlength="128" autocomplete="new-password">
          <small>至少 8 位；只在创建这一刻传给服务端，之后只存哈希。</small>
        </label>
        <label class="field"><span>备注（可选）</span>
          <input type="text" name="note" maxlength="120" autocomplete="off" placeholder="订单 #42">
        </label>
        <label class="field"><span>配额 MiB（可选，留空不限）</span>
          <input type="number" name="quota" min="0" step="64" placeholder="0">
        </label>
        <label class="field"><span>到期时间（可选，YYYY-MM-DD）</span>
          <input type="text" name="expires" maxlength="10" placeholder="2027-01-01">
        </label>
        <p class="form-error" role="alert"></p>
        <div class="modal-actions">
          <button class="btn" type="button" data-cancel>取消</button>
          <button class="btn primary" type="submit">创建</button>
        </div>
      </form>`);
    const error = root.querySelector(".form-error");
    root.querySelector("[data-cancel]").onclick = () => { close(); resolve(false); };
    root.onclick = (event) => { if (event.target === root) { close(); resolve(false); } };
    root.querySelector("form").onsubmit = async (event) => {
      event.preventDefault();
      const form = new FormData(event.target);
      const quotaMiB = Number(form.get("quota") || 0);
      const expiresRaw = String(form.get("expires") || "").trim();
      let expiresAt = "";
      if (expiresRaw) {
        const parsed = new Date(`${expiresRaw}T00:00:00Z`);
        if (Number.isNaN(parsed.getTime())) { error.textContent = "到期时间格式应为 YYYY-MM-DD"; return; }
        expiresAt = parsed.toISOString();
      }
      const payload = {
        username: String(form.get("username") || "").trim(),
        password: String(form.get("password") || ""),
        note: String(form.get("note") || "").trim(),
        quotaBytes: Number.isFinite(quotaMiB) ? Math.max(0, Math.round(quotaMiB * 1024 * 1024)) : 0,
        expiresAt,
      };
      const created = await guarded(() => api("users", { method: "POST", body: payload }), "用户已创建");
      if (created === undefined) { error.textContent = "创建失败，请检查用户名/密码"; return; }
      close();
      resolve(true);
    };
    root.querySelector('input[name="username"]').focus();
  });
}

async function renderTokens() {
  if (!store.deviceTokens || !store.pairings) main.innerHTML = `<div class="skeleton"></div>`;
  await reloadTokenData();
  if (currentRoute().name !== "tokens") return;
  renderTokensView();
}

function tokenRow(token) {
  const revoked = Boolean(token.revoked_at);
  const bound = Boolean(token.device_id);
  const via = token.created_via === "pairing" ? "配对码" : token.created_via === "admin" ? "网页生成" : token.created_via || "—";
  return `<div class="token-row ${revoked ? "revoked" : ""}">
    <div class="token-main">
      <div class="token-title"><strong>${esc(token.name || "未命名")}</strong>
        ${revoked ? statusChip("danger", "已吊销") : statusChip("ok", "有效")}
        <span class="tag ${token.created_via === "pairing" ? "accent" : ""}">${esc(via)}</span></div>
      <div class="token-binding">${icon("devices")}<span>${bound
        ? `${esc(token.display_name || `设备 ${shortID(token.device_id, 12)}`)}`
        : "尚未绑定设备"}</span>
        ${bound && !revoked ? (token.online ? statusChip("ok", "在线") : statusChip("neutral", "离线")) : ""}</div>
      <div class="token-meta">
        <span title="${esc(token.last_used_at ? absTime(token.last_used_at) : "")}">最近使用：${esc(token.last_used_at ? relTime(token.last_used_at) : "从未使用")}</span>
        <span>创建：${esc(absTime(token.created_at))}</span>
      </div>
      ${revoked ? `<div class="token-meta token-revoked-at">吊销于：${esc(absTime(token.revoked_at))}</div>` : ""}
      <div class="token-id"><span>令牌 ID</span><code title="${esc(token.id)}">${esc(token.id)}</code>
        <button class="icon-btn" type="button" data-copy="${esc(token.id)}" title="复制令牌 ID" aria-label="复制令牌 ID">${icon("copy")}</button></div>
    </div>
    <div class="token-actions">${revoked
      ? `<button class="btn sm ghost danger" type="button" data-action="delete-token" data-id="${esc(token.id)}">${icon("trash")}删除记录</button>`
      : `<button class="btn sm ghost" type="button" data-action="rename-token" data-id="${esc(token.id)}">${icon("edit")}改名</button>
         <button class="btn sm ghost danger" type="button" data-action="revoke-token" data-id="${esc(token.id)}">${icon("trash")}吊销</button>`}</div>
  </div>`;
}

function sharedTokenDevicesBlock(devices) {
  return `<div class="shared-block"><h3>仍在使用共享令牌的设备</h3>
    ${devices.length ? `<ul class="shared-list">${devices.map((device) => `
      <li class="shared-device"><span class="shared-device-name">${esc(device.name || "未命名设备")}</span>
        ${device.platform ? platformTag(device.platform) : ""}
        <span class="shared-device-time" title="${esc(absTime(device.last_seen))}">${esc(relTime(device.last_seen))}</span></li>`).join("")}</ul>`
      : `<p class="shared-empty">没有设备在用共享令牌</p>`}
  </div>`;
}

function pendingPairingsCard(pairings) {
  if (!pairings.length) return "";
  return `<section class="card token-section">
    <div class="card-head"><div><h2>待认领的配对码</h2><p>尚未被设备使用的配对码 · 服务重启后自动失效</p></div>
      <span class="tag accent">${pairings.length} 个</span></div>
    <div class="card-body pairing-list">${pairings.map((pairing) => {
      const remaining = secondsUntil(pairing.expires_at);
      return `<div class="pairing-row">
        <div class="pairing-row-main"><strong>${esc(pairing.name || "未命名配对码")}</strong>
          <span>${remaining > 0 ? `剩余 ${esc(countdownClock(Math.ceil(remaining)))}` : "已过期"}</span></div>
        <button class="btn sm ghost danger" type="button" data-action="cancel-pairing" data-id="${esc(pairing.id)}">${icon("trash")}取消</button>
      </div>`;
    }).join("")}</div>
  </section>`;
}

function renderTokensView() {
  const data = store.deviceTokens || {};
  const tokens = data.tokens || [];
  const active = tokens.filter((token) => !token.revoked_at);
  const revoked = tokens.filter((token) => token.revoked_at);
  const mode = data.shared_token_mode || "allow";
  const sharedDevices = data.shared_token_devices || [];
  const pairings = store.pairings || [];

  main.innerHTML = `
    ${pageHead("令牌", "管理每台设备的独立访问令牌，或生成一次性配对码让客户端接入。",
      `<button class="btn primary" type="button" data-action="pair-device">${icon("shield")}配对新设备</button>
       <button class="btn" type="button" data-action="create-token">${icon("copy")}直接生成令牌</button>`)}
    <div class="stack">
      <section class="card token-section">
        <div class="card-head"><div><h2>有效令牌</h2><p>吊销后立即生效，设备需要重新配对或使用新令牌。</p></div>
          <span class="tag ${active.length ? "accent" : ""}">${active.length} 个</span></div>
        <div class="card-body token-list">${active.length ? active.map(tokenRow).join("")
          : emptyState("还没有有效令牌", "点击“配对新设备”或“直接生成令牌”创建第一个设备令牌。", "shield")}</div>
      </section>
      ${revoked.length ? `<section class="card token-section">
        <div class="card-head"><div><h2>已吊销</h2><p>保留用于审计，可以删除记录；已吊销的令牌无法恢复。</p></div>
          <span class="tag">${revoked.length} 个</span></div>
        <div class="card-body token-list">${revoked.map(tokenRow).join("")}</div>
      </section>` : ""}
      ${pendingPairingsCard(pairings)}
      <section class="card token-section">
        <div class="card-head"><div><h2>共享令牌兼容期</h2><p>逐步把客户端迁移到设备令牌，全部迁移后再禁用共享令牌。</p></div>
          ${statusChip(mode === "deny" ? "danger" : mode === "warn" ? "warn" : "ok", sharedTokenModeLabel(mode))}</div>
        <div class="card-body">
          <div class="mode-grid">${Object.entries(sharedTokenModes).map(([key, option]) => `
            <button class="mode-option ${mode === key ? "active" : ""}" type="button"
              data-action="set-shared-mode" data-mode="${key}" aria-pressed="${mode === key}">
              <strong>${esc(option.label)}</strong><span>${esc(option.description)}</span>
            </button>`).join("")}</div>
          ${sharedTokenDevicesBlock(sharedDevices)}
        </div>
      </section>
    </div>`;
}

function showTokenSecretDialog(result) {
  const state = { token: String(result.token || ""), name: result.record?.name || "未命名" };
  result.token = "";
  let removeEscape = null;
  const { root, close } = modal(`
    <h2>设备令牌已生成</h2>
    <p>“${esc(state.name)}”的令牌如下。设备首次用有效设备 ID 连接时会自动绑定。</p>
    <p class="token-warning">只显示这一次，关闭后无法再查看。请现在复制并妥善保存。</p>
    <code class="token-secret mono" data-token-secret tabindex="0" aria-label="设备令牌明文"></code>
    <div class="form-actions"><button class="btn primary" type="button" data-token-copy>${icon("copy")}复制令牌</button></div>
    <p class="hint">粘贴到客户端原来填写同步密钥/同步令牌的位置。Eden 只需替换 <code>save_sync_token</code>，加密密钥 <code>save_sync_key</code> 保持不变。</p>
    <div class="modal-actions"><button class="btn" type="button" data-token-close>我已保存，关闭</button></div>`,
  {
    className: "wide",
    onClose: () => {
      state.token = "";
      const secret = root.querySelector("[data-token-secret]");
      if (secret) secret.textContent = "";
      if (removeEscape) removeEscape();
    },
  });
  const secret = root.querySelector("[data-token-secret]");
  secret.textContent = state.token;
  root.querySelector("[data-token-copy]").addEventListener("click", async () => {
    if (!state.token) return;
    try { await navigator.clipboard.writeText(state.token); toast("令牌已复制"); }
    catch { toast("复制失败，请手动选择令牌文本", true); }
  });
  root.querySelector("[data-token-close]").onclick = close;
  removeEscape = closeOnEscape(close);
  secret.focus();
}

function showPairDeviceDialog() {
  const state = {
    name: "", ttl: 10, server: location.origin || "", pairing: null, deadline: 0,
    closed: false, completed: false, expired: false, cancelled: false,
    tickTimer: null, pollTimer: null,
  };
  let removeEscape = null;
  function stopTimers() {
    if (state.tickTimer) clearInterval(state.tickTimer);
    if (state.pollTimer) clearInterval(state.pollTimer);
    state.tickTimer = null;
    state.pollTimer = null;
  }
  const { root, close } = modal("", {
    className: "wide",
    onClose: () => {
      state.closed = true;
      stopTimers();
      state.pairing = null;
      if (removeEscape) removeEscape();
      refreshTokenDataSoon();
    },
  });
  removeEscape = closeOnEscape(close);
  const dialog = root.querySelector(".modal");

  const bindClose = () => {
    dialog.querySelectorAll("[data-pair-close]").forEach((button) => { button.onclick = close; });
  };

  const renderForm = (errorMessage = "") => {
    stopTimers();
    state.pairing = null;
    dialog.innerHTML = `
      <h2>配对新设备</h2>
      <p>生成一次性配对码。客户端输入配对码，或扫描二维码后即可领取设备令牌。</p>
      <form class="form" data-pair-form>
        <label class="field"><span>设备名称（可选）</span>
          <input type="text" name="name" maxlength="80" value="${esc(state.name)}" placeholder="例如：客厅的 Android 平板">
          <small>客户端未上报名称时会使用这里的名称。</small></label>
        <label class="field"><span>有效期（分钟）</span>
          <input type="number" name="ttl" min="1" max="60" step="1" value="${state.ttl}">
          <small>1–60 分钟，默认 10 分钟。</small></label>
        <p class="form-error" data-pair-form-error role="alert">${esc(errorMessage)}</p>
        <div class="modal-actions">
          <button class="btn" type="button" data-pair-close>取消</button>
          <button class="btn primary" type="submit">生成配对码</button>
        </div>
      </form>`;
    bindClose();
    const form = dialog.querySelector("[data-pair-form]");
    const error = dialog.querySelector("[data-pair-form-error]");
    const submit = form.querySelector('button[type="submit"]');
    form.onsubmit = async (event) => {
      event.preventDefault();
      const name = form.elements.name.value.trim();
      const ttl = Number(form.elements.ttl.value);
      if (!Number.isInteger(ttl) || ttl < 1 || ttl > 60) {
        error.textContent = "有效期必须是 1 到 60 之间的整数";
        form.elements.ttl.focus();
        return;
      }
      submit.disabled = true;
      const failure = await createPairing(name, ttl);
      if (!failure) return;
      submit.disabled = false;
      if (failure instanceof UnauthorizedError) { close(); showLogin(); return; }
      error.textContent = failure.message || String(failure);
    };
    form.elements.name.focus();
  };

  const createPairing = async (name, ttl) => {
    try {
      const result = await api("pairings", { method: "POST", body: { name, ttl_minutes: ttl } });
      if (state.closed) return null;
      state.name = name;
      state.ttl = ttl;
      state.pairing = { id: result.id, code: result.code, expiresAt: result.expires_at };
      state.completed = false;
      state.expired = false;
      state.cancelled = false;
      showPairingCode();
      refreshTokenDataSoon();
      return null;
    } catch (error) {
      return error;
    }
  };

  const showPairingCode = () => {
    stopTimers();
    const pairing = state.pairing;
    state.deadline = Date.parse(pairing.expiresAt || "");
    if (Number.isNaN(state.deadline)) state.deadline = Date.now() + state.ttl * 60000;
    dialog.innerHTML = `
      <h2>配对新设备</h2>
      <p>${state.name ? `设备名称：<b>${esc(state.name)}</b>。` : ""}在客户端输入下面的配对码，或扫描二维码。</p>
      <div class="pair-code-block">
        <div class="pair-code">${esc(pairing.code)}</div>
        <button class="btn sm" type="button" data-copy="${esc(pairing.code)}">${icon("copy")}复制配对码</button>
      </div>
      <div class="pair-status" data-pair-status><span>剩余</span><b class="pair-countdown" data-pair-countdown>--:--</b></div>
      <p class="form-error" data-pair-error role="alert"></p>
      <div class="pair-qr-block" data-pair-qr-block>
        <label class="field"><span>服务地址</span>
          <input type="url" data-pair-server value="${esc(state.server)}" spellcheck="false" autocomplete="off">
          <small>二维码和链接中使用的服务地址，默认是当前网页地址；手机需要访问其他地址时在这里修改。</small></label>
        <p class="form-error" data-pair-server-error role="alert"></p>
        <div class="pair-qr-frame" data-pair-qr></div>
      </div>
      <div class="pair-uri" data-pair-uri-block>
        <code data-pair-uri tabindex="0"></code>
        <button class="btn sm" type="button" data-copy="" data-copy-uri disabled>${icon("copy")}复制链接</button>
      </div>
      <div class="modal-actions" data-pair-actions>
        <button class="btn danger" type="button" data-pair-cancel>取消配对码</button>
        <button class="btn primary" type="button" data-pair-close>完成</button>
      </div>`;
    bindClose();

    const serverInput = dialog.querySelector("[data-pair-server]");
    const serverError = dialog.querySelector("[data-pair-server-error]");
    const pairError = dialog.querySelector("[data-pair-error]");
    const qrHolder = dialog.querySelector("[data-pair-qr]");
    const uriText = dialog.querySelector("[data-pair-uri]");
    const uriCopy = dialog.querySelector("[data-copy-uri]");
    const countdown = dialog.querySelector("[data-pair-countdown]");
    const status = dialog.querySelector("[data-pair-status]");
    const actions = dialog.querySelector("[data-pair-actions]");

    const updateServer = () => {
      const server = normalizePairServer(serverInput.value);
      if (!server) {
        serverError.textContent = "服务地址必须以 http:// 或 https:// 开头，并且是有效地址";
        qrHolder.innerHTML = `<span class="pair-qr-placeholder">修正服务地址后显示二维码</span>`;
        uriText.textContent = "";
        uriCopy.dataset.copy = "";
        uriCopy.disabled = true;
        return;
      }
      state.server = server;
      serverError.textContent = "";
      const uri = pairURI(server, pairing.code);
      uriText.textContent = uri;
      uriCopy.dataset.copy = uri;
      uriCopy.disabled = false;
      const qr = pairQRCodeSVG(uri);
      if (qr) {
        qrHolder.innerHTML = qr;
      } else {
        qrHolder.innerHTML = "";
        serverError.textContent = "无法生成二维码";
      }
    };
    serverInput.addEventListener("input", updateServer);
    serverInput.addEventListener("blur", () => {
      const server = normalizePairServer(serverInput.value);
      if (server) serverInput.value = server;
      updateServer();
    });
    updateServer();

    const setFinishedActions = (label, primary) => {
      actions.innerHTML = `<button class="btn" type="button" data-pair-close>关闭</button>
        <button class="btn primary" type="button" data-pair-${primary}>${label}</button>`;
      bindClose();
    };

    const markExpired = () => {
      if (state.expired || state.completed || state.cancelled) return;
      state.expired = true;
      stopTimers();
      status.innerHTML = `${statusChip("danger", "已过期")}<span class="hint">配对码已失效，请重新生成。</span>`;
      dialog.querySelector("[data-pair-qr-block]").hidden = true;
      dialog.querySelector("[data-pair-uri-block]").hidden = true;
      setFinishedActions("重新生成", "regen");
      actions.querySelector("[data-pair-regen]").onclick = async (event) => {
        const button = event.currentTarget;
        button.disabled = true;
        const failure = await createPairing(state.name, state.ttl);
        if (failure) {
          if (failure instanceof UnauthorizedError) { close(); showLogin(); return; }
          button.disabled = false;
          toast(failure.message || String(failure), true);
        }
      };
    };

    const markCompleted = () => {
      if (state.completed || state.expired || state.cancelled) return;
      state.completed = true;
      stopTimers();
      status.innerHTML = `${statusChip("ok", "配对完成")}<span class="hint">设备已领取令牌，令牌列表已刷新。</span>`;
      dialog.querySelector("[data-pair-qr-block]").hidden = true;
      dialog.querySelector("[data-pair-uri-block]").hidden = true;
      actions.innerHTML = `<button class="btn primary" type="button" data-pair-close>完成</button>`;
      bindClose();
      refreshTokenDataSoon();
    };

    const tick = () => {
      if (state.closed || state.completed || state.expired || state.cancelled) return;
      const remaining = (state.deadline - Date.now()) / 1000;
      if (remaining <= 0) { markExpired(); return; }
      countdown.textContent = countdownClock(Math.ceil(remaining));
    };
    tick();
    if (!state.expired) state.tickTimer = setInterval(tick, 1000);

    const checkPairing = async () => {
      if (state.closed || state.completed || state.expired || state.cancelled) return;
      try {
        const data = await api("pairings");
        if (state.closed || state.completed || state.expired || state.cancelled) return;
        if (!(data.pairings || []).some((item) => item.id === state.pairing.id)) {
          if (Date.now() >= state.deadline) markExpired();
          else markCompleted();
        }
      } catch (error) {
        if (error instanceof UnauthorizedError) { close(); showLogin(); }
        // Transient polling failures are retried on the next tick.
      }
    };

    dialog.querySelector("[data-pair-cancel]").onclick = async (event) => {
      if (state.completed || state.expired || state.cancelled) return;
      const button = event.currentTarget;
      button.disabled = true;
      pairError.textContent = "";
      try {
        await api(`pairings/${encodeURIComponent(pairing.id)}`, { method: "DELETE" });
        state.cancelled = true;
        stopTimers();
        toast("配对码已取消");
        close();
      } catch (error) {
        if (error instanceof UnauthorizedError) { close(); showLogin(); return; }
        button.disabled = false;
        pairError.textContent = error.message || String(error);
      }
    };

    if (state.pollTimer) clearInterval(state.pollTimer);
    if (!state.expired) state.pollTimer = setInterval(checkPairing, 3000);
  };

  renderForm();
}

const activityKinds = [
  ["all", "全部"], ["upload", "上传"], ["conflict", "冲突"], ["rollback", "恢复"],
  ["maintenance", "维护"], ["device", "设备"],
];

function matchesActivity(item) {
  switch (store.activityFilter) {
    case "all": return true;
    case "rollback": return item.kind === "rollback" || item.kind === "promote";
    case "maintenance": return ["prune", "prune_scope", "verify", "settings", "savevault_meta", "opensave_meta", "delete_snapshot", "delete_scope",
      "restore_scope", "retire_scope", "unretire_scope", "tombstone_cleared", "snapshot_lock", "snapshot_unlock", "retention"].includes(item.kind);
    case "device": return item.kind.startsWith("device");
    default: return item.kind === store.activityFilter;
  }
}

async function renderActivity(useCached = false) {
  if (!store.events) main.innerHTML = `<div class="skeleton"></div>`;
  if (!useCached || !store.events) {
    store.events = (await api("events")).events;
  }
  if (currentRoute().name !== "activity") return;
  const groups = mergeActivity(store.events).filter((group) =>
    matchesActivity(group.kind === "upload-group" ? group.items[0] : group.item));
  let body = "";
  let lastDay = "";
  for (const group of groups) {
    const day = longDay.format(new Date(group.time));
    if (day !== lastDay) { body += `<div class="day-head">${esc(day)}</div>`; lastDay = day; }
    body += activityRow(group);
  }
  main.innerHTML = `
    ${pageHead("活动", "最近 2000 条同步与管理事件")}
    <div class="toolbar"><div class="chips">${activityKinds.map(([key, label]) =>
      `<button class="chip ${store.activityFilter === key ? "active" : ""}" data-kind="${key}">${label}</button>`).join("")}</div></div>
    <section class="card"><div class="card-body list">${body || emptyState("没有活动", "当前筛选条件下没有记录。", "activity")}</div></section>`;
  main.querySelectorAll("[data-kind]").forEach((chip) => {
    chip.onclick = () => {
      store.activityFilter = chip.dataset.kind;
      renderActivity(true).catch((error) => {
        if (error instanceof UnauthorizedError) showLogin();
        else toast(error.message || String(error), true);
      });
    };
  });
}

function retentionGameRow(key, policy) {
  const name = saveVaultGameNameForKey(key);
  return `<div class="retention-game-row" data-retention-game="${esc(key)}">
    <div class="retention-game-name"><strong title="${esc(name || key)}">${esc(name || key)}</strong><code title="${esc(key)}">${esc(key)}</code></div>
    <label class="check"><input type="checkbox" data-field="enabled" ${policy.enabled ? "checked" : ""}>启用</label>
    <label class="field"><span>保留最近</span><input type="number" data-field="keepVersions" min="0" max="1000" step="1" value="${Number(policy.keepVersions) || 0}"></label>
    <label class="field"><span>保留天数</span><input type="number" data-field="keepDays" min="0" max="3650" step="1" value="${Number(policy.keepDays) || 0}"></label>
    <button class="icon-btn danger" type="button" data-remove-retention title="移除这个游戏的单独设置" aria-label="移除 ${esc(name || key)} 的单独设置">${icon("trash")}</button>
  </div>`;
}

function retentionPreviewReport(preview) {
  if (!preview) return `<div class="report" id="savevault-retention-report"><b>暂无试算结果</b><span>点击“试算”读取当前设置。</span></div>`;
  const scopes = (preview.scopes || []).filter((item) => item.enabled !== false && item.removed > 0);
  const removed = scopes.reduce((sum, item) => sum + (Number(item.removed) || 0), 0);
  const bytes = scopes.reduce((sum, item) => sum + (Number(item.bytes) || 0), 0);
  return `<div class="report ${removed ? "" : "ok"}" id="savevault-retention-report">
    <b>${removed ? `将删除 ${removed} 个版本，释放 ${formatBytes(bytes)}` : "按已保存的设置，没有需要删除的版本"}</b>
    ${scopes.length ? `<div class="retention-preview-list">${scopes.map((item) => {
      const name = saveVaultGameNameForScope(item.type, item.gameId) || item.gameId;
      return `<div class="retention-preview-row">
        <span><b>${esc(name)}</b> <code>${esc(item.type)}/${esc(item.gameId)}</code></span>
        <span>${item.removed} 个版本 · ${formatBytes(item.bytes)}</span>
      </div>`;
    }).join("")}</div>` : ""}
    <span>孤立归档 ${preview.orphans || 0} 个，占用 ${formatBytes(preview.orphanBytes)}。试算使用已保存的设置，不改动任何数据。</span>
  </div>`;
}

function saveVaultRetentionCard(retention, preview) {
  const policy = retention.default || { enabled: false, keepVersions: 20, keepDays: 0 };
  const games = retention.games || {};
  const keys = Object.keys(games).sort((left, right) => left.localeCompare(right));
  const configured = new Set(keys);
  const options = allSaveVaultGames().filter((game) => game.key && !configured.has(game.key))
    .map((game) => `<option value="${esc(game.key)}">${esc(gameTitle(game, game.key))}（${esc(game.key)}）</option>`).join("");
  return `<section class="card" id="savevault-retention">
    <div class="card-head"><div><h2>SaveVault 自动保留</h2><p>默认关闭 · 由维护任务按游戏执行</p></div>
      ${policy.enabled ? statusChip("accent", "默认已开启") : statusChip("neutral", "默认关闭")}</div>
    <form class="card-body form" id="savevault-retention-form">
      <p class="hint">开关打开后，维护任务会按下面的规则删除旧版本。两个条件满足一个就保留；当前版本、冲突副本、冲突副本指向的版本、锁定的版本永远保留。</p>
      <label class="switch-row">
        <input type="checkbox" id="savevault-retention-enabled" ${policy.enabled ? "checked" : ""}>
        <span class="switch-track" aria-hidden="true"></span>
        <span class="switch-copy"><strong>全局默认自动保留</strong>
          <small>没有单独设置的游戏使用这里的规则。已合并（只读）的范围也按规则清理，已删除的范围不参与。</small></span>
      </label>
      <div class="inline-fields">
        <label class="field"><span>保留最近</span>
          <input type="number" id="savevault-retention-versions" min="0" max="1000" step="1" value="${Number(policy.keepVersions) || 0}">
          <small>个普通版本（0–1000，启用时至少 1）</small></label>
        <label class="field"><span>保留最近</span>
          <input type="number" id="savevault-retention-days" min="0" max="3650" step="1" value="${Number(policy.keepDays) || 0}">
          <small>天内收到的版本（0 表示不按天数）</small></label>
      </div>
      <div class="retention-games-block">
        <div class="retention-games-head"><strong>按游戏的单独设置</strong><span class="hint">没有单独设置的游戏使用全局默认</span></div>
        <div class="retention-games" id="savevault-retention-games">${keys.length ? keys.map((key) => retentionGameRow(key, games[key])).join("")
          : `<p class="hint">还没有单独设置的游戏。</p>`}</div>
        <div class="retention-add">
          <select id="savevault-retention-game-select" aria-label="选择要单独设置的游戏" ${options ? "" : "disabled"}>
            <option value="">${options ? "选择游戏…" : "没有可添加的游戏"}</option>${options}
          </select>
          <button class="btn" type="button" id="savevault-retention-add" ${options ? "" : "disabled"}>添加游戏设置</button>
        </div>
      </div>
      ${retentionPreviewReport(preview)}
      <div class="form-actions">
        <button class="btn" type="button" id="savevault-retention-preview">${icon("search")}试算</button>
        <button class="btn primary" type="submit">保存设置</button>
      </div>
    </form>
  </section>`;
}

function saveVaultOrphanCard(retention, preview) {
  const files = preview?.orphanFiles || [];
  const count = Number(preview?.orphans) || files.length;
  const bytes = Number(preview?.orphanBytes) || files.reduce((sum, file) => sum + (Number(file.size) || 0), 0);
  return `<section class="card" id="savevault-orphans">
    <div class="card-head"><div><h2>孤立归档</h2><p>上传成功但没有写入任何版本的归档</p></div>
      <span class="tag ${count ? "warn" : ""}">${count} 个 · ${formatBytes(bytes)}</span></div>
    <div class="card-body form">
      <p class="hint">已删除范围的归档在回收区里，不算孤立归档；已合并（只读）范围的归档有版本引用，也不算。</p>
      <label class="switch-row">
        <input type="checkbox" id="savevault-orphan-toggle" ${retention.orphanCleanup ? "checked" : ""}>
        <span class="switch-track" aria-hidden="true"></span>
        <span class="switch-copy"><strong>清理孤立归档</strong>
          <small>打开后删除超过 24 小时、没有任何版本引用的归档。与自动保留开关互不影响；关闭时只统计，不删除。</small></span>
      </label>
      ${files.length ? `<details class="orphan-details">
        <summary>查看 ${files.length} 个孤立归档</summary>
        <div class="orphan-list">${files.map((file) => `<div class="orphan-row">
          <div class="orphan-main"><code title="${esc(file.name)}">${esc(middleEllipsis(file.name, 26, 14))}</code>
            <small>${esc(saveVaultTypeLabel(file.type))} · 推测 gameId：${esc(file.gameId || "未知")} · ${esc(file.modifiedAt ? absTime(file.modifiedAt) : "时间未知")}</small></div>
          <span class="orphan-size">${formatBytes(file.size)}</span>
        </div>`).join("")}</div>
      </details>` : `<p class="hint">没有孤立归档。</p>`}
    </div>
  </section>`;
}

function saveVaultTombstoneCard(tombstones) {
  return `<section class="card" id="savevault-tombstones">
    <div class="card-head"><div><h2>最近删除</h2><p>删除后 7 天内可恢复 · 合并会一直保留记录</p></div>
      <span class="tag ${tombstones.length ? "accent" : ""}">${tombstones.length} 条</span></div>
    <div class="card-body">${tombstones.length ? `<div class="tombstone-list">${tombstones.map((item) => {
      const name = saveVaultGameNameForScope(item.type, item.gameId) || item.gameId;
      const recreated = item.conflictState === "recreated";
      const retired = item.state === "retired";
      if (retired) {
        const targetName = saveVaultGameNameForKey(item.successorGame) || item.successorGame || item.successor || "另一个范围";
        return `<div class="tombstone-row retired">
          <div class="tombstone-head">${statusChip("neutral", "已合并")}
            <b title="${esc(name)}">${esc(name)}</b><code>${esc(item.type)}/${esc(item.gameId)}</code></div>
          <div class="tombstone-stats"><span>已合并到 <b>${esc(targetName)}</b>（只读）</span><span>${esc(absTime(item.at))}</span></div>
          <div class="tombstone-actions">
            <button class="btn sm ghost" type="button" data-action="unretire-tombstone" data-type="${esc(item.type)}" data-game="${esc(item.gameId)}">${icon("restore")}取消合并</button>
          </div>
        </div>`;
      }
      const remaining = item.purgeAfter ? Math.max(0, Math.ceil((Date.parse(item.purgeAfter) - Date.now()) / 86400000)) : 0;
      return `<div class="tombstone-row${recreated ? " recreated" : ""}">
        <div class="tombstone-head">${recreated ? statusChip("danger", "删除后又被重新上传")
          : item.restorable ? statusChip("warn", "最近删除") : statusChip("neutral", "已清除")}
          <b title="${esc(name)}">${esc(name)}</b><code>${esc(item.type)}/${esc(item.gameId)}</code></div>
        ${recreated ? `<p class="recreate-warning">${icon("activity")}删除后又被重新上传。墓碑不会自动删除新数据，请确认是再次删除，还是解除标记。</p>` : ""}
        <div class="tombstone-stats">
          <span>删除于 ${esc(absTime(item.at))}</span>
          <span>${item.restorable ? `剩余 ${remaining} 天` : "已清除，可以解除删除标记"}</span>
          <span>${Number(item.snapshots) || 0} 个版本 · ${Number(item.conflicts) || 0} 个冲突副本 · ${formatBytes(item.bytes)}</span>
        </div>
        <div class="tombstone-actions">
          ${recreated ? `<button class="btn sm ghost danger" type="button" data-action="delete-recreated-tombstone" data-type="${esc(item.type)}" data-game="${esc(item.gameId)}">${icon("trash")}再次删除</button>` : ""}
          ${item.restorable ? `<button class="btn sm primary" type="button" data-action="restore-tombstone" data-type="${esc(item.type)}" data-game="${esc(item.gameId)}">${icon("restore")}恢复</button>` : ""}
          <button class="btn sm ghost danger" type="button" data-action="clear-tombstone" data-type="${esc(item.type)}" data-game="${esc(item.gameId)}">${icon("trash")}解除标记</button>
        </div>
        ${recreated ? `<p class="hint">解除标记后设备可以重新上传为新存档；再次删除会按当前版本重新进入回收区。</p>` : ""}
      </div>`;
    }).join("")}</div>` : emptyState("没有删除或合并记录", "删除的范围会在这里保留 7 天，可以恢复；合并的记录会一直保留到取消合并。", "trash")}</div>
  </section>`;
}

async function refreshSaveVaultRetentionPreview() {
  try {
    store.saveVaultRetentionPreview = await api("savevault/retention/preview", { method: "POST" });
  } catch (error) {
    if (error instanceof UnauthorizedError) throw error;
    store.saveVaultRetentionPreview = null;
  }
  return store.saveVaultRetentionPreview;
}

function bindSaveVaultRetention(current) {
  const form = $("#savevault-retention-form");
  const gamesBox = $("#savevault-retention-games");
  const select = $("#savevault-retention-game-select");
  const add = $("#savevault-retention-add");
  if (!form) return;

  const readNumber = (input, min, max, label) => {
    const value = Number(input.value);
    if (!Number.isInteger(value) || value < min || value > max) {
      toast(`${label}必须是 ${min} 到 ${max} 之间的整数`, true);
      input.focus();
      return null;
    }
    return value;
  };
  const collect = () => {
    const enabled = $("#savevault-retention-enabled").checked;
    const versions = readNumber($("#savevault-retention-versions"), 0, 1000, "全局保留版本数");
    if (versions === null) return null;
    if (enabled && versions < 1) { toast("开启自动保留时，保留版本数至少为 1", true); return null; }
    const days = readNumber($("#savevault-retention-days"), 0, 3650, "全局保留天数");
    if (days === null) return null;
    const games = {};
    for (const row of gamesBox.querySelectorAll("[data-retention-game]")) {
      const key = row.dataset.retentionGame;
      const rowEnabled = row.querySelector('[data-field="enabled"]').checked;
      const rowVersionsInput = row.querySelector('[data-field="keepVersions"]');
      const rowDaysInput = row.querySelector('[data-field="keepDays"]');
      const rowVersions = readNumber(rowVersionsInput, 0, 1000, `${key} 的保留版本数`);
      if (rowVersions === null) return null;
      if (rowEnabled && rowVersions < 1) { toast(`开启自动保留时，${key} 的保留版本数至少为 1`, true); rowVersionsInput.focus(); return null; }
      const rowDays = readNumber(rowDaysInput, 0, 3650, `${key} 的保留天数`);
      if (rowDays === null) return null;
      games[key] = { enabled: rowEnabled, keepVersions: rowVersions, keepDays: rowDays };
    }
    return { default: { enabled, keepVersions: versions, keepDays: days }, orphanCleanup: Boolean(current.orphanCleanup), games };
  };

  const save = async (settings, message) => {
    const result = await guarded(() => api("savevault/retention", { method: "PUT", body: settings }), message);
    if (!result) return false;
    store.saveVaultRetention = result;
    current = result;
    await refreshSaveVaultRetentionPreview();
    renderMaintenance();
    return true;
  };

  form.onsubmit = async (event) => {
    event.preventDefault();
    const settings = collect();
    if (!settings) return;
    const enablingDefault = settings.default.enabled && !current.default?.enabled;
    const enablingGame = Object.entries(settings.games).some(([key, policy]) =>
      policy.enabled && !(current.games?.[key]?.enabled));
    if (enablingDefault || enablingGame) {
      const ok = await confirmDialog({
        title: "开启自动保留？", danger: true, confirm: "开启并保存",
        message: "维护任务会按规则永久删除旧版本及其归档。当前版本、冲突副本、冲突副本指向的版本和锁定的版本不会被删除。",
      });
      if (!ok) return;
    }
    await save(settings, "自动保留设置已保存");
  };

  gamesBox.onclick = (event) => {
    const button = event.target.closest("[data-remove-retention]");
    if (!button) return;
    const row = button.closest("[data-retention-game]");
    const key = row?.dataset.retentionGame;
    row?.remove();
    if (gamesBox && !gamesBox.querySelector("[data-retention-game]")) {
      gamesBox.innerHTML = `<p class="hint">还没有单独设置的游戏。</p>`;
    }
    if (select && key) {
      const option = [...select.options].find((item) => item.value === key);
      if (option) option.disabled = false;
    }
  };

  if (add && select) add.onclick = () => {
    const key = select.value;
    if (!key) { toast("请先选择要添加的游戏", true); return; }
    if ([...gamesBox.querySelectorAll("[data-retention-game]")].some((row) => row.dataset.retentionGame === key)) {
      toast("这个游戏已经有单独设置", true);
      return;
    }
    const placeholder = gamesBox.querySelector(".hint");
    if (placeholder) placeholder.remove();
    const policy = current.default || { enabled: false, keepVersions: 20, keepDays: 0 };
    gamesBox.insertAdjacentHTML("beforeend", retentionGameRow(key, {
      enabled: true, keepVersions: policy.keepVersions || 20, keepDays: policy.keepDays || 0,
    }));
    const option = [...select.options].find((item) => item.value === key);
    if (option) option.disabled = true;
    select.value = "";
  };

  const previewButton = $("#savevault-retention-preview");
  if (previewButton) previewButton.onclick = async () => {
    previewButton.disabled = true;
    const result = await guarded(() => api("savevault/retention/preview", { method: "POST" }));
    if (result) {
      store.saveVaultRetentionPreview = result;
      renderMaintenance();
    } else if (document.contains(previewButton)) {
      previewButton.disabled = false;
    }
  };

  const orphanToggle = $("#savevault-orphan-toggle");
  if (orphanToggle) orphanToggle.onchange = async () => {
    const enabled = orphanToggle.checked;
    const ok = await confirmDialog({
      title: enabled ? "打开孤立归档清理？" : "关闭孤立归档清理？", danger: enabled, confirm: enabled ? "打开" : "关闭",
      message: enabled
        ? "打开后，维护任务会删除超过 24 小时、没有任何版本引用的归档。归档会立即删除，无法恢复。"
        : "关闭后，维护任务只统计孤立归档的数量和占用，不再删除。",
    });
    if (!ok) { orphanToggle.checked = !enabled; return; }
    orphanToggle.disabled = true;
    const settings = {
      default: current.default || { enabled: false, keepVersions: 20, keepDays: 0 },
      games: current.games || {},
      orphanCleanup: enabled,
    };
    const result = await guarded(() => api("savevault/retention", { method: "PUT", body: settings }),
      enabled ? "孤立归档清理已打开" : "孤立归档清理已关闭");
    if (result) {
      store.saveVaultRetention = result;
      current = result;
      await refreshSaveVaultRetentionPreview();
      renderMaintenance();
    } else {
      orphanToggle.disabled = false;
      orphanToggle.checked = !enabled;
    }
  };
}

async function renderMaintenance() {
  if (!store.saveVaultRetention || !store.saveVaultTombstones) {
    main.innerHTML = `<div class="skeleton"></div>`;
    try {
      const [retentionData, tombstoneData] = await Promise.all([
        api("savevault/retention"), api("savevault/tombstones"),
      ]);
      store.saveVaultRetention = retentionData;
      store.saveVaultTombstones = tombstoneData.tombstones || [];
      await refreshSaveVaultRetentionPreview();
    } catch (error) {
      if (error instanceof UnauthorizedError) { showLogin(); return; }
      toast(error.message || String(error), true);
      return;
    }
    if (currentRoute().name !== "maintenance") return;
  }
  const { settings, server } = store.overview;
  const edenRetention = settings.retention;
  const openRetention = store.saveVaultRetention;
  const tombstones = store.saveVaultTombstones || [];
  const retentionPreview = store.saveVaultRetentionPreview;
  const prune = store.pruneReport;
  const verify = store.verifyReport;
  const origin = location.origin;
  const metaSettings = store.metaSettings || { coverFetch: false, lockedByEnv: false };

  main.innerHTML = `
    ${pageHead("维护", "保留策略、存储清理与完整性校验")}
    <div class="stack management-stack">
      ${saveVaultRetentionCard(openRetention, retentionPreview)}
      ${saveVaultOrphanCard(openRetention, retentionPreview)}
      ${saveVaultTombstoneCard(tombstones)}
    </div>
    <div class="grid two">
      <div class="stack">
        <section class="card">
          <div class="card-head"><h2>快照保留策略</h2>${edenRetention.keep_snapshots || edenRetention.keep_days
            ? statusChip("accent", "已启用") : statusChip("neutral", "保留全部")}</div>
          <form class="card-body form" id="retention-form">
            <div class="inline-fields">
              <label class="field"><span>每个存档至少保留最近</span>
                <input type="number" name="keep_snapshots" min="0" max="10000" value="${edenRetention.keep_snapshots}"><small>个快照，0 表示不限</small></label>
              <label class="field"><span>并保留最近</span>
                <input type="number" name="keep_days" min="0" max="36500" value="${edenRetention.keep_days}"><small>天内的快照，0 表示不限</small></label>
            </div>
            <p class="hint">满足任一条件的快照都会保留；当前版本与已固定的快照永远不会被清理。两项都为 0 时不自动清理。
              策略在每次上传后以及每隔一段时间自动执行。</p>
            <div><button class="btn primary" type="submit">保存策略</button></div>
          </form>
        </section>
        <section class="card">
          <div class="card-head"><h2>存储清理</h2></div>
          <div class="card-body">
            <p class="hint">按保留策略移除过期快照，并回收不再被任何快照引用的加密对象（1 小时内上传的对象会跳过）。</p>
            <div class="form-actions card-actions">
              <button class="btn" data-action="prune-preview">${icon("search")}预览</button>
              <button class="btn primary" data-action="prune-run">${icon("broom")}立即清理</button>
            </div>
            ${prune ? (() => {
              const hasChanges = prune.snapshots || prune.objects || prune.covers
                || prune.savevault_versions || prune.savevault_archives || prune.savevault_orphans_removed
                || prune.tombstones_purged || prune.temporary_removed;
              const saveVaultParts = [];
              if (prune.savevault_versions || prune.savevault_archives) {
                saveVaultParts.push(`SaveVault ${prune.savevault_versions || 0} 个版本 / ${prune.savevault_archives || 0} 个归档`);
              }
              if (prune.savevault_orphans_removed || prune.savevault_orphan_archives) {
                saveVaultParts.push(`孤立归档 ${prune.dry_run ? (prune.savevault_orphan_archives || 0) : (prune.savevault_orphans_removed || 0)} 个`);
              }
              if (prune.savevault_freed_bytes) {
                saveVaultParts.push(`SaveVault ${prune.dry_run ? "可释放" : "释放"} ${formatBytes(prune.savevault_freed_bytes)}`);
              }
              if (prune.covers) saveVaultParts.push(`未引用封面 ${prune.covers} 个`);
              if (prune.tombstones_purged) saveVaultParts.push(`过期回收区 ${prune.tombstones_purged} 个`);
              return `<div class="report ${hasChanges ? "" : "ok"}">
              <b>${prune.dry_run ? "预览结果" : "清理完成"}</b>
              <span>${prune.dry_run ? "将移除" : "已移除"} ${prune.snapshots} 个快照（${prune.scopes.length} 个存档范围）、${prune.objects} 个对象，
                ${prune.dry_run ? "可回收" : "回收"} ${formatBytes(prune.reclaimed_bytes)}${prune.temporary_removed ? `，临时文件 ${prune.temporary_removed} 个` : ""}${saveVaultParts.length ? `；${saveVaultParts.join("，")}` : ""}。</span></div>`;
            })() : ""}
          </div>
        </section>
        <section class="card">
          <div class="card-head"><h2>完整性校验</h2></div>
          <div class="card-body">
            <p class="hint">重新计算每个被引用的 Eden 加密对象和 SaveVault 存档的 SHA-256，发现丢失或损坏的快照。数据较多时可能需要几分钟。</p>
            <div class="form-actions card-actions">
              <button class="btn" data-action="verify">${icon("shield")}开始校验</button></div>
            ${verify ? `<div class="report ${verify.problems.length ? "bad" : "ok"}">
              <b>${verify.problems.length ? `发现 ${verify.problems.length} 个问题` : "全部正常"}</b>
              <span>检查 ${verify.objects} 个对象，共 ${formatBytes(verify.bytes)}，用时 ${verify.duration_seconds.toFixed(1)} 秒。</span>
              ${verify.problems.map((p) => `<span>${p.problem === "missing" ? "丢失" : p.problem === "corrupt" ? "损坏" : "无法读取"}：
                ${p.archive
                  ? p.scopes.map((id) => { const [type, ...game] = id.split("/"); return `<a href="#/savevault/${encodeURIComponent(type)}/${encodeURIComponent(game.join("/"))}"><b>SaveVault ${esc(id)}</b></a>`; }).join("、")
                  : p.scopes.map((id) => `<a href="#/saves/${encodeURIComponent(id)}"><b>${esc(scopeTitle(id))}</b></a>`).join("、")}
                <code>${esc(p.archive || shortID(p.blob, 16))}</code></span>`).join("")}
            </div>` : ""}
          </div>
        </section>
      </div>
      <div class="stack">
        <section class="card">
          <div class="card-head"><h2>服务信息</h2></div>
          <div class="card-body"><dl class="kv">
            <dt>版本</dt><dd class="mono">${esc(server.version)}</dd>
            <dt>运行环境</dt><dd class="mono">${esc(server.go_version)}</dd>
            <dt>启动时间</dt><dd>${esc(absTime(server.started_at))}</dd>
            <dt>已运行</dt><dd>${esc(duration((Date.now() - new Date(server.started_at)) / 1000))}</dd>
            <dt>单个对象上限</dt><dd>${formatBytes(server.max_object_bytes)}</dd>
            <dt>请求数</dt><dd>${server.requests.toLocaleString("zh-CN")}</dd>
            ${server.disk_total ? `<dt>数据卷</dt><dd>${formatBytes(server.disk_free)} 可用 / ${formatBytes(server.disk_total)}</dd>` : ""}
          </dl></div>
        </section>
        <section class="card">
          <div class="card-head"><h2>从上游缓存封面</h2>${metaSettings.coverFetch
            ? statusChip("accent", "已开启") : statusChip("neutral", "已关闭")}</div>
          <div class="card-body form">
            <label class="switch-row">
              <input type="checkbox" id="cover-fetch-toggle" role="switch" ${metaSettings.coverFetch ? "checked" : ""} ${metaSettings.lockedByEnv ? "disabled" : ""}>
              <span class="switch-track" aria-hidden="true"></span>
              <span class="switch-copy"><strong>允许从上游缓存封面</strong>
                <small>打开后 NAS 会访问 Steam、Nintendo、tinfoil，并带上 AppID/TitleID；国内网络建议配 HTTPS_PROXY。</small></span>
            </label>
            <p class="hint">只在身份上报、首次上传存档或后台维护时抓取，读取操作不会触发请求。单次超时 8 秒、最多 2 个并发；404 记为没有封面并在 24 小时内不再尝试。${metaSettings.lockedByEnv
              ? `当前由环境变量 <code>GAME_SAVE_COVER_FETCH</code> 控制，网页上的开关不可修改。` : ""}</p>
          </div>
        </section>
        <section class="card">
          <div class="card-head"><h2>监控与脚本</h2></div>
          <div class="card-body form">
            <p class="hint">Prometheus 指标位于 <code>/metrics</code>，使用 HTTP Basic Auth（任意用户名，密码为管理员令牌）。</p>
            <pre class="snippet">curl -u admin:$GAME_SAVE_ADMIN_TOKEN \\
  ${esc(origin)}/metrics</pre>
            <p class="hint">管理 API 位于 <code>/admin/api/</code>，同样支持 Basic Auth；修改类请求需附带请求头 <code>X-Game-Save-Admin: 1</code>。</p>
          </div>
        </section>
      </div>
    </div>`;

  bindSaveVaultRetention(openRetention);

  $("#retention-form").onsubmit = async (event) => {
    event.preventDefault();
    const form = new FormData(event.target);
    const body = { retention: {
      keep_snapshots: Number(form.get("keep_snapshots")) || 0,
      keep_days: Number(form.get("keep_days")) || 0,
    } };
    const result = await guarded(() => api("settings", { method: "PUT", body }), "保留策略已保存");
    if (result) { store.pruneReport = null; await refresh(); }
  };

  const coverToggle = $("#cover-fetch-toggle");
  if (coverToggle && !coverToggle.disabled) coverToggle.onchange = async () => {
    const enabled = coverToggle.checked;
    coverToggle.disabled = true;
    const result = await guarded(() => api("meta-settings", { method: "PUT", body: { coverFetch: enabled } }),
      enabled ? "上游封面抓取已开启" : "上游封面抓取已关闭");
    if (result) {
      store.metaSettings = result;
      await loadOverview();
      renderMaintenance();
    } else {
      coverToggle.checked = !enabled;
      coverToggle.disabled = false;
    }
  };
}

// ---------- routing ----------

function safeDecodeURI(value) {
  try { return decodeURIComponent(value); } catch { return value; }
}

function currentRoute() {
  const hash = location.hash.replace(/^#\/?/, "");
  const [rawName, id, detail] = hash.split("/");
  const name = rawName === "opensave" ? "savevault" : rawName;
  if (name === "saves" && id) return { name: "scope", id: safeDecodeURI(id), nav: "saves" };
  if (name === "savevault" && id && detail) {
    return { name: "savevaultScope", type: safeDecodeURI(id), gameId: safeDecodeURI(detail), nav: "savevault" };
  }
  const known = ["saves", "savevault", "devices", "users", "tokens", "activity", "maintenance"];
  return { name: known.includes(name) ? name : "overview", nav: known.includes(name) ? name : "overview" };
}

async function render() {
  const route = currentRoute();
  document.querySelectorAll("#nav a").forEach((link) => {
    const active = link.dataset.route === route.nav;
    link.classList.toggle("active", active);
    if (active) link.setAttribute("aria-current", "page");
    else link.removeAttribute("aria-current");
  });
  const titles = { overview: "概览", saves: "存档", scope: "存档", savevault: "SaveVault", savevaultScope: "SaveVault 存档", devices: "设备", users: "用户", tokens: "令牌", activity: "活动", maintenance: "维护" };
  document.title = `${titles[route.name]} · SaveVault 云端`;
  switch (route.name) {
    case "saves": return renderSaves();
    case "scope": return renderScope(route.id);
    case "savevault": return renderSaveVault();
    case "savevaultScope": return renderSaveVaultScope(route.type, route.gameId);
    case "devices": return renderDevices();
    case "users": return renderUsers();
    case "tokens": return renderTokens();
    case "activity": return renderActivity();
    case "maintenance": return renderMaintenance();
    default: return renderOverview();
  }
}

let refreshing = false;
async function refresh({ quiet = false } = {}) {
  if (refreshing) return;
  refreshing = true;
  try {
    await loadOverview();
    const route = currentRoute();
    // Avoid wiping half-typed form input or active SaveVault caches during background refreshes.
    const editing = main.contains(document.activeElement) && document.activeElement.matches("input, select, textarea");
    if (!(quiet && editing && route.name !== "overview")) {
      store.saveVaultFiles = null;
      store.saveVaultScopes = null;
      store.saveVaultGames = null;
      store.saveVaultScopeDetail = null;
      store.saveVaultRetention = null;
      store.saveVaultTombstones = null;
      store.saveVaultRetentionPreview = null;
      await render();
    }
  } catch (error) {
    if (error instanceof UnauthorizedError) showLogin();
    else if (!quiet) toast(error.message || String(error), true);
  } finally {
    refreshing = false;
  }
}

// ---------- actions ----------

async function onAction(event) {
  const target = event.target.closest("[data-action], [data-copy]");
  if (!target) return;
  if (target.dataset.copy) {
    try { await navigator.clipboard.writeText(target.dataset.copy); toast("已复制"); } catch { toast("复制失败", true); }
    return;
  }
  const action = target.dataset.action;
  const id = target.dataset.id;
  const scopeID = store.scopeDetail?.scope.id;
  switch (action) {
    case "theme": return toggleTheme();
    case "edit-game-meta": return showGameMetaDialog(target.dataset.key);
    case "refresh": {
      target.classList.add("spinning");
      try {
        await refresh();
        toast("已刷新");
      } finally {
        target.classList.remove("spinning");
      }
      return;
    }
    case "logout":
      try {
        await fetch("api/logout", { method: "POST", headers: { "X-Game-Save-Admin": "1" } });
      } catch { /* clear local UI regardless of network errors */ }
      return showLogin();
    case "rollback": {
      const ok = await confirmDialog({
        title: "设为当前版本？",
        message: "云端会新增一个指向该快照的版本，作为最新存档；原有历史全部保留。设备需执行“下载最新云存档”才会应用到本地。",
        confirm: "设为当前版本",
      });
      if (!ok) return;
      if (await guarded(() => api(`scopes/${encodeURIComponent(scopeID)}/rollback`, { method: "POST", body: { snapshot: id } }), "已恢复为当前版本")) await refresh();
      return;
    }
    case "promote-savevault": {
      const scope = store.saveVaultScopeDetail;
      if (!scope) return;
      const ok = await confirmDialog({
        title: "设为当前版本？",
        message: "其他设备下次上传会以它为父版本；旧头仍保留在历史里。",
        confirm: "设为当前版本",
      });
      if (!ok) return;
      const done = await guarded(() => api(`savevault/scopes/${encodeURIComponent(scope.type)}/${encodeURIComponent(scope.gameId)}/promote`,
        { method: "POST", body: { snapshot: id, expectedHead: target.dataset.head || "" } }), "已设为当前版本");
      if (done) {
        store.saveVaultScopes = null;
        store.saveVaultGames = null;
        store.saveVaultScopeDetail = null;
        await render();
      }
      return;
    }
    case "pin": {
      const pinned = target.dataset.pinned !== "1";
      if (await guarded(() => api(`scopes/${encodeURIComponent(scopeID)}/snapshots/${encodeURIComponent(id)}`,
        { method: "PATCH", body: { pinned } }), pinned ? "已固定快照" : "已取消固定")) await refresh();
      return;
    }
    case "lock-savevault": {
      const scope = store.saveVaultScopeDetail;
      if (!scope) return;
      const snapshot = (scope.snapshots || []).find((item) => item.id === id);
      const note = await promptDialog({
        title: "锁定这个版本", label: "备注（可选）", value: snapshot?.lockNote || "", maxlength: 120,
        placeholder: "例如：通关前",
        hint: "锁定的版本不会被自动保留或手动清理删除，备注最多 120 个字符。",
      });
      if (note === null) return;
      const saved = await guarded(() => api(
        `savevault/scopes/${encodeURIComponent(scope.type)}/${encodeURIComponent(scope.gameId)}/snapshots/${encodeURIComponent(id)}/lock`,
        { method: "PUT", body: { note } }), "已锁定版本");
      if (saved) {
        store.saveVaultScopeDetail = null;
        await refresh();
      }
      return;
    }
    case "unlock-savevault": {
      const scope = store.saveVaultScopeDetail;
      if (!scope) return;
      const ok = await confirmDialog({
        title: "解锁这个版本？", danger: true, confirm: "解锁",
        message: "解锁后这个版本不再受锁定保护，可能被自动保留或手动清理删除。",
      });
      if (!ok) return;
      const done = await guarded(() => api(
        `savevault/scopes/${encodeURIComponent(scope.type)}/${encodeURIComponent(scope.gameId)}/snapshots/${encodeURIComponent(id)}/lock`,
        { method: "DELETE" }), "已解锁版本");
      if (done !== undefined) {
        store.saveVaultScopeDetail = null;
        await refresh();
      }
      return;
    }
    case "merge-savevault": {
      const scope = store.saveVaultScopeDetail;
      if (scope) await showSaveVaultMergeDialog(scope);
      return;
    }
    case "delete-savevault": {
      const scope = store.saveVaultScopeDetail;
      if (scope) showSaveVaultDeleteDialog(scope);
      return;
    }
    case "unretire-savevault": {
      const scope = store.saveVaultScopeDetail;
      if (!scope) return;
      const ok = await confirmDialog({
        title: "取消合并？", danger: true, confirm: "取消合并",
        message: "取消后这个范围恢复可写。已经清掉已确认头的设备下次同步时会作为冲突副本上传，不会覆盖云端当前版本。",
      });
      if (!ok) return;
      const done = await guarded(() => api(
        `savevault/scopes/${encodeURIComponent(scope.type)}/${encodeURIComponent(scope.gameId)}/unretire`,
        { method: "POST" }), "已取消合并");
      if (done) {
        clearSaveVaultCaches();
        await refresh();
      }
      return;
    }
    case "delete-snapshot": {
      const ok = await confirmDialog({
        title: "删除快照？", danger: true, confirm: "删除",
        message: "该快照记录将被永久删除，其加密对象若未被其他快照引用，会在下次清理时回收。此操作无法撤销。",
      });
      if (!ok) return;
      if (await guarded(() => api(`scopes/${encodeURIComponent(scopeID)}/snapshots/${encodeURIComponent(id)}`, { method: "DELETE" }), "快照已删除")) await refresh();
      return;
    }
    case "delete-scope": {
      const ok = await confirmDialog({
        title: "删除整个存档范围？", danger: true, confirm: "永久删除",
        message: `将删除 <b>${esc(scopeTitle(scopeID))}</b> 的全部 ${store.scopeDetail.snapshots.length} 个快照记录。设备本地存档不受影响，但下次同步时会作为新存档重新上传。此操作无法撤销。`,
      });
      if (!ok) return;
      if (await guarded(() => api(`scopes/${encodeURIComponent(scopeID)}`, { method: "DELETE" }), "存档范围已删除")) {
        store.scopeDetail = null;
        location.hash = "#/saves";
        await refresh();
      }
      return;
    }
    case "create-token": {
      const name = await promptDialog({
        title: "直接生成设备令牌", label: "名称（可选）", placeholder: "例如：书房的 PC",
        hint: "仅用于识别。留空则显示为“未命名”，之后可以改名。",
      });
      if (name === null) return;
      const result = await guarded(() => api("device-tokens", { method: "POST", body: { name } }));
      if (!result) return;
      showTokenSecretDialog(result);
      refreshTokenDataSoon();
      return;
    }
    case "pair-device": {
      showPairDeviceDialog();
      return;
    }
    case "rename-token": {
      const token = tokenByID(id);
      const name = await promptDialog({
        title: "重命名设备令牌", label: "名称", value: token?.name || "",
        placeholder: token?.display_name || "令牌名称", hint: "留空则显示为“未命名”。",
      });
      if (name === null) return;
      if (await guarded(() => api(`device-tokens/${encodeURIComponent(id)}`, { method: "PATCH", body: { name } }), "令牌名称已更新")) {
        await reloadTokensPage();
      }
      return;
    }
    case "create-user": {
      if (await createUserDialog()) await reloadUsers().then(renderUsersView);
      return;
    }
    case "reset-user-password": {
      const password = await promptDialog({
        title: "重置密码",
        label: `用户 ${id} 的新密码`,
        hint: "至少 8 位；保存后旧密码立即失效，已登录的设备仍持有自己的令牌。",
        maxlength: 128,
      });
      if (!password) return;
      if (await guarded(() => api(`users/${encodeURIComponent(id)}/password`, { method: "POST", body: { password } }), "密码已更新")) {
        await reloadUsers().then(renderUsersView);
      }
      return;
    }
    case "delete-user": {
      const ok = await confirmDialog({
        title: "删除用户？", danger: true, confirm: "删除",
        message: `将吊销 <b>${esc(id)}</b> 的登录凭据并把该保险库从内存移除。磁盘上的存档数据会保留，重建同名用户即可恢复。`,
      });
      if (!ok) return;
      if (await guarded(() => api(`users/${encodeURIComponent(id)}`, { method: "DELETE" }), "用户已删除")) {
        await reloadUsers().then(renderUsersView);
      }
      return;
    }
    case "revoke-token": {
      const token = tokenByID(id);
      const ok = await confirmDialog({
        title: "吊销设备令牌？", danger: true, confirm: "吊销",
        message: `将立即吊销 <b>${esc(token?.name || token?.display_name || "未命名")}</b>。使用该令牌的设备会收到 401，必须重新配对或换用新令牌才能继续同步。此操作无法撤销。`,
      });
      if (!ok) return;
      if (await guarded(() => api(`device-tokens/${encodeURIComponent(id)}/revoke`, { method: "POST" }), "设备令牌已吊销")) {
        await reloadTokensPage();
      }
      return;
    }
    case "delete-token": {
      const token = tokenByID(id);
      const ok = await confirmDialog({
        title: "删除令牌记录？", danger: true, confirm: "删除",
        message: `将永久删除 <b>${esc(token?.name || token?.display_name || "未命名")}</b> 的已吊销记录，审计信息也会一并移除。此操作无法撤销。`,
      });
      if (!ok) return;
      if (await guarded(() => api(`device-tokens/${encodeURIComponent(id)}`, { method: "DELETE" }), "令牌记录已删除")) {
        await reloadTokensPage();
      }
      return;
    }
    case "cancel-pairing": {
      if (await guarded(() => api(`pairings/${encodeURIComponent(id)}`, { method: "DELETE" }), "配对码已取消")) {
        await reloadTokensPage();
      }
      return;
    }
    case "set-shared-mode": {
      const mode = target.dataset.mode;
      if (!sharedTokenModes[mode] || store.deviceTokens?.shared_token_mode === mode) return;
      if (mode === "deny") {
        const devices = store.deviceTokens?.shared_token_devices || [];
        const message = devices.length
          ? `仍在使用共享令牌的设备：${devices.map((device) => `<b>${esc(device.name || "未命名设备")}</b>`).join("、")}。切换后共享令牌会被立即拒绝，这些设备将收到 401，需要先改用设备令牌。确认继续？`
          : "确认所有客户端都已改用设备令牌。切换后共享令牌会被立即拒绝，仍在使用共享令牌的客户端将收到 401，需要改用设备令牌。";
        const ok = await confirmDialog({ title: "禁用共享令牌？", danger: true, confirm: "切换为禁用", message });
        if (!ok) return;
      }
      if (await guarded(() => api("auth-settings", { method: "PUT", body: { shared_token_mode: mode } }), `共享令牌阶段已切换为${sharedTokenModeLabel(mode)}`)) {
        await reloadTokensPage();
      }
      return;
    }
    case "rename-device": {
      const device = deviceByID(id);
      const alias = await promptDialog({
        title: "重命名设备", label: "显示名称", value: device?.alias || "", placeholder: device?.name || "设备名称",
        hint: "留空则使用客户端上报的名称。",
      });
      if (alias === null) return;
      if (await guarded(() => api(`devices/${encodeURIComponent(id)}`, { method: "PATCH", body: { alias } }), "已更新设备名称")) await refresh();
      return;
    }
    case "delete-device": {
      const ok = await confirmDialog({
        title: "移除设备记录？", danger: true, confirm: "移除",
        message: "会同时吊销绑定到这台设备的全部设备令牌；使用这些令牌的客户端会收到 401，需要重新配对才能继续同步。仍在使用共享令牌的设备只删除记录，下次连接时会重新出现。",
      });
      if (!ok) return;
      if (await guarded(() => api(`devices/${encodeURIComponent(id)}`, { method: "DELETE" }), "设备记录已移除")) await refresh();
      return;
    }
    case "prune-preview":
    case "prune-run": {
      const dry = action === "prune-preview";
      if (!dry) {
        const ok = await confirmDialog({
          title: "立即清理存储？", danger: true, confirm: "清理",
          message: "将按保留策略删除过期快照并回收未引用的对象。建议先预览。",
        });
        if (!ok) return;
      }
      target.disabled = true;
      const report = await guarded(() => api(`maintenance/prune${dry ? "?dry_run=1" : ""}`, { method: "POST" }));
      target.disabled = false;
      if (report) {
        store.pruneReport = report;
        if (!dry) {
          clearSaveVaultCaches();
          store.saveVaultFiles = null;
          store.saveVaultRetention = null;
          await refresh();
        } else if (currentRoute().name === "maintenance") {
          renderMaintenance();
        }
      }
      return;
    }
    case "restore-tombstone": {
      const type = target.dataset.type;
      const game = target.dataset.game;
      const ok = await confirmDialog({
        title: "恢复这个范围？", confirm: "恢复",
        message: "会把回收区里的归档和版本记录移回原处，并去掉删除标记。恢复后设备可以继续同步。",
      });
      if (!ok) return;
      const done = await guarded(() => api(
        `savevault/scopes/${encodeURIComponent(type)}/${encodeURIComponent(game)}/restore`,
        { method: "POST" }), "存档范围已恢复");
      if (done) {
        clearSaveVaultCaches();
        await refresh();
      }
      return;
    }
    case "clear-tombstone": {
      const type = target.dataset.type;
      const game = target.dataset.game;
      const ok = await confirmDialog({
        title: "解除删除标记？", danger: true, confirm: "解除标记",
        message: "解除后设备可以重新上传这个游戏，作为新存档从头开始。如果回收区里还有数据，也会一并清除；此操作无法撤销。",
      });
      if (!ok) return;
      if (await guarded(() => api(
        `savevault/tombstones/${encodeURIComponent(type)}/${encodeURIComponent(game)}`,
        { method: "DELETE" }), "已解除删除标记")) {
        clearSaveVaultCaches();
        await refresh();
      }
      return;
    }
    case "unretire-tombstone": {
      const type = target.dataset.type;
      const game = target.dataset.game;
      const ok = await confirmDialog({
        title: "取消合并？", danger: true, confirm: "取消合并",
        message: "取消后这个范围恢复可写。已经清掉已确认头的设备下次同步时会作为冲突副本上传，不会覆盖云端当前版本。",
      });
      if (!ok) return;
      if (await guarded(() => api(
        `savevault/scopes/${encodeURIComponent(type)}/${encodeURIComponent(game)}/unretire`,
        { method: "POST" }), "已取消合并")) {
        clearSaveVaultCaches();
        await refresh();
      }
      return;
    }
    case "delete-recreated-tombstone": {
      const type = target.dataset.type;
      const game = target.dataset.game;
      const detail = await guarded(() => api(
        `savevault/scopes/${encodeURIComponent(type)}/${encodeURIComponent(game)}`));
      if (detail) showSaveVaultDeleteDialog(detail);
      return;
    }
    case "verify": {
      target.disabled = true;
      target.innerHTML = `${icon("shield")}校验中…`;
      const report = await guarded(() => api("maintenance/verify", { method: "POST" }));
      if (report) {
        store.verifyReport = report;
        if (currentRoute().name === "maintenance") renderMaintenance();
      } else { target.disabled = false; target.innerHTML = `${icon("shield")}开始校验`; }
      return;
    }
  }
}

// ---------- theme ----------

function applyTheme(theme) {
  if (theme) document.documentElement.dataset.theme = theme;
  else delete document.documentElement.dataset.theme;
  const dark = theme ? theme === "dark" : matchMedia("(prefers-color-scheme: dark)").matches;
  document.querySelectorAll('[data-action="theme"]').forEach((button) => {
    button.innerHTML = icon(dark ? "sun" : "moon");
    button.title = dark ? "切换浅色主题" : "切换深色主题";
    button.setAttribute("aria-label", button.title);
  });
}

function toggleTheme() {
  const current = document.documentElement.dataset.theme
    || (matchMedia("(prefers-color-scheme: dark)").matches ? "dark" : "light");
  const next = current === "dark" ? "light" : "dark";
  storageSet("game-save-theme", next);
  applyTheme(next);
}

// ---------- session ----------

function showLogin() {
  if (activeModalClose) activeModalClose();
  store.overview = null;
  store.overviewSaveVaultGames = null;
  store.scopeDetail = null;
  store.events = null;
  store.saveVaultFiles = null;
  store.saveVaultScopes = null;
  store.saveVaultGames = null;
  store.saveVaultScopeDetail = null;
  store.saveVaultRetention = null;
  store.saveVaultTombstones = null;
  store.saveVaultRetentionPreview = null;
  store.metaSettings = null;
  store.deviceTokens = null;
  store.pairings = null;
  store.users = null;
  store.pruneReport = null;
  store.verifyReport = null;
  store.savesQuery = "";
  store.saveVaultQuery = "";
  store.saveVaultType = "all";
  store.activityFilter = "all";
  $("#nav-saves").textContent = "";
  $("#nav-savevault").textContent = "";
  $("#nav-devices").textContent = "";
  main.innerHTML = "";
  const modalRoot = $("#modal-root");
  if (modalRoot) {
    modalRoot.innerHTML = "";
    modalRoot.hidden = true;
  }
  $("#app").hidden = true;
  $("#login").hidden = false;
  const tokenInput = $("#login-token");
  tokenInput.type = "password";
  tokenInput.value = "";
  const toggleButton = $("#login-token-toggle");
  if (toggleButton) {
    toggleButton.innerHTML = icon("eye");
    toggleButton.title = "显示令牌";
    toggleButton.setAttribute("aria-label", "显示令牌");
  }
  tokenInput.focus();
}

async function showApp() {
  $("#login").hidden = true;
  $("#app").hidden = false;
  main.innerHTML = `<div class="skeleton-stack">
    <div class="skeleton-kpis"><div class="skeleton"></div><div class="skeleton"></div><div class="skeleton"></div><div class="skeleton"></div></div>
    <div class="skeleton skeleton-panel"></div>
  </div>`;
  await refresh();
}

const loginTokenToggle = $("#login-token-toggle");
if (loginTokenToggle) {
  loginTokenToggle.addEventListener("click", () => {
    const input = $("#login-token");
    const visible = input.type === "text";
    input.type = visible ? "password" : "text";
    loginTokenToggle.innerHTML = icon(visible ? "eye" : "eye-off");
    loginTokenToggle.title = visible ? "显示令牌" : "隐藏令牌";
    loginTokenToggle.setAttribute("aria-label", loginTokenToggle.title);
    input.focus();
  });
}

$("#login-form").addEventListener("submit", async (event) => {
  event.preventDefault();
  const error = $("#login-error");
  const button = event.target.querySelector('button[type="submit"]');
  error.textContent = "";
  button.disabled = true;
  try {
    const response = await fetch("api/login", {
      method: "POST",
      headers: { "Content-Type": "application/json", "X-Game-Save-Admin": "1" },
      body: JSON.stringify({ token: $("#login-token").value }),
    });
    if (!response.ok) {
      let message = "登录失败";
      try { message = translateError((await response.json()).error); } catch { /* ignore */ }
      error.textContent = message;
      return;
    }
    await showApp();
  } catch {
    error.textContent = "无法连接到服务";
  } finally {
    button.disabled = false;
  }
});

document.addEventListener("click", onAction);
document.addEventListener("keydown", (event) => {
  if (event.key !== "/" || event.metaKey || event.ctrlKey || event.altKey) return;
  if ($("#app").hidden || !$("#modal-root").hidden) return;
  const active = document.activeElement;
  if (active && active.matches("input, textarea, select, [contenteditable]")) return;
  const search = $(".search input", main);
  if (!search) return;
  event.preventDefault();
  search.focus();
  search.select();
});
window.addEventListener("hashchange", () => {
  if (!$("#app").hidden) render().catch((error) => {
    if (error instanceof UnauthorizedError) showLogin();
    else toast(error.message, true);
  });
  main.focus({ preventScroll: true });
  window.scrollTo(0, 0);
});
setInterval(() => {
  if (!document.hidden && !$("#app").hidden && $("#modal-root").hidden) refresh({ quiet: true });
}, 20000);
document.addEventListener("visibilitychange", () => {
  if (!document.hidden && !$("#app").hidden && $("#modal-root").hidden) refresh({ quiet: true });
});

// Inline handlers are blocked by the CSP, so broken cover images are removed here.
document.addEventListener("error", (event) => {
  if (event.target instanceof HTMLImageElement && event.target.closest(".cover")) event.target.remove();
}, true);

applyTheme(storageGet("game-save-theme"));
(async () => {
  try {
    const response = await fetch("api/session", { credentials: "same-origin" });
    const session = await response.json();
    if (session.authenticated) await showApp();
    else showLogin();
  } catch {
    showLogin();
  }
})();

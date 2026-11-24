<script>
  import { slidingIndicator } from '../lib/motion.js';
  import { onMount, onDestroy } from 'svelte';
  import { settings, toast, askConfirm, gameList, navigate } from '../lib/stores.js';
  import { showSetupAgain } from '../lib/setup.js';
  import { api, native } from '../lib/api.js';
  import { adoptOutsideChanges, changedFields, createAutosave } from '../lib/autosave.js';
  import { t, tt, trError, segments } from '../lib/i18n.js';
  import LibraryViewOptions from './home/LibraryViewOptions.svelte';
  import Skeleton from '../components/ui/Skeleton.svelte';
  import MoreInfo from '../components/ui/MoreInfo.svelte';
  import AppearanceOptions from './settings/AppearanceOptions.svelte';
  import ControllerOptions from './settings/ControllerOptions.svelte';
  import NotificationOptions from './settings/NotificationOptions.svelte';
  import StorageUsage from './settings/StorageUsage.svelte';
  import SnapshotChecks from './settings/SnapshotChecks.svelte';
  import FileCheck from 'lucide-svelte/icons/file-check';
  import PieChart from 'lucide-svelte/icons/chart-pie';
  import Bell from 'lucide-svelte/icons/bell';
  import Palette from 'lucide-svelte/icons/palette';
  import Monitor from 'lucide-svelte/icons/monitor';
  import LayoutGrid from 'lucide-svelte/icons/layout-grid';
  import Rocket from 'lucide-svelte/icons/rocket';
  import Download from 'lucide-svelte/icons/download';
  import RefreshCw from 'lucide-svelte/icons/refresh-cw';
  import Globe from 'lucide-svelte/icons/globe';
  import RadioTower from 'lucide-svelte/icons/radio-tower';
  import Cloud from 'lucide-svelte/icons/cloud';
  import HardDrive from 'lucide-svelte/icons/hard-drive';
  import Timer from 'lucide-svelte/icons/timer';
  import History from 'lucide-svelte/icons/history';
  import BrushCleaning from 'lucide-svelte/icons/brush-cleaning';
  import ScanSearch from 'lucide-svelte/icons/scan-search';
  import RotateCcw from 'lucide-svelte/icons/rotate-ccw';
  import Network from 'lucide-svelte/icons/network';
  import ArrowLeftRight from 'lucide-svelte/icons/arrow-left-right';
  import X from 'lucide-svelte/icons/x';
  import Plus from 'lucide-svelte/icons/plus';
  import Gamepad2 from 'lucide-svelte/icons/gamepad-2';
  import ArrowRight from 'lucide-svelte/icons/arrow-right';
  import Check from 'lucide-svelte/icons/check';
  import LoaderCircle from 'lucide-svelte/icons/loader-circle';
  import TriangleAlert from 'lucide-svelte/icons/triangle-alert';

  /** From navigate('settings', {tab}). */
  export let params = {};
  const TABS = ['general', 'sync', 'storage', 'advanced'];
  let tab = TABS.includes(params?.tab) ? params.tab : 'general';
  let pruning = false;

  // The running build, so the updates toggle can explain what it means for
  // THIS install: someone already on a beta is offered betas regardless, and
  // saying so is the difference between the checkbox looking broken and
  // looking deliberate.
  let appVersion = '';
  $: onPreRelease = appVersion.includes('-');
  onMount(async () => {
    try {
      appVersion = (await native.appInfo())?.version ?? '';
    } catch {
      appVersion = '';
    }
  });

  // Every change saves itself (see lib/autosave.js): a toggle or a choice at
  // once, typed text when you leave the box or press Enter. `draft` is what
  // the page shows and edits; `saved` is what the daemon last confirmed.
  let draft = null;
  let saved = null;
  let saveState = null; // null | 'saving' | 'saved' | {error}
  // Values the daemon refused, and why, by field: left out of later saves
  // until changed (lib/autosave.js), and shown until then — a later save
  // succeeding does not mean this one did.
  let rejected = {};
  let refusedWhy = {};
  const FIELD_NAMES = {
    get relayUrl() { return tt('settings.field.relayUrl'); },
    get relayPort() { return tt('settings.field.relayPort'); },
    get port() { return tt('settings.field.port'); },
    get deviceName() { return tt('settings.field.deviceName'); },
    get backupsDir() { return tt('settings.field.backupsDir'); },
    get syncBackupsDir() { return tt('settings.field.syncBackupsDir'); },
    get pathTranslations() { return tt('settings.field.pathTranslations'); }
  };
  $: unsaved = draft ? Object.keys(rejected).filter((key) => same(draft[key], rejected[key])) : [];
  const firstSentence = (text) => String(text).split(/(?<=\.)\s/)[0];

  // The settings as this page edits them. The cloud settings are set on the
  // Cloud Backup page and left out: they are not this page's to send.
  function editable(s) {
    const { cloudSync, cloudAutoPull, ...rest } = structuredClone(s);
    // Older daemons predate the separate manual-snapshot budget; 0 is the
    // "keep forever" default, so an omitted value behaves as it should.
    rest.defaultMaxManualSnapshots ??= 0;
    // Older daemons predate the update channel; stable is the default.
    rest.updateChannel ??= 'stable';
    return rest;
  }

  // A path rule with a side still empty is not a rule yet: it stays on the
  // page but is not saved until both sides are filled in.
  const ready = (d) => ({
    ...d,
    pathTranslations: (d.pathTranslations ?? []).filter((r) => r.fromPattern?.trim() && r.toPattern?.trim())
  });

  const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);

  const saver = createAutosave({
    collect: () => changedFields(saved, ready(draft), { rejected }),
    send: (patch) => api.post('/api/settings', patch),
    onSaved(result, patch) {
      const next = editable(result);
      // The daemon's version of what was sent — it may have tidied it — unless
      // the field has been changed again in the meantime.
      for (const key of Object.keys(patch)) {
        if (same(draft[key], patch[key])) draft[key] = structuredClone(next[key]);
        delete rejected[key];
        delete refusedWhy[key];
      }
      rejected = rejected;
      saved = next;
      settings.set(result);
    },
    onFailed(patch, error) {
      rejected = { ...rejected, ...patch };
      for (const key of Object.keys(patch)) refusedWhy[key] = error.message;
    },
    onState: (state) => (saveState = state)
  });
  const saveNow = () => saver.flush();
  onDestroy(saveNow);

  // Arrivals from elsewhere while the page is open — the tray, the terminal,
  // another screen — are taken in, except into a field being edited here.
  function takeSettings(s) {
    if (!s) return;
    if (!draft) {
      saved = editable(s);
      draft = structuredClone(saved);
      return;
    }
    ({ saved, draft } = adoptOutsideChanges(saved, draft, editable(s)));
  }
  $: takeSettings($settings);

  function showTab(next) {
    saveNow();
    tab = next;
  }

  async function cleanUpSnapshots() {
    pruning = true;
    try {
      // Save the limit first so the cleanup uses it, then prune everything.
      await saver.flush();
      if (saveState?.error) throw new Error(tt('settings.storage.limitsNotSaved', { error: saveState.error }));
      const res = await api.post('/api/snapshots/prune', { applyDefaultToAll: true });
      const mb = (res.freedBytes / 1048576).toFixed(1);
      toast(
        res.removed > 0
          ? tt('settings.storage.pruned', { n: res.removed, mb })
          : tt('settings.storage.nothingToPrune'),
        'success'
      );
    } catch (e) {
      toast(trError(e.message), 'error');
    } finally {
      pruning = false;
    }
  }

  // Path translations editor
  function addRule() {
    draft.pathTranslations = [...(draft.pathTranslations ?? []), { fromPattern: '', toPattern: '' }];
  }
  function removeRule(i) {
    draft.pathTranslations = draft.pathTranslations.filter((_, idx) => idx !== i);
    saveNow();
  }

  // Custom scan paths
  async function addScanPath() {
    const dir = await native.selectDirectory(tt('settings.scanner.addFolderTitle'));
    if (dir) draft.customScanPaths = [...(draft.customScanPaths ?? []), dir];
    saveNow();
  }
  function removeScanPath(i) {
    draft.customScanPaths = draft.customScanPaths.filter((_, idx) => idx !== i);
    saveNow();
  }

  // Excluded folders — locations the scan should skip entirely.
  async function addExcludePath() {
    const dir = await native.selectDirectory(tt('settings.scanner.excludeFolderTitle'));
    if (dir) draft.excludePaths = [...(draft.excludePaths ?? []), dir];
    saveNow();
  }
  function removeExcludePath(i) {
    draft.excludePaths = draft.excludePaths.filter((_, idx) => idx !== i);
    saveNow();
  }

  // Reset tracking — untrack every game so the user can re-add them from the
  // right locations (e.g. after moving games between launchers or drives).
  // Non-destructive: this only clears the tracked list; snapshot backups on
  // disk are kept.
  let resetting = false;
  async function resetTracking() {
    const n = $gameList.length;
    if (n === 0) return;
    const ok = await askConfirm(
      tt('settings.reset.confirm', { n }),
      { title: tt('settings.reset.confirmTitle'), confirmText: tt('settings.reset.confirmButton', { count: n }), danger: true }
    );
    if (!ok) return;
    resetting = true;
    try {
      const res = await api.post('/api/games/untrack-bulk', { all: true });
      toast(tt('settings.reset.done', { n: res.untracked }), 'success');
    } catch (e) {
      toast(trError(e.message), 'error');
    } finally {
      resetting = false;
    }
  }

  async function pickBackupsDir() {
    const dir = await native.selectDirectory(tt('settings.storage.pickBackupsDir'));
    if (dir) draft.backupsDir = dir;
    saveNow();
  }

  async function pickSyncBackupsDir() {
    const dir = await native.selectDirectory(tt('settings.storage.pickSyncBackupsDir'));
    if (dir) draft.syncBackupsDir = dir;
    saveNow();
  }

  // Relay hosting: LAN IPs / public IP to share with friends. Shown only on
  // request — these identify the machine, so they shouldn't appear on screen
  // just because the checkbox was ticked.
  let relayInfo = null;
  let relayInfoShown = false;
  let relayInfoLoading = false;

  async function toggleRelayInfo() {
    if (relayInfoShown) {
      relayInfoShown = false;
      return;
    }
    relayInfoLoading = true;
    try {
      // Re-fetch each time: the public IP changes, and a stale one sent to a
      // friend is worse than none.
      relayInfo = await api.get('/api/relay/ips');
      relayInfoShown = true;
    } catch (e) {
      toast(trError(e.message), 'error');
    } finally {
      relayInfoLoading = false;
    }
  }

  // Turning hosting off should also hide addresses left on screen.
  $: if (draft && !draft.hostRelay && relayInfoShown) relayInfoShown = false;
</script>

<div class="head">
  <h2 class="page-title">{$t('settings.title')}</h2>
  {#if draft}
    <span class="save-state" class:error={saveState?.error || unsaved.length} aria-live="polite">
      {#if saveState === 'saving'}
        <LoaderCircle size={14} class="spin" />{$t('settings.saving')}
      {:else if unsaved.length}
        <TriangleAlert size={14} />
        <span class="why" title={trError(refusedWhy[unsaved[0]])}>
          {$t('settings.notSaved', { field: FIELD_NAMES[unsaved[0]] ?? unsaved[0], why: trError(firstSentence(refusedWhy[unsaved[0]])) })}
        </span>
      {:else if saveState?.error}
        <TriangleAlert size={14} /><span class="why" title={trError(saveState.error)}>{$t('settings.couldntSave', { error: trError(firstSentence(saveState.error)) })}</span>
      {:else if saveState === 'saved'}
        <Check size={14} />{$t('settings.saved')}
      {:else}
        {$t('settings.autosaveHint')}
      {/if}
    </span>
  {/if}
</div>

{#if !draft}
  <Skeleton kind="cards" count={3} />
{:else}
  <div class="pill-tabs" style="margin-bottom: 18px;" use:slidingIndicator={{ inset: 10 }}>
    <button class:active={tab === 'general'} on:click={() => showTab('general')}>{$t('settings.tabs.general')}</button>
    <button class:active={tab === 'sync'} on:click={() => showTab('sync')}>{$t('settings.tabs.sync')}</button>
    <button class:active={tab === 'storage'} on:click={() => showTab('storage')}>{$t('settings.tabs.storage')}</button>
    <button class:active={tab === 'advanced'} on:click={() => showTab('advanced')}>{$t('settings.tabs.advanced')}</button>
  </div>

  <!-- A toggle or a choice commits at once; typed text when the box is left
       or Enter is pressed. See lib/autosave.js. -->
  <div
    class="tab-body"
    on:change={saveNow}
    on:keydown={(e) => e.key === 'Enter' && e.target.matches('input') && saveNow()}
    role="presentation"
  >
  {#if tab === 'general'}
    <div class="card">
      <h3 class="section-title with-icon"><Monitor size={17} />{$t('settings.device.title')}</h3>
      <div class="field">
        <label for="s-name">{$t('settings.device.name')}</label>
        <input id="s-name" class:invalid={unsaved.includes('deviceName')} bind:value={draft.deviceName} />
      </div>
      <div class="field">
        <label for="s-type">{$t('settings.device.type')}</label>
        <select id="s-type" bind:value={draft.deviceType}>
          <option value="desktop">{$t('settings.device.typeDesktop')}</option>
          <option value="deck">{$t('settings.device.typeDeck')}</option>
          <option value="handheld">{$t('settings.device.typeHandheld')}</option>
          <option value="mobile">{$t('settings.device.typeMobile')}</option>
        </select>
        <span class="hint">{$t('settings.device.typeHint')}</span>
      </div>
      <div class="field">
        <label for="s-node">{$t('settings.device.id')}</label>
        <input id="s-node" value={draft.nodeId ?? ''} readonly class="mono" />
        <span class="hint">{$t('settings.device.idHint')}</span>
      </div>
    </div>

    <!-- Kept on this device rather than in the daemon's settings, like the
         library view below. -->
    <div class="card" style="margin-top: 14px;">
      <h3 class="section-title with-icon"><Palette size={17} />{$t('settings.appearance.title')}</h3>
      <AppearanceOptions />
    </div>

    <!-- Using SaveVault from a gamepad: a Steam Deck, a handheld, a pad on a
         PC. Also on this device only. -->
    <div class="card" style="margin-top: 14px;">
      <h3 class="section-title with-icon"><Gamepad2 size={17} />{$t('settings.controller.title')}</h3>
      <ControllerOptions />
    </div>

    <div class="card" style="margin-top: 14px;">
      <h3 class="section-title with-icon"><Bell size={17} />{$t('settings.notifications.title')}</h3>
      <NotificationOptions />
    </div>

    <!-- The same thing the View menu on the library changes, kept on this
         device: see lib/libraryview.js for why. -->
    <div class="card" style="margin-top: 14px;">
      <h3 class="section-title with-icon"><LayoutGrid size={17} />{$t('settings.library.title')}</h3>
      <p class="hint library-hint">{$t('settings.library.hint')}</p>
      <LibraryViewOptions />
      <div class="guide-row">
        <div>
          <strong>{$t('settings.library.setupGuide')}</strong>
          <p class="hint">{$t('settings.library.setupGuideHint')}</p>
        </div>
        <button class="btn small" on:click={() => { showSetupAgain(); navigate('home'); }}>{$t('settings.library.showAgain')}</button>
      </div>
    </div>

    <div class="card" style="margin-top: 14px;">
      <h3 class="section-title with-icon"><Rocket size={17} />{$t('settings.startup.title')}</h3>
      <label class="check">
        <input type="checkbox" bind:checked={draft.startOnBoot} />
        {$t('settings.startup.startOnBoot')}
      </label>
      <p class="hint" style="margin-top: 6px;">
        {$t('settings.startup.hint')}
      </p>
    </div>

    <div class="card" style="margin-top: 14px;">
      <h3 class="section-title with-icon"><Download size={17} />{$t('settings.updates.title')}</h3>
      <label class="check">
        <input
          type="checkbox"
          checked={draft.updateChannel === 'beta'}
          on:change={(e) => (draft.updateChannel = e.currentTarget.checked ? 'beta' : 'stable')}
        />
        {$t('settings.updates.offerBeta')}
      </label>
      <p class="hint" style="margin-top: 6px;">
        {#if onPreRelease}
          {#each segments($t('settings.updates.onBeta'), { version: appVersion }, ['version']) as part}{#if part.bold}<strong>{part.text}</strong>{:else}{part.text}{/if}{/each}
        {:else}
          {$t('settings.updates.betaHint')}
        {/if}
      </p>
    </div>

  {:else if tab === 'sync'}
    <div class="card">
      <h3 class="section-title with-icon"><RefreshCw size={17} />{$t('settings.sync.title')}</h3>
      <label class="check">
        <input type="checkbox" bind:checked={draft.autoSyncOnTrack} />
        {$t('settings.sync.autoSyncOnTrack')}
      </label>
      <label class="check" style="margin-top: 18px;">
        <input type="checkbox" bind:checked={draft.detectNewGames} />
        {$t('settings.sync.detectNewGames')}
      </label>
      <span class="hint" style="margin-top: 6px;">
        {$t('settings.sync.detectNewGamesHint')}
      </span>
      <label class="check" style="margin-top: 18px;">
        <input type="checkbox" bind:checked={draft.matchByAppId} />
        {$t('settings.sync.matchByAppId')}
      </label>
      <span class="hint" style="margin-top: 6px;">
        {$t('settings.sync.matchByAppIdHint')}
        <MoreInfo>
          {$t('settings.sync.matchByAppIdMore')}
        </MoreInfo>
      </span>
        <div class="field" style="margin-top: 18px;">
          <label for="s-unknown-game">{$t('settings.sync.unknownGame')}</label>
          <select id="s-unknown-game" bind:value={draft.unknownGameFromPeer}>
            <option value="track">{$t('settings.sync.unknownGameTrack')}</option>
            <option value="ask">{$t('settings.sync.unknownGameAsk')}</option>
          </select>
          <span class="hint">
            {$t('settings.sync.unknownGameHint')}
            <MoreInfo>
              {#each segments($t('settings.sync.unknownGameMore'), { askMe: $t('settings.sync.askMe') }, ['askMe']) as part}{#if part.bold}<em>{part.text}</em>{:else}{part.text}{/if}{/each}
            </MoreInfo>
          </span>
        </div>
      <div class="field" style="margin-top: 14px;">
        <label for="s-limit">{$t('settings.sync.speedLimit')}</label>
        <select id="s-limit" bind:value={draft.speedLimit}>
          <option value={0}>{$t('settings.sync.speedUnlimited')}</option>
          <option value={100}>{$t('settings.sync.speed100')}</option>
          <option value={500}>{$t('settings.sync.speed500')}</option>
          <option value={1024}>{$t('settings.sync.speed1024')}</option>
          <option value={5120}>{$t('settings.sync.speed5120')}</option>
          <option value={10240}>{$t('settings.sync.speed10240')}</option>
        </select>
        <span class="hint">{$t('settings.sync.speedLimitHint')}</span>
      </div>
    </div>

    <div class="card" style="margin-top: 14px;">
      <h3 class="section-title with-icon"><Globe size={17} />{$t('settings.relay.title')}</h3>
      <div class="field">
        <label for="s-relay-url">{$t('settings.relay.url')}</label>
        <input id="s-relay-url" class:invalid={unsaved.includes('relayUrl')} disabled={$settings?.relayUrlLocked} bind:value={draft.relayUrl} placeholder="wss://relay.example.com" />
        <span class="hint">{#each segments($t('settings.relay.urlHint'), { internetSync: $t('settings.relay.internetSync') }, ['internetSync']) as part}{#if part.bold}<strong>{part.text}</strong>{:else}{part.text}{/if}{/each}</span>
      </div>
      <label class="check">
        <input type="checkbox" bind:checked={draft.hostRelay} />
        {$t('settings.relay.host')}
      </label>
      <p class="hint" style="margin-top: 6px;">
        {$t('settings.relay.hostHint')}
      </p>
      {#if draft.hostRelay}
        <div class="field" style="margin-top: 12px;">
          <label for="s-relay-port">{$t('settings.relay.port')}</label>
          <input id="s-relay-port" type="number" class:invalid={unsaved.includes('relayPort')} bind:value={draft.relayPort} />
          <span class="hint">{$t('settings.relay.portHint')}</span>
        </div>
        <button class="btn small" on:click={toggleRelayInfo} disabled={relayInfoLoading}>
          {#if relayInfoLoading}
            {$t('settings.relay.lookingUp')}
          {:else if relayInfoShown}
            {$t('settings.relay.hideAddresses')}
          {:else}
            {$t('settings.relay.showAddresses')}
          {/if}
        </button>
        {#if relayInfoShown && relayInfo}
          <div class="share-banner">
            <div class="share-title with-icon"><RadioTower size={15} />{$t('settings.relay.shareTitle')}</div>
            <div class="share-row"><span>{$t('settings.relay.lanIps')}</span> {relayInfo.lanIps?.join(', ') || '—'}</div>
            <div class="share-row"><span>{$t('settings.relay.publicIp')}</span> {relayInfo.publicIp || $t('settings.relay.unavailable')}</div>
            <div class="share-row"><span>{$t('settings.relay.relayPort')}</span> {relayInfo.relayPort}</div>
          </div>
        {/if}
      {/if}
    </div>

    <div class="card moved" style="margin-top: 14px;">
      <div>
        <h3 class="section-title with-icon"><Cloud size={17} />{$t('settings.cloud.title')}</h3>
        <p class="hint">
          {$t('settings.cloud.hint')}
        </p>
      </div>
      <button class="btn" on:click={() => navigate('cloud')}>{$t('settings.cloud.open')}</button>
    </div>
  {:else if tab === 'storage'}
    <div class="card">
      <h3 class="section-title with-icon"><PieChart size={17} />{$t('settings.storage.spaceUsed')}</h3>
      <StorageUsage />
    </div>

    <div class="card" style="margin-top: 14px;">
      <h3 class="section-title with-icon"><FileCheck size={17} />{$t('settings.storage.canRestore')}</h3>
      <SnapshotChecks />
    </div>

    <div class="card" style="margin-top: 14px;">
      <h3 class="section-title with-icon"><HardDrive size={17} />{$t('settings.storage.title')}</h3>
      <div class="field">
        <label for="s-backups">{$t('settings.storage.backupsDir')}</label>
        <div class="path-row">
          <input id="s-backups" bind:value={draft.backupsDir} />
          <button class="btn" on:click={pickBackupsDir}>{$t('settings.browse')}</button>
        </div>
        <span class="hint">{$t('settings.storage.backupsDirHint')}</span>
      </div>
      <div class="field">
        <label for="s-sync-backups">{$t('settings.storage.syncBackupsDir')}</label>
        <div class="path-row">
          <input id="s-sync-backups" bind:value={draft.syncBackupsDir} placeholder={$t('settings.storage.syncBackupsDirDefault', { path: '~/.savevault/backups' })} />
          <button class="btn" on:click={pickSyncBackupsDir}>{$t('settings.browse')}</button>
        </div>
        <span class="hint">{$t('settings.storage.syncBackupsDirHint')}</span>
      </div>
    </div>

    <div class="card" style="margin-top: 14px;">
      <h3 class="section-title with-icon"><Timer size={17} />{$t('settings.storage.retention')}</h3>
      <label class="check">
        <input type="checkbox" bind:checked={draft.autoDeleteBackups} />
        {$t('settings.storage.autoDelete')}
      </label>
      <span class="hint">
        {$t('settings.storage.autoDeleteHint')}
      </span>
      {#if draft.autoDeleteBackups}
        <div class="field" style="margin-top: 10px;">
          <label for="s-days">{$t('settings.storage.deleteOlderThan')}</label>
          <select id="s-days" bind:value={draft.autoDeleteDays}>
            <option value={7}>{$t('settings.storage.days', { n: 7 })}</option>
            <option value={14}>{$t('settings.storage.days', { n: 14 })}</option>
            <option value={30}>{$t('settings.storage.days', { n: 30 })}</option>
            <option value={60}>{$t('settings.storage.days', { n: 60 })}</option>
            <option value={90}>{$t('settings.storage.days', { n: 90 })}</option>
            <option value={180}>{$t('settings.storage.days', { n: 180 })}</option>
          </select>
        </div>
      {/if}
    </div>

    <div class="card" style="margin-top: 14px;">
      <h3 class="section-title with-icon"><History size={17} />{$t('settings.storage.history')}</h3>
      <div class="field">
        <label for="s-max-snaps">{$t('settings.storage.maxSnapshots')}</label>
        <input id="s-max-snaps" type="number" min="0" style="max-width: 120px;" bind:value={draft.defaultMaxSnapshots} />
        <span class="hint">
          {$t('settings.storage.maxSnapshotsHint')}
        </span>
      </div>
      <div class="field">
        <label for="s-max-manual-snaps">{$t('settings.storage.maxManualSnapshots')}</label>
        <input id="s-max-manual-snaps" type="number" min="0" style="max-width: 120px;" bind:value={draft.defaultMaxManualSnapshots} />
        <span class="hint">
          {#each segments($t('settings.storage.maxManualSnapshotsHint'), { keepForever: $t('settings.storage.keepForever') }, ['keepForever']) as part}{#if part.bold}<strong>{part.text}</strong>{:else}{part.text}{/if}{/each}
        </span>
      </div>
      <div class="field" style="margin-bottom: 0;">
        <div>
          <button class="btn small" disabled={pruning} on:click={cleanUpSnapshots}>
            <BrushCleaning size={14} />{pruning ? $t('settings.storage.applying') : $t('settings.storage.applyLimits')}
          </button>
        </div>
        <span class="hint">
          {$t('settings.storage.applyLimitsHint')}
        </span>
      </div>
    </div>

    <div class="card" style="margin-top: 14px;">
      <h3 class="section-title with-icon"><ScanSearch size={17} />{$t('settings.scanner.title')}</h3>
      <div class="field" style="margin-bottom: 0;">
        <label for="s-scan-paths">{$t('settings.scanner.extraFolders')}</label>
        <span class="hint">{$t('settings.scanner.extraFoldersHint')}</span>
        {#each draft.customScanPaths ?? [] as p, i}
          <div class="rule-row">
            <span class="rule-path" title={p}>{p}</span>
            <button class="btn small ghost icon" title={$t('settings.remove')} aria-label={$t('settings.remove')} on:click={() => removeScanPath(i)}><X size={15} /></button>
          </div>
        {/each}
        <button id="s-scan-paths" class="btn small" on:click={addScanPath}><Plus size={14} />{$t('settings.scanner.addFolder')}</button>
      </div>

      <div class="field" style="margin: 18px 0 0;">
        <label for="s-exclude-paths">{$t('settings.scanner.exclude')}</label>
        <span class="hint">{$t('settings.scanner.excludeHint')}</span>
        {#each draft.excludePaths ?? [] as p, i}
          <div class="rule-row">
            <span class="rule-path" title={p}>{p}</span>
            <button class="btn small ghost icon" title={$t('settings.remove')} aria-label={$t('settings.remove')} on:click={() => removeExcludePath(i)}><X size={15} /></button>
          </div>
        {/each}
        <button id="s-exclude-paths" class="btn small" on:click={addExcludePath}><Plus size={14} />{$t('settings.scanner.excludeFolder')}</button>
      </div>
    </div>

    <div class="card" style="margin-top: 14px;">
      <h3 class="section-title with-icon"><RotateCcw size={17} />{$t('settings.reset.title')}</h3>
      <div class="field" style="margin-bottom: 0;">
        <span class="hint">{$t('settings.reset.hint')}</span>
        <button
          class="btn small danger"
          style="margin-top: 12px; width: fit-content; align-self: flex-start;"
          on:click={resetTracking}
          disabled={resetting || $gameList.length === 0}
        >
          {#if resetting}{$t('settings.reset.untracking')}{:else if $gameList.length === 0}{$t('settings.reset.noGames')}{:else}{$t('settings.reset.untrackAll', { n: $gameList.length })}{/if}
        </button>
      </div>
    </div>
  {:else if tab === 'advanced'}
    <div class="card">
      <h3 class="section-title with-icon"><Network size={17} />{$t('settings.network.title')}</h3>
      <div class="field" style="margin-bottom: 0;">
        <label for="s-port">{$t('settings.network.port')}</label>
        <input id="s-port" type="number" class:invalid={unsaved.includes('port')} bind:value={draft.port} />
        <span class="hint">{$t('settings.network.portHint')}</span>
      </div>
    </div>

    <div class="card" style="margin-top: 14px;">
      <h3 class="section-title with-icon"><ArrowLeftRight size={17} />{$t('settings.paths.title')}</h3>
      <div class="field" style="margin-bottom: 0;">
        <span class="hint">
          {$t('settings.paths.hint', { from: 'C:\\Users\\me\\Saves', to: '/home/deck/saves' })}
        </span>
        {#each draft.pathTranslations ?? [] as rule, i}
          <div class="rule-row">
            <input placeholder={$t('settings.paths.fromPattern')} bind:value={rule.fromPattern} />
            <span class="arrow"><ArrowRight size={15} /></span>
            <input placeholder={$t('settings.paths.toPattern')} bind:value={rule.toPattern} />
            <button class="btn small ghost icon" title={$t('settings.remove')} aria-label={$t('settings.remove')} on:click={() => removeRule(i)}><X size={15} /></button>
          </div>
        {/each}
        <button id="s-rules" class="btn small" on:click={addRule}><Plus size={14} />{$t('settings.paths.addRule')}</button>
      </div>
    </div>
  {/if}

  </div>
{/if}

<style>
  .head {
    display: flex;
    align-items: baseline;
    justify-content: space-between;
    gap: 16px;
    margin-bottom: 18px;
  }
  .save-state {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    font-size: 0.8rem;
    color: var(--text-faint);
    min-width: 0;
  }
  .save-state.error {
    color: var(--danger-text);
  }
  .why {
    white-space: nowrap;
    overflow: hidden;
    text-overflow: ellipsis;
    max-width: min(560px, 60vw);
  }
  input.invalid {
    border-color: var(--danger);
  }
  .save-state :global(.spin) {
    animation: settings-spin 1s linear infinite;
  }
  @keyframes settings-spin {
    to {
      transform: rotate(360deg);
    }
  }
  /* .check and the checkbox itself are styled globally in app.css. */
  .path-row {
    display: flex;
    gap: 8px;
  }
  .path-row input {
    flex: 1;
  }
  .rule-row {
    display: flex;
    align-items: center;
    gap: 8px;
    margin-bottom: 8px;
  }
  .rule-row input {
    flex: 1;
    padding: 7px 10px;
    background: var(--bg);
    border: 1px solid var(--border-strong);
    border-radius: 8px;
    color: var(--text);
    font-size: 0.85rem;
    outline: none;
  }
  .rule-path {
    flex: 1;
    font-size: 0.83rem;
    color: var(--text-dim);
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }
  .arrow {
    color: var(--text-faint);
  }
  /* A pointer to where the cloud settings moved, rather than a gap where
     they were. */
  .moved {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 16px;
  }
  .moved .hint {
    font-size: 0.82rem;
    color: var(--text-faint);
    line-height: 1.5;
  }
  .guide-row {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 16px;
    margin-top: 18px;
    padding-top: 14px;
    border-top: 1px solid var(--border);
  }
  .guide-row strong {
    font-size: 0.88rem;
  }
  .guide-row .hint {
    margin: 2px 0 0;
  }
  .library-hint {
    font-size: 0.82rem;
    color: var(--text-faint);
    margin: -4px 0 14px;
  }
  .section-title {
    font-size: 0.95rem;
    font-weight: 600;
    margin-bottom: 14px;
    padding-bottom: 10px;
    border-bottom: 1px solid var(--border);
    color: var(--text);
  }
  .mono {
    font-family: ui-monospace, 'Cascadia Code', 'Consolas', monospace;
    font-size: 0.82rem;
    color: var(--text-dim);
  }
  input[readonly] {
    opacity: 0.75;
    cursor: default;
  }
  .share-banner {
    margin-top: 12px;
    background: rgba(var(--accent-rgb), 0.06);
    border: 1px solid rgba(var(--accent-rgb), 0.28);
    border-radius: var(--radius);
    padding: 12px 14px;
    font-size: 0.82rem;
  }
  .share-title {
    font-weight: 600;
    margin-bottom: 6px;
  }
  .share-row {
    color: var(--text-dim);
    margin-top: 3px;
  }
  .share-row span {
    color: var(--text-faint);
    display: inline-block;
    width: 78px;
  }
</style>

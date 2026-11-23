<script>
  import { dialogOut } from '../lib/motion.js';
  import { onMount } from 'svelte';
  import { native } from '../lib/api.js';
  import { navigate } from '../lib/stores.js';
  import { backdropClose } from '../lib/backdrop.js';
  import { GITHUB_URL } from '../lib/links.js';
  import { t } from '../lib/i18n.js';
  import logoUrl from '../assets/logo.png';
  import X from 'lucide-svelte/icons/x';
  import ExternalLink from 'lucide-svelte/icons/external-link';
  import ScrollText from 'lucide-svelte/icons/scroll-text';

  export let onClose = () => {};

  let info = null;
  onMount(async () => {
    try {
      info = await native.appInfo();
    } catch {
      info = { name: 'SaveVault', version: 'dev' };
    }
  });

  // The changelog has its own view now; showing a second, raw copy here was
  // where users met literal markdown.
  function openChangelog() {
    onClose();
    navigate('changelog');
  }

  $: buildLabel =
    info?.buildTime && info.buildTime !== '0' ? new Date(Number(info.buildTime)).toLocaleString() : '';

  function onKeydown(e) {
    if (e.key === 'Escape') onClose();
  }
</script>

<svelte:window on:keydown={onKeydown} />

<div class="backdrop" use:backdropClose={onClose} role="presentation" out:dialogOut|global>
  <div class="modal" role="dialog" aria-modal="true" aria-label={$t('about.title')}>
    <button class="btn ghost icon x" on:click={onClose} title={$t('about.close')} aria-label={$t('about.close')}><X size={18} /></button>
    <img class="logo" src={logoUrl} alt="" />
    <h2>{info?.name ?? 'SaveVault'}</h2>
    <div class="ver">
      {$t('about.version', { version: info?.version ?? '—' })}{#if buildLabel}<span class="build"> {$t('about.built', { date: buildLabel })}</span>{/if}
    </div>
    <p class="tagline">{info?.tagline ?? ''}</p>

    <div class="meta">
      <div><span>{$t('about.license')}</span> {info?.license ?? 'MIT'}</div>
      <div><span>{$t('about.builtWith')}</span> {info?.tech ?? 'Go + Wails'}</div>
    </div>

    <div class="links">
      <button class="btn small" on:click={() => native.openExternal(GITHUB_URL)}>GitHub<ExternalLink size={13} /></button>
      <button class="btn small" on:click={openChangelog}><ScrollText size={14} />{$t('about.changelog')}</button>
    </div>

    <p class="copy">{info?.copyright ?? ''}</p>
  </div>
</div>

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    background: var(--overlay);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 90;
    padding: 32px;
  }
  .modal {
    position: relative;
    width: min(420px, 100%);
    background: var(--bg-raised);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-lg);
    padding: 32px 28px 26px;
    text-align: center;
    box-shadow: var(--shadow);
  }
  .x {
    position: absolute;
    top: 12px;
    right: 12px;
  }
  .logo {
    width: 72px;
    height: 72px;
    border-radius: 18px;
    margin-bottom: 12px;
  }
  h2 {
    font-size: 1.4rem;
    font-weight: 700;
  }
  .ver {
    color: var(--accent-text);
    font-weight: 600;
    font-size: 0.9rem;
    margin-top: 2px;
  }
  .tagline {
    color: var(--text-dim);
    font-size: 0.9rem;
    margin-top: 8px;
  }
  .meta {
    display: flex;
    justify-content: center;
    gap: 22px;
    margin: 18px 0 14px;
    font-size: 0.85rem;
    color: var(--text);
  }
  .meta span {
    display: block;
    color: var(--text-faint);
    font-size: 0.72rem;
    text-transform: uppercase;
    letter-spacing: 0.05em;
    margin-bottom: 2px;
  }
  .build {
    color: var(--text-faint);
    font-weight: 400;
    font-size: 0.78rem;
  }
  .links {
    display: flex;
    justify-content: center;
    flex-wrap: wrap;
    gap: 8px;
    margin-bottom: 14px;
  }
  .copy {
    font-size: 0.78rem;
    color: var(--text-faint);
  }
</style>

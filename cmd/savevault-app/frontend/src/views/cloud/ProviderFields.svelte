<script>
  // Where a provider without a sign-in sends its backups.
  import { native } from '../../lib/api.js';
  import { GAME_SAVE_PROVIDER } from '../../lib/cloudproviders.js';
  import { t, tt } from '../../lib/i18n.js';

  export let config;

  async function pickFolder() {
    const dir = await native.selectDirectory(tt('cloud.fields.selectDestination'));
    if (dir) config.url = dir;
  }
</script>

{#if config.provider === 'local'}
  <div class="field">
    <label for="cb-folder">{$t('cloud.fields.destinationFolder')}</label>
    <div class="path-row">
      <input id="cb-folder" bind:value={config.url} placeholder="D:\Backups\SaveVault" />
      <button class="btn" on:click={pickFolder}>{$t('cloud.fields.browse')}</button>
    </div>
  </div>
{:else if config.provider === 'webdav' || config.provider === GAME_SAVE_PROVIDER}
  <div class="field">
    <label for="cb-url">{config.provider === GAME_SAVE_PROVIDER ? $t('cloud.fields.gameSaveAddress') : $t('cloud.fields.webdavUrl')}</label>
    <input
      id="cb-url"
      bind:value={config.url}
      placeholder={config.provider === GAME_SAVE_PROVIDER ? 'https://nas.example:8788' : 'https://nas.local/dav/savevault/'}
    />
    {#if config.provider === GAME_SAVE_PROVIDER}
      <span class="hint">{$t('cloud.fields.gameSaveAddressHint')}</span>
    {/if}
  </div>
  <div class="two">
    {#if config.provider === GAME_SAVE_PROVIDER}
      <div class="field two-wide">
        <label for="cb-pass">{$t('cloud.fields.syncKey')}</label>
        <input
          id="cb-pass"
          type="password"
          bind:value={config.password}
          placeholder={config.hasPassword ? '••••••••' : ''}
          autocomplete="off"
        />
        <span class="hint">{$t('cloud.fields.syncKeyHint')}</span>
      </div>
    {:else}
    <div class="field">
      <label for="cb-user">{$t('cloud.fields.username')}</label>
      <input id="cb-user" bind:value={config.username} />
    </div>
    <div class="field">
      <label for="cb-pass">{$t('cloud.fields.password')}</label>
      <input
        id="cb-pass"
        type="password"
        bind:value={config.password}
        placeholder={config.hasPassword ? '••••••••' : ''}
        autocomplete="off"
      />
    </div>
    {/if}
  </div>
{:else if config.provider === 's3'}
  <div class="field">
    <label for="cb-s3-endpoint">{$t('cloud.fields.s3Endpoint')}</label>
    <input
      id="cb-s3-endpoint"
      bind:value={config.url}
      placeholder="https://<account>.r2.cloudflarestorage.com"
    />
  </div>
  <div class="two">
    <div class="field">
      <label for="cb-s3-bucket">{$t('cloud.fields.s3Bucket')}</label>
      <input id="cb-s3-bucket" bind:value={config.s3Bucket} placeholder="savevault-backups" />
    </div>
    <div class="field">
      <label for="cb-s3-region">{$t('cloud.fields.s3Region')}</label>
      <input id="cb-s3-region" bind:value={config.s3Region} placeholder="auto" />
    </div>
    <div class="field">
      <label for="cb-s3-ak">{$t('cloud.fields.s3AccessKey')}</label>
      <input id="cb-s3-ak" bind:value={config.username} autocomplete="off" />
    </div>
    <div class="field">
      <label for="cb-s3-sk">{$t('cloud.fields.s3SecretKey')}</label>
      <input
        id="cb-s3-sk"
        type="password"
        bind:value={config.password}
        placeholder={config.hasPassword ? '••••••••' : ''}
        autocomplete="off"
      />
    </div>
    <div class="field two-wide">
      <label for="cb-s3-prefix">{$t('cloud.fields.s3Prefix')}</label>
      <input id="cb-s3-prefix" bind:value={config.s3Prefix} placeholder="savevault/" />
    </div>
  </div>
  <label class="check s3-path-style">
    <input type="checkbox" bind:checked={config.s3PathStyle} />
    {$t('cloud.fields.s3PathStyle')}
  </label>
{:else if config.provider === 'webhook'}
  <div class="field">
    <label for="cb-hook">{$t('cloud.fields.webhookUrl')}</label>
    <input id="cb-hook" bind:value={config.url} />
  </div>
  <div class="field">
    <label for="cb-headers">{$t('cloud.fields.customHeaders')}</label>
    <input
      id="cb-headers"
      bind:value={config.headers}
      placeholder={config.hasHeaders ? '••••••••' : '{"Authorization": "Bearer …"}'}
    />
  </div>
{/if}

<style>
  .two {
    display: grid;
    grid-template-columns: 1fr 1fr;
    gap: 12px;
  }
  .two-wide { grid-column: 1 / -1; }
  .s3-path-style { margin-top: 8px; }
</style>

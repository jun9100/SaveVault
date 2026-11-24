<script>
  import { dialogOut } from '../lib/motion.js';
  import { confirmRequest, answerConfirm } from '../lib/stores.js';
  import { backdropClose } from '../lib/backdrop.js';

  let cancelBtn;
  let confirmBtn;

  // Focus Cancel on destructive prompts so an accidental Enter never wipes data;
  // focus Confirm on routine prompts.
  $: if ($confirmRequest) {
    if ($confirmRequest.danger && cancelBtn) cancelBtn.focus();
    else if (confirmBtn) confirmBtn.focus();
  }

  function onKeydown(e) {
    if (!$confirmRequest) return;
    if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      answerConfirm(false);
      return;
    }
    if (e.key === 'Enter') {
      // Let focused buttons/controls handle Enter natively instead of forcing Confirm.
      if (document.activeElement?.closest('button, a, input, select, textarea')) return;
      e.preventDefault();
      e.stopPropagation();
      answerConfirm(!$confirmRequest.danger);
    }
  }
</script>

<svelte:window on:keydown|capture={onKeydown} />

{#if $confirmRequest}
  <div class="backdrop" use:backdropClose={() => answerConfirm(false)} role="presentation" out:dialogOut|global>
    <div class="modal card" role="alertdialog" aria-modal="true" aria-label={$confirmRequest.title}>
      <h3>{$confirmRequest.title}</h3>
      <p class="message">{$confirmRequest.message}</p>
      <div class="actions">
        <button class="btn" bind:this={cancelBtn} on:click={() => answerConfirm(false)}>{$confirmRequest.cancelText}</button>
        <button
          class="btn primary"
          class:danger={$confirmRequest.danger}
          bind:this={confirmBtn}
          on:click={() => answerConfirm(true)}
        >
          {$confirmRequest.confirmText}
        </button>
      </div>
    </div>
  </div>
{/if}

<style>
  .backdrop {
    position: fixed;
    inset: 0;
    background: var(--overlay);
    display: flex;
    align-items: center;
    justify-content: center;
    z-index: 96; /* above the conflict modal (90) — confirms are always the top question */
    padding: 32px;
  }
  .modal {
    width: min(440px, 100%);
    background: var(--bg-raised);
    border: 1px solid var(--border-strong);
    border-radius: var(--radius-lg);
    padding: 22px 24px 20px;
    box-shadow: var(--shadow);
  }
  h3 {
    margin: 0 0 10px;
    font-size: 1.05rem;
  }
  .message {
    color: var(--text-dim);
    font-size: 0.92rem;
    line-height: 1.5;
    margin: 0 0 18px;
    word-break: break-word;
  }
  .actions {
    display: flex;
    justify-content: flex-end;
    gap: 10px;
  }
  /* The one action the dialog exists for, so solid rather than tinted. */
  .btn.danger {
    background: var(--danger);
    border-color: transparent;
    color: #fff;
  }
  .btn.danger:hover {
    background: var(--danger);
    filter: brightness(1.1);
  }
</style>

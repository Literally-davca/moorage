<script lang="ts">
  import { api, type UserPreferences } from '../lib/api';
  import { app } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';

  let busy = $state(false);
  let err = $state('');

  const BEGINNER: UserPreferences = {
    mode: 'beginner',
    composeView: 'form',
    envView: 'vars',
    linterShowTips: true,
    newStackDefault: 'templates',
    guidesMode: 'extended',
    confirmActions: 'all',
    onboardingDone: true,
    manualOverrides: { ...(app.user?.preferences?.manualOverrides ?? {}) },
  };

  const ADVANCED: UserPreferences = {
    mode: 'advanced',
    composeView: 'yaml-comments',
    envView: 'raw',
    linterShowTips: false,
    newStackDefault: 'blank',
    guidesMode: 'compact',
    confirmActions: 'destructive',
    onboardingDone: true,
    manualOverrides: { ...(app.user?.preferences?.manualOverrides ?? {}) },
  };

  async function choose(preset: UserPreferences) {
    busy = true;
    err = '';
    try {
      const saved = await api.savePreferences(preset);
      if (app.user) {
        app.user = { ...app.user, preferences: saved };
      }
    } catch (e) {
      err = (e as Error).message;
    } finally {
      busy = false;
    }
  }
</script>

<div class="onboard-backdrop" role="dialog" aria-modal="true" aria-labelledby="onboard-title">
  <div class="onboard card card-pad">
    <div class="onboard-head">
      <div class="logo-mark"><Icon name="anchor" /></div>
      <div>
        <h2 id="onboard-title">Choose your Moorage experience</h2>
        <p class="meta">Pick how compose files, variables, linter tips, and confirmations behave by default.</p>
      </div>
    </div>

    <div class="modes">
      <button type="button" class="mode-card" disabled={busy} onclick={() => choose(BEGINNER)}>
        <div class="mode-top">
          <strong>Beginner (Guided)</strong>
          <span class="badge">Recommended for quick setup</span>
        </div>
        <ul>
          <li><strong>Compose Form</strong> editor (lossless YAML fields)</li>
          <li><strong>Variables table</strong> for <code>.env</code> secrets &amp; defaults</li>
          <li><strong>Linter tips expanded</strong> with 1-click fixes</li>
          <li><strong>Templates</strong> highlighted for new stacks</li>
          <li><strong>Extended guides</strong> &amp; confirmations on actions</li>
        </ul>
      </button>

      <button type="button" class="mode-card" disabled={busy} onclick={() => choose(ADVANCED)}>
        <div class="mode-top">
          <strong>Advanced (YAML-first)</strong>
          <span class="badge">Direct control</span>
        </div>
        <ul>
          <li><strong>Raw YAML</strong> editor (with inline comments view)</li>
          <li><strong>Raw <code>.env</code></strong> text editor</li>
          <li><strong>Compact linter</strong> (tips collapsed until needed)</li>
          <li><strong>Blank compose</strong> ready for paste / auto-indent</li>
          <li><strong>Confirm destructive actions</strong> only</li>
        </ul>
      </button>
    </div>

    {#if err}<p class="error-text" role="alert">{err}</p>{/if}

    <p class="meta onboard-foot">
      You can switch modes or customize every individual option anytime in
      <strong>Account → Experience &amp; editor preferences</strong>.
    </p>
  </div>
</div>

<style>
  .onboard-backdrop {
    position: fixed;
    inset: 0;
    z-index: 80;
    background: rgba(7, 13, 21, 0.78);
    backdrop-filter: blur(4px);
    display: grid;
    place-items: center;
    padding: 16px;
  }
  .onboard {
    width: min(760px, 100%);
    display: grid;
    gap: 16px;
    background: var(--pier-raised);
    box-shadow: var(--lift-raised);
  }
  .onboard-head {
    display: flex;
    gap: 12px;
    align-items: flex-start;
  }
  .logo-mark {
    width: 36px;
    height: 36px;
    border-radius: 10px;
    background: var(--buoy);
    color: var(--buoy-ink);
    display: grid;
    place-items: center;
    flex-shrink: 0;
  }
  .onboard-head h2 {
    margin: 0 0 4px;
    font-size: 18px;
    color: var(--foam);
  }
  .onboard-head p {
    margin: 0;
  }
  .modes {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 12px;
  }
  @media (max-width: 640px) {
    .modes {
      grid-template-columns: minmax(0, 1fr);
    }
  }
  .mode-card {
    display: grid;
    gap: 10px;
    align-content: start;
    text-align: left;
    padding: 16px;
    border-radius: var(--r-card);
    border: 1px solid var(--rope-strong);
    background: var(--well);
    color: var(--mist);
    font: inherit;
    cursor: pointer;
    transition: border-color 0.15s ease, background 0.15s ease;
  }
  .mode-card:hover:not(:disabled) {
    border-color: var(--buoy);
    background: var(--pier);
  }
  .mode-top {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 8px;
    flex-wrap: wrap;
  }
  .mode-top strong {
    font-size: 15px;
    color: var(--foam);
  }
  .mode-card ul {
    margin: 0;
    padding-left: 18px;
    display: grid;
    gap: 6px;
    font-size: 13px;
    line-height: 1.4;
  }
  .onboard-foot {
    margin: 0;
    font-size: 12.5px;
    text-align: center;
  }
</style>

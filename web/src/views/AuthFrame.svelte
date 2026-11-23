<script lang="ts">
  import type { Snippet } from 'svelte';
  import Icon from '../lib/Icon.svelte';
  let { title, lead, children }: { title: string; lead: string; children: Snippet } = $props();
</script>

<main class="frame">
  <div class="card panel">
    <div class="brand">
      <span class="logo"><Icon name="anchor" /></span>
      <span class="word">Moorage</span>
    </div>
    <h1>{title}</h1>
    <p class="lead">{lead}</p>
    {@render children()}
  </div>
</main>

<style>
  .frame {
    min-height: 100vh;
    display: grid;
    place-items: center;
    padding: 24px 16px;
  }
  .panel {
    width: min(400px, 100%);
    padding: 28px 28px 24px;
  }
  .brand {
    display: flex;
    align-items: center;
    gap: 10px;
    margin-bottom: 28px;
  }
  .logo {
    width: 28px;
    height: 28px;
    border-radius: 8px;
    background: var(--buoy);
    color: var(--buoy-ink);
    display: inline-flex;
    align-items: center;
    justify-content: center;
    font-size: 17px;
  }
  .word {
    font-weight: 600;
    font-size: 16px;
  }
  h1 {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .lead {
    color: var(--haze);
    margin: 6px 0 22px;
  }
</style>

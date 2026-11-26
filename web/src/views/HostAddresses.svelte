<script lang="ts">
  // Named addresses of one host (M17B): Ethernet / Wi-Fi / VPN / domain / custom. The
  // default address builds the main link of every service; the others are offered next
  // to it. Auto-detected interfaces (native / host-network mode) can be picked as default.
  import { untrack } from 'svelte';
  import Icon from '../lib/Icon.svelte';
  import Select from '../lib/Select.svelte';
  import { addressKinds, hostnet, kindLabel, netModeNote, type AddressKind, type HostAddress, type NetMode } from '../lib/hostnet';

  let { hostId, hostName, onClose, onSaved }: { hostId: string; hostName: string; onClose: () => void; onSaved?: () => void } = $props();

  let list = $state<HostAddress[]>([]);
  let netMode = $state<NetMode | undefined>(undefined);
  let loaded = $state(false);
  let saving = $state(false);
  let saved = $state(false);
  let error = $state('');
  let dirty = $state(false);

  async function load() {
    error = '';
    try {
      const r = await hostnet.addresses(hostId);
      list = r.addresses.map((a) => ({ ...a }));
      netMode = r.netMode;
      loaded = true;
      dirty = false;
    } catch (e) {
      error = (e as Error).message;
    }
  }

  $effect(() => {
    void hostId;
    untrack(load);
  });

  const locked = $derived(list.some((a) => a.locked));
  const manual = $derived(list.filter((a) => !a.autoDetected && !a.locked));
  const detected = $derived(list.filter((a) => a.autoDetected));

  function touch() {
    dirty = true;
    saved = false;
  }

  function setDefault(a: HostAddress) {
    if (locked) return;
    for (const x of list) x.isDefault = x === a;
    touch();
  }

  function addRow() {
    list.push({ id: '', label: '', kind: 'custom', address: '', isDefault: !list.some((a) => a.isDefault) });
    touch();
  }

  function removeRow(a: HostAddress) {
    const wasDefault = a.isDefault;
    list = list.filter((x) => x !== a);
    if (wasDefault && !locked) {
      const next = list.find((x) => !x.missing);
      if (next) next.isDefault = true;
    }
    touch();
  }

  async function save() {
    saving = true;
    error = '';
    try {
      await hostnet.saveAddresses(
        hostId,
        list.filter((a) => !a.locked && (!a.autoDetected || a.isDefault)),
      );
      saved = true;
      await load();
      onSaved?.();
    } catch (e) {
      error = (e as Error).message;
    } finally {
      saving = false;
    }
  }
</script>

<section class="card card-pad addrs" aria-label={`Addresses of ${hostName}`}>
  <div class="head-bar">
    <h2 class="section-title">Addresses · {hostName}</h2>
    <span class="grow"></span>
    <button type="button" class="btn btn-ghost" onclick={onClose}><Icon name="x" /> Close</button>
  </div>
  <p class="meta">
    Service links use the default address; the other addresses are offered next to each link. Labels from
    <span class="mono">moorage.url</span> and proxy URLs always win.
  </p>

  {#if !loaded}
    <div class="skeleton" style="height:14px;width:45%"></div>
  {:else}
    {#if locked}
      {@const env = list.find((a) => a.locked)}
      <div class="row locked">
        <span class="radio on" aria-hidden="true"></span>
        <div class="grow">
          <div class="lbl">Set by <span class="mono">MOORAGE_HOST_IP</span></div>
          <div class="meta mono">{env?.address}</div>
        </div>
        <span class="tag"><Icon name="lock" /> Default</span>
      </div>
    {/if}

    <div class="group">
      <h3 class="sub">Your addresses</h3>
      {#if manual.length === 0}
        <p class="meta">No addresses yet. Add the LAN IP, a VPN address or a domain of this host.</p>
      {/if}
      {#each manual as a, i (a.id || 'new' + i)}
        <div class="row edit">
          <input
            type="radio"
            name={'def-' + hostId}
            checked={!!a.isDefault}
            disabled={locked}
            aria-label={`Use ${a.label || a.address || 'this address'} as default`}
            onchange={() => setDefault(a)}
          />
          <input class="field lbl-in" placeholder="Name (e.g. LAN)" maxlength="40" aria-label="Address name" bind:value={a.label} oninput={touch} />
          <div class="kind">
            <Select
              label="Address type"
              compact
              bind:value={a.kind}
              items={addressKinds.map((k) => ({ value: k.k, label: k.label }))}
              onchange={touch}
            />
          </div>
          <input class="field mono addr" placeholder="192.168.1.10 or nas.lan" aria-label="IP address or host name" bind:value={a.address} oninput={touch} />
          <button type="button" class="btn btn-ghost btn-icon" title="Remove address" aria-label="Remove address" onclick={() => removeRow(a)}>
            <Icon name="trash" />
          </button>
        </div>
      {/each}
      <div>
        <button type="button" class="btn btn-ghost" onclick={addRow} disabled={manual.length >= 20}><Icon name="plus" /> Add address</button>
      </div>
    </div>

    <div class="group">
      <h3 class="sub">Detected on this host</h3>
      {#if detected.length === 0}
        <p class="meta">{netModeNote(netMode)}</p>
      {:else}
        {#each detected as a (a.id)}
          <label class="row">
            <input type="radio" name={'def-' + hostId} checked={!!a.isDefault} disabled={locked} onchange={() => setDefault(a as HostAddress)} />
            <div class="grow">
              <div class="lbl">{a.label} <span class="meta">{kindLabel(a.kind as AddressKind)}</span></div>
              <div class="meta mono">{a.address}</div>
            </div>
            {#if a.missing}<span class="tag warn">Not detected now</span>{:else}<span class="tag">Auto</span>{/if}
          </label>
        {/each}
      {/if}
    </div>

    <div class="actions">
      {#if saved && !dirty}<span class="meta ok"><Icon name="check" /> Saved</span>{/if}
      <span class="grow"></span>
      <button type="button" class="btn btn-primary" onclick={save} disabled={!dirty || saving} aria-busy={saving}>
        {saving ? 'Saving…' : 'Save addresses'}
      </button>
    </div>
  {/if}
  {#if error}<p class="error-text" role="alert">{error}</p>{/if}
</section>

<style>
  .addrs {
    display: grid;
    gap: 12px;
  }
  .head-bar,
  .actions {
    display: flex;
    align-items: center;
    gap: 8px;
  }
  .grow {
    flex: 1;
    min-width: 0;
  }
  .group {
    display: grid;
    gap: 6px;
  }
  .sub {
    font-size: 12px;
    font-weight: 600;
    color: var(--mist);
    margin: 4px 0 0;
  }
  .row {
    display: flex;
    align-items: center;
    gap: 10px;
    padding: 8px 10px;
    border-radius: var(--r-md);
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  label.row {
    cursor: pointer;
  }
  label.row:hover {
    background: var(--rope);
  }
  .row.edit {
    padding: 6px 8px;
  }
  .row input[type='radio'] {
    accent-color: var(--buoy);
    width: 16px;
    height: 16px;
    flex: none;
  }
  .radio {
    width: 14px;
    height: 14px;
    border-radius: 999px;
    box-shadow: inset 0 0 0 4px var(--buoy);
    flex: none;
  }
  .lbl {
    font-size: 13px;
    color: var(--foam);
  }
  .field {
    height: 32px;
  }
  .lbl-in {
    flex: 1 1 140px;
  }
  .kind {
    flex: 0 0 120px;
  }
  .addr {
    flex: 2 1 180px;
  }
  .tag {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    font-size: 11px;
    padding: 2px 8px;
    border-radius: 999px;
    background: var(--rope);
    color: var(--haze);
    white-space: nowrap;
  }
  .tag.warn {
    background: var(--signal-wash);
    color: var(--signal-text);
  }
  .ok {
    color: var(--afloat-text);
    display: inline-flex;
    gap: 4px;
    align-items: center;
  }
  @media (max-width: 640px) {
    .row.edit {
      flex-wrap: wrap;
    }
    .kind {
      flex-basis: 100%;
    }
  }
</style>

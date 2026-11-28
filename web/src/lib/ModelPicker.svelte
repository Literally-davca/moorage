<script lang="ts">
  import { api, type AssistantStatus } from './api';
  import Select, { type SelectItem } from './Select.svelte';
  import { chat } from './assistantStore.svelte';

  // Model choice for this chat (M19E, M20J). Admins always see it; the list of models
  // is fetched when it opens and dropped when it closes (hundreds of rows, M20E).
  // Everyone else sees the model name.
  let groups = $state.raw<NonNullable<AssistantStatus['models']> | null>(null);
  let error = $state('');

  const value = $derived(
    chat.pick.provider ? chat.pick.provider + '|' + chat.pick.model : chat.info?.default ? chat.info.default.provider + '|' + chat.info.default.model : '',
  );
  const items = $derived.by<SelectItem[]>(() => {
    const cur: SelectItem = { value, label: value.split('|').slice(1).join('|') || (chat.info?.model ?? 'Model') };
    if (!groups) return error ? [cur, { value: '', label: "Couldn't load models", meta: error, disabled: error }] : [cur];
    const out: SelectItem[] = groups.flatMap((g) => [
      ...(g.error ? [{ value: '', label: `${g.name}: couldn't load models`, meta: g.error, disabled: g.error }] : []),
      ...g.models.map((m) => ({ value: g.id + '|' + m, label: m, meta: `${g.name} (${g.models.length})` })),
    ]);
    return out.some((i) => i.value === value) ? out : [cur, ...out];
  });

  async function toggle(open: boolean) {
    if (!open) {
      groups = null;
      return;
    }
    error = '';
    try {
      groups = (await api.assistantStatus(true)).models ?? [];
    } catch (e) {
      error = (e as Error).message;
    }
  }
  function pick(v: string) {
    const [p, ...rest] = v.split('|');
    if (p) chat.pick = { provider: p, model: rest.join('|') };
  }
</script>

{#if chat.info?.admin}
  <div class="picker"><Select {items} {value} label="Model" compact onchange={pick} ontoggle={toggle} /></div>
  {#if error || groups?.some((g) => g.error)}<a class="meta" href="#/settings/addons?open=assistant">Assistant settings</a>{/if}
{:else if chat.info?.model}
  <span class="meta">{chat.info.model}</span>
{/if}

<style>
  .picker {
    min-width: min(180px, 100%);
    max-width: 100%;
  }
</style>

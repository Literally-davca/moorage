<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type Op, type RenderedTemplate, type TemplateDetail } from '../lib/api';
  import { app, canEdit, currentHost, link } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import CodeEditor from '../lib/CodeEditor.svelte';
  import OpPanel from './OpPanel.svelte';

  let { source, id, onCreated }: { source: string; id: string; onCreated: () => void } = $props();

  let detail = $state<TemplateDetail | null>(null);
  let loadError = $state('');
  let step = $state<'form' | 'review' | 'done'>('form');

  let name = $state('');
  let values = $state<Record<string, string>>({});
  let reveal = $state<Record<string, boolean>>({});
  let fieldErrors = $state<Record<string, string>>({});
  let formError = $state('');
  let rendering = $state(false);

  let rendered = $state<RenderedTemplate | null>(null);
  let showSecrets = $state(false);
  let validation = $state('');
  let validating = $state(false);
  let deploy = $state(true);
  let creating = $state(false);
  let createError = $state('');

  let createdName = $state('');
  let op = $state<Op | null>(null);
  let deployError = $state('');

  const nameOk = $derived(/^[a-z0-9][a-z0-9_-]{0,62}$/.test(name));
  const host = $derived(currentHost());
  const editable = $derived(canEdit() && !!host?.connected);

  async function load() {
    try {
      const d = await api.template(source, id);
      detail = d;
      name = d.template.id;
      const v: Record<string, string> = {};
      for (const x of d.variables) v[x.name] = d.generated?.[x.name] ?? x.default ?? '';
      values = v;
    } catch (e) {
      loadError = (e as Error).message;
    }
  }
  $effect(() => {
    untrack(() => void load());
  });

  async function review(e: SubmitEvent) {
    e.preventDefault();
    if (!detail) return;
    formError = '';
    fieldErrors = {};
    rendering = true;
    try {
      const r = await api.renderTemplate(source, id, $state.snapshot(values));
      rendered = r.result;
      // Generated values the admin left empty are filled in now; keep them for Back.
      values = { ...values, ...r.result.values };
      showSecrets = false;
      step = 'review';
      void validate();
    } catch (err) {
      const msg = (err as Error).message;
      const field = detail.variables.find((v) => msg.startsWith(v.name + ':'));
      if (field) fieldErrors = { [field.name]: msg.slice(field.name.length + 1).trim() };
      else formError = msg;
    } finally {
      rendering = false;
    }
  }

  async function validate() {
    if (!rendered) return;
    validating = true;
    validation = '';
    try {
      const r = await api.validate(name, rendered.compose, rendered.env);
      validation = r.ok ? '' : (r.error ?? 'The compose file is not valid.');
    } catch (err) {
      validation = (err as Error).message;
    } finally {
      validating = false;
    }
  }

  // Secret values are replaced in what is shown until the admin reveals them. In the files
  // a "$" in a value is written as "$$", so that form is masked too.
  function mask(text: string): string {
    if (!rendered || showSecrets) return text;
    let out = text;
    for (const s of rendered.secrets) {
      const v = rendered.values[s];
      if (!v || v.length < 4) continue;
      for (const form of [v.replaceAll('$', '$$$$'), v]) out = out.split(form).join('••••••••');
    }
    return out;
  }
  const shownCompose = $derived(rendered ? mask(rendered.compose) : '');
  const shownEnv = $derived(rendered ? mask(rendered.env) : '');

  async function create() {
    if (!rendered) return;
    creating = true;
    createError = '';
    try {
      const created = await api.create(name, rendered.compose, rendered.env, { source, id });
      app.generatedNote = created?.generatedNote ?? '';
    } catch (err) {
      createError = (err as Error).message;
      creating = false;
      return;
    }
    createdName = name;
    step = 'done';
    creating = false;
    onCreated();
    if (deploy) {
      try {
        op = await api.action(createdName, 'up');
      } catch (err) {
        deployError = (err as Error).message;
      }
    }
  }

  const stackHref = $derived(link('stacks/' + encodeURIComponent(createdName)));
</script>

<div class="page">
  <a class="back meta" href={link('templates')}><Icon name="chevronLeft" /> Templates</a>

  {#if loadError}
    <section class="card card-pad"><p class="error-text" role="alert">{loadError}</p></section>
  {:else if !detail}
    <section class="card card-pad"><span class="skeleton" style="height:14px;width:200px;display:block"></span></section>
  {:else}
    <section class="card card-pad head">
      <div class="head-text">
        <h1 class="title">{detail.template.name}</h1>
        <p class="meta">
          {detail.template.sourceLabel}{detail.template.version ? ` · version ${detail.template.version}` : ''}{detail.template.category ? ` · ${detail.template.category}` : ''}
        </p>
        {#if detail.template.description}<p class="desc">{detail.template.description}</p>{/if}
      </div>
      {#if detail.template.website}
        <a class="btn btn-ghost" href={detail.template.website} target="_blank" rel="noopener noreferrer"><Icon name="external" /> Website</a>
      {/if}
    </section>

    <ol class="steps" aria-label="Steps">
      <li aria-current={step === 'form' ? 'step' : undefined}>1 · Values</li>
      <li aria-current={step === 'review' ? 'step' : undefined}>2 · Review</li>
      <li aria-current={step === 'done' ? 'step' : undefined}>3 · Create</li>
    </ol>

    {#if !editable}
      <section class="card card-pad grid1">
        <p class="meta">
          {#if !canEdit()}Only admins can deploy templates, and not on a read-only host.{:else}{host?.name ?? 'This host'} is offline.{/if}
          This is the template's compose file.
        </p>
        <CodeEditor value={detail.compose} readonly label="Template compose file" minHeight={200} />
        {#if detail.env}<CodeEditor value={detail.env} language="plain" readonly label="Template .env file" minHeight={60} />{/if}
      </section>
    {:else if step === 'form'}
      <form class="card card-pad grid1" onsubmit={review}>
        {#if detail.warnings.length}
          <ul class="warns" aria-label="Warnings">
            {#each detail.warnings as w (w)}<li><Icon name="alert" /> {w}</li>{/each}
          </ul>
        {/if}
        <div class="field-wrap">
          <label class="label" for="tname">Stack name</label>
          <input id="tname" class="field" bind:value={name} spellcheck="false" autocomplete="off" required />
          <p class="hint" class:error-text={name !== '' && !nameOk}>
            Created on {host?.name ?? 'this host'}. Lowercase letters, digits, dash or underscore.
          </p>
        </div>

        {#each detail.variables as v (v.name)}
          <div class="field-wrap">
            <label class="label" for={'v-' + v.name}>
              {v.label}{#if v.required}<span class="req" aria-hidden="true"> *</span>{/if}
              <span class="vname mono">{v.name}</span>
            </label>
            <div class="inline">
              <input
                id={'v-' + v.name}
                class="field"
                class:mono={v.secret}
                type={v.secret && !reveal[v.name] ? 'password' : 'text'}
                bind:value={values[v.name]}
                required={v.required && !v.generate}
                spellcheck="false"
                autocomplete="off"
                aria-invalid={!!fieldErrors[v.name]}
              />
              {#if v.secret}
                <button type="button" class="btn btn-icon" onclick={() => (reveal[v.name] = !reveal[v.name])}
                  aria-label={reveal[v.name] ? 'Hide value' : 'Show value'} title={reveal[v.name] ? 'Hide value' : 'Show value'}>
                  <Icon name={reveal[v.name] ? 'eyeOff' : 'eye'} />
                </button>
              {/if}
            </div>
            {#if fieldErrors[v.name]}
              <p class="hint error-text" role="alert">{fieldErrors[v.name]}</p>
            {:else if v.description || v.generate}
              <p class="hint">
                {v.description ?? ''}{v.generate ? `${v.description ? ' ' : ''}Generated (${v.generate}); leave empty for a new one.` : ''}
              </p>
            {/if}
          </div>
        {/each}

        {#if formError}<p class="error-text" role="alert">{formError}</p>{/if}
        <div class="row">
          <a class="btn btn-ghost" href={link('templates')}>Cancel</a>
          <button class="btn btn-primary" disabled={!nameOk || rendering} aria-busy={rendering}>Review files</button>
        </div>
      </form>
    {:else if step === 'review' && rendered}
      <section class="card card-pad grid1">
        <div class="review-head">
          <p class="section-title">Review before creating <span class="mono">{name}</span></p>
          {#if rendered.secrets.length}
            <button type="button" class="btn" onclick={() => (showSecrets = !showSecrets)}>
              <Icon name={showSecrets ? 'eyeOff' : 'eye'} /> {showSecrets ? 'Hide secrets' : 'Show secrets'}
            </button>
          {/if}
        </div>
        <p class="meta">These are the exact files the stack gets. You can still edit them on the stack page later.</p>
        {#if rendered.warnings.length}
          <ul class="warns" aria-label="Warnings">
            {#each rendered.warnings as w (w)}<li><Icon name="alert" /> {w}</li>{/each}
          </ul>
        {/if}
        <div>
          <span class="label">compose.yaml</span>
          {#key showSecrets}<CodeEditor value={shownCompose} readonly label="Final compose file" minHeight={200} />{/key}
        </div>
        {#if rendered.env}
          <div>
            <span class="label">.env</span>
            {#key showSecrets}<CodeEditor value={shownEnv} language="plain" readonly label="Final .env file" minHeight={60} />{/key}
          </div>
        {/if}
        {#if validating}
          <p class="meta">Checking the compose file…</p>
        {:else if validation}
          <div class="invalid" role="alert"><Icon name="alert" /><span class="mono">{validation}</span></div>
        {:else}
          <p class="meta ok"><Icon name="check" /> The compose file is valid.</p>
        {/if}
        <label class="checkbox">
          <input type="checkbox" bind:checked={deploy} />
          <span>Deploy right after creating (pulls images and starts the containers)</span>
        </label>
        {#if createError}<p class="error-text" role="alert">{createError}</p>{/if}
        <div class="row">
          <button type="button" class="btn btn-ghost" onclick={() => (step = 'form')} disabled={creating}>Back</button>
          <button type="button" class="btn btn-primary" onclick={create} disabled={creating || validating || !!validation} aria-busy={creating}>
            {deploy ? 'Create and deploy' : 'Create stack'}
          </button>
        </div>
      </section>
    {:else if step === 'done'}
      <section class="card card-pad grid1">
        <p class="section-title"><Icon name="check" /> Stack <span class="mono">{createdName}</span> created</p>
        {#if rendered?.notes}<pre class="notes">{rendered.notes}</pre>{/if}
        {#if deployError}<p class="error-text" role="alert">Deploy didn't start: {deployError}</p>{/if}
        <div class="row">
          <a class="btn btn-primary" href={stackHref}>Open stack</a>
        </div>
      </section>
      {#if op}
        <OpPanel {op} onDone={() => onCreated()} onClose={() => (op = null)} />
      {/if}
    {/if}
  {/if}
</div>

<style>
  .page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
    max-width: 920px;
  }
  .back {
    display: inline-flex;
    align-items: center;
    gap: 4px;
    width: fit-content;
  }
  .head {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 12px;
  }
  .head-text {
    display: grid;
    gap: 4px;
    min-width: 0;
  }
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
    overflow-wrap: anywhere;
  }
  .desc {
    font-size: 13px;
    color: var(--mist);
  }
  .steps {
    display: flex;
    flex-wrap: wrap;
    gap: 6px 16px;
    list-style: none;
    margin: 0;
    padding: 0 4px;
    font-size: 12px;
    color: var(--murk);
  }
  .steps li[aria-current='step'] {
    color: var(--foam);
    font-weight: 600;
  }
  .grid1 {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 16px;
  }
  .field-wrap {
    max-width: 480px;
    min-width: 0;
  }
  .inline {
    display: flex;
    gap: 6px;
  }
  .req {
    color: var(--buoy-text);
  }
  .vname {
    margin-left: 6px;
    font-size: 11px;
    font-weight: 400;
    color: var(--murk);
  }
  .row {
    display: flex;
    justify-content: flex-end;
    flex-wrap: wrap;
    gap: 8px;
  }
  .btn:disabled {
    opacity: 0.45;
    cursor: not-allowed;
  }
  .review-head {
    display: flex;
    justify-content: space-between;
    align-items: center;
    flex-wrap: wrap;
    gap: 8px;
  }
  .warns {
    list-style: none;
    margin: 0;
    padding: 10px 14px;
    display: grid;
    gap: 6px;
    border-radius: var(--r-md);
    background: var(--signal-wash);
    font-size: 13px;
    color: var(--signal-text);
  }
  .warns li {
    display: flex;
    gap: 8px;
    overflow-wrap: anywhere;
  }
  .invalid {
    display: flex;
    gap: 10px;
    padding: 10px 14px;
    border-radius: var(--r-md);
    background: var(--adrift-wash);
    color: var(--adrift-text);
    font-size: 12px;
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
  .ok {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    color: var(--afloat-text);
  }
  .section-title {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    flex-wrap: wrap;
  }
  .notes {
    margin: 0;
    padding: 12px 14px;
    border-radius: var(--r-md);
    background: var(--well);
    font: 400 13px/1.6 var(--font);
    color: var(--mist);
    white-space: pre-wrap;
    overflow-wrap: anywhere;
  }
</style>

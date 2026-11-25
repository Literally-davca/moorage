<!--
  Lossless Form view for compose.yaml powered by `yaml` AST (parseDocument).
  Edits mutate AST nodes in place so comments and advanced/unrepresented Compose keys
  (deploy, logging, cap_add, labels, sysctls, etc.) stay intact.
-->
<script lang="ts">
  import { isMap, isScalar, isSeq, parseDocument, Document, YAMLMap, YAMLSeq } from 'yaml';
  import Icon from './Icon.svelte';
  import Select from './Select.svelte';

  let {
    value = $bindable(''),
    readonly = false,
    onSwitchYaml,
  }: {
    value: string;
    readonly?: boolean;
    onSwitchYaml?: () => void;
  } = $props();

  const RESTART_OPTIONS = [
    { value: '', label: 'Default (not set)' },
    { value: 'unless-stopped', label: 'unless-stopped' },
    { value: 'always', label: 'always' },
    { value: 'on-failure', label: 'on-failure' },
    { value: 'no', label: 'no' },
  ];

  interface ServiceModel {
    name: string;
    image: string;
    restart: string;
    ports: string[];
    volumes: string[];
    env: { key: string; value: string }[];
    networks: string[];
    dependsOn: string[];
    memLimit: string;
    healthTest: string;
    healthInterval: string;
    healthTimeout: string;
    healthRetries: string;
    moorageUrl: string;
    advancedKeys: string[];
  }

  const HANDLED_SERVICE_KEYS = new Set([
    'image',
    'restart',
    'ports',
    'volumes',
    'environment',
    'networks',
    'depends_on',
    'mem_limit',
    'healthcheck',
    'labels',
  ]);

  let parseErr = $state('');
  let services = $state<ServiceModel[]>([]);
  let lastSerialized = $state('');
  let newServiceName = $state('');
  let newPort = $state<Record<string, string>>({});
  let newVol = $state<Record<string, string>>({});
  let newNet = $state<Record<string, string>>({});
  let newDep = $state<Record<string, string>>({});
  let newEnvKey = $state<Record<string, string>>({});
  let newEnvVal = $state<Record<string, string>>({});

  function seqToStrings(node: unknown): string[] {
    if (!isSeq(node)) return [];
    const out: string[] = [];
    for (const item of node.items) {
      if (isScalar(item)) out.push(String(item.value ?? ''));
      else if (item && typeof (item as { toJS?: () => unknown }).toJS === 'function') {
        out.push(JSON.stringify((item as { toJS: () => unknown }).toJS()));
      }
    }
    return out;
  }

  function mapOrSeqKeys(node: unknown): string[] {
    if (isSeq(node)) return seqToStrings(node);
    if (isMap(node)) return node.items.map((p) => String(isScalar(p.key) ? p.key.value : p.key));
    return [];
  }

  function readEnvPairs(node: unknown): { key: string; value: string }[] {
    if (isSeq(node)) {
      return seqToStrings(node).map((s) => {
        const idx = s.indexOf('=');
        return idx >= 0 ? { key: s.slice(0, idx), value: s.slice(idx + 1) } : { key: s, value: '' };
      });
    }
    if (isMap(node)) {
      return node.items.map((p) => ({
        key: String(isScalar(p.key) ? p.key.value : p.key),
        value: isScalar(p.value) ? (p.value.value == null ? '' : String(p.value.value)) : String(p.value ?? ''),
      }));
    }
    return [];
  }

  function readMoorageUrl(labelsNode: unknown): string {
    if (isMap(labelsNode)) {
      const v = labelsNode.get('moorage.url');
      return v == null ? '' : String(v);
    }
    if (isSeq(labelsNode)) {
      for (const s of seqToStrings(labelsNode)) {
        if (s.startsWith('moorage.url=')) return s.slice('moorage.url='.length);
      }
    }
    return '';
  }

  function parseFromYaml(src: string) {
    try {
      const doc = parseDocument(src || 'services: {}\n');
      if (doc.errors.length > 0) {
        parseErr = doc.errors[0].message;
        return;
      }
      parseErr = '';
      const svcsNode = doc.get('services', true);
      if (!isMap(svcsNode)) {
        services = [];
        return;
      }
      const next: ServiceModel[] = [];
      for (const pair of svcsNode.items) {
        const sName = String(isScalar(pair.key) ? pair.key.value : pair.key);
        const sMap = pair.value;
        if (!isMap(sMap)) continue;
        const hc = sMap.get('healthcheck', true);
        let hTest = '';
        let hInterval = '';
        let hTimeout = '';
        let hRetries = '';
        if (isMap(hc)) {
          const t = hc.get('test', true);
          if (isScalar(t)) hTest = String(t.value ?? '');
          else if (isSeq(t)) hTest = seqToStrings(t).join(' ');
          hInterval = hc.get('interval') != null ? String(hc.get('interval')) : '';
          hTimeout = hc.get('timeout') != null ? String(hc.get('timeout')) : '';
          hRetries = hc.get('retries') != null ? String(hc.get('retries')) : '';
        }
        const adv: string[] = [];
        for (const sp of sMap.items) {
          const k = String(isScalar(sp.key) ? sp.key.value : sp.key);
          if (!HANDLED_SERVICE_KEYS.has(k)) adv.push(k);
        }
        next.push({
          name: sName,
          image: sMap.get('image') != null ? String(sMap.get('image')) : '',
          restart: sMap.get('restart') != null ? String(sMap.get('restart')) : '',
          ports: seqToStrings(sMap.get('ports', true)),
          volumes: seqToStrings(sMap.get('volumes', true)),
          env: readEnvPairs(sMap.get('environment', true)),
          networks: mapOrSeqKeys(sMap.get('networks', true)),
          dependsOn: mapOrSeqKeys(sMap.get('depends_on', true)),
          memLimit: sMap.get('mem_limit') != null ? String(sMap.get('mem_limit')) : '',
          healthTest: hTest,
          healthInterval: hInterval,
          healthTimeout: hTimeout,
          healthRetries: hRetries,
          moorageUrl: readMoorageUrl(sMap.get('labels', true)),
          advancedKeys: adv,
        });
      }
      services = next;
    } catch (e) {
      parseErr = (e as Error).message;
    }
  }

  $effect(() => {
    const v = value;
    if (v !== lastSerialized) {
      parseFromYaml(v);
    }
  });

  function setOrDeleteScalar(doc: Document, map: YAMLMap, key: string, val: string) {
    const trimmed = val.trim();
    if (!trimmed) {
      map.delete(key);
    } else {
      map.set(key, doc.createNode(trimmed));
    }
  }

  function setOrDeleteStringList(doc: Document, map: YAMLMap, key: string, list: string[]) {
    const cleaned = list.map((x) => x.trim()).filter(Boolean);
    if (!cleaned.length) {
      map.delete(key);
      return;
    }
    const seq = new YAMLSeq();
    for (const item of cleaned) seq.add(doc.createNode(item));
    map.set(key, seq);
  }

  function commit(updated: ServiceModel[]) {
    services = updated;
    const doc = parseDocument(value || 'services: {}\n');
    if (doc.errors.length > 0) return;
    let svcsNode = doc.get('services', true);
    if (!isMap(svcsNode)) {
      svcsNode = new YAMLMap();
      doc.set('services', svcsNode);
    }
    const desiredNames = new Set(updated.map((s) => s.name));
    for (const pair of [...(svcsNode as YAMLMap).items]) {
      const k = String(isScalar(pair.key) ? pair.key.value : pair.key);
      if (!desiredNames.has(k)) (svcsNode as YAMLMap).delete(k);
    }
    for (const s of updated) {
      let sMap = (svcsNode as YAMLMap).get(s.name, true) as YAMLMap | undefined;
      if (!isMap(sMap)) {
        sMap = new YAMLMap();
        (svcsNode as YAMLMap).set(s.name, sMap);
      }
      setOrDeleteScalar(doc, sMap, 'image', s.image);
      setOrDeleteScalar(doc, sMap, 'restart', s.restart);
      setOrDeleteStringList(doc, sMap, 'ports', s.ports);
      setOrDeleteStringList(doc, sMap, 'volumes', s.volumes);

      // Environment: preserve map style if already map, otherwise write map
      const cleanEnv = s.env.filter((p) => p.key.trim() !== '');
      if (!cleanEnv.length) {
        sMap.delete('environment');
      } else {
        const existingEnv = sMap.get('environment', true);
        if (isSeq(existingEnv)) {
          const seq = new YAMLSeq();
          for (const e of cleanEnv) seq.add(doc.createNode(`${e.key.trim()}=${e.value}`));
          sMap.set('environment', seq);
        } else {
          const envMap = new YAMLMap();
          for (const e of cleanEnv) envMap.set(e.key.trim(), doc.createNode(e.value));
          sMap.set('environment', envMap);
        }
      }

      // Networks (preserve object keys if already a map with options)
      const existingNets = sMap.get('networks', true);
      if (isMap(existingNets)) {
        const cleanSet = new Set(s.networks.map((x) => x.trim()).filter(Boolean));
        for (const p of [...existingNets.items]) {
          const k = String(isScalar(p.key) ? p.key.value : p.key);
          if (!cleanSet.has(k)) existingNets.delete(k);
        }
        for (const n of cleanSet) {
          if (!existingNets.has(n)) existingNets.set(n, null);
        }
        if (existingNets.items.length === 0) sMap.delete('networks');
      } else {
        setOrDeleteStringList(doc, sMap, 'networks', s.networks);
      }

      // DependsOn (preserve condition maps when present)
      const existingDeps = sMap.get('depends_on', true);
      if (isMap(existingDeps)) {
        const cleanSet = new Set(s.dependsOn.map((x) => x.trim()).filter(Boolean));
        for (const p of [...existingDeps.items]) {
          const k = String(isScalar(p.key) ? p.key.value : p.key);
          if (!cleanSet.has(k)) existingDeps.delete(k);
        }
        for (const d of cleanSet) {
          if (!existingDeps.has(d)) existingDeps.set(d, { condition: 'service_started' });
        }
        if (existingDeps.items.length === 0) sMap.delete('depends_on');
      } else {
        setOrDeleteStringList(doc, sMap, 'depends_on', s.dependsOn);
      }

      setOrDeleteScalar(doc, sMap, 'mem_limit', s.memLimit);

      // Healthcheck
      if (s.healthTest.trim() || s.healthInterval.trim() || s.healthTimeout.trim() || s.healthRetries.trim()) {
        const existingHc = sMap.get('healthcheck', true);
        let hcNode: YAMLMap;
        if (isMap(existingHc)) {
          hcNode = existingHc;
        } else {
          hcNode = new YAMLMap();
          sMap.set('healthcheck', hcNode);
        }
        setOrDeleteScalar(doc, hcNode, 'test', s.healthTest);
        setOrDeleteScalar(doc, hcNode, 'interval', s.healthInterval);
        setOrDeleteScalar(doc, hcNode, 'timeout', s.healthTimeout);
        const rNum = Number(s.healthRetries.trim());
        if (s.healthRetries.trim() && Number.isFinite(rNum)) {
          hcNode.set('retries', doc.createNode(rNum));
        } else {
          hcNode.delete('retries');
        }
      } else {
        sMap.delete('healthcheck');
      }

      // Labels (`moorage.url` only, leaving any Traefik/Caddy/other labels untouched)
      const urlVal = s.moorageUrl.trim();
      const labelsNode = sMap.get('labels', true);
      if (urlVal) {
        if (isSeq(labelsNode)) {
          const filtered = seqToStrings(labelsNode).filter((x) => !x.startsWith('moorage.url='));
          filtered.push(`moorage.url=${urlVal}`);
          setOrDeleteStringList(doc, sMap, 'labels', filtered);
        } else {
          const lMap = isMap(labelsNode) ? labelsNode : new YAMLMap();
          lMap.set('moorage.url', doc.createNode(urlVal));
          sMap.set('labels', lMap);
        }
      } else if (isMap(labelsNode)) {
        labelsNode.delete('moorage.url');
        if (labelsNode.items.length === 0) sMap.delete('labels');
      } else if (isSeq(labelsNode)) {
        const filtered = seqToStrings(labelsNode).filter((x) => !x.startsWith('moorage.url='));
        setOrDeleteStringList(doc, sMap, 'labels', filtered);
      }
    }

    const out = doc.toString({ lineWidth: 0 });
    lastSerialized = out;
    value = out;
  }

  function mutateService(idx: number, fn: (s: ServiceModel) => ServiceModel) {
    const next = services.map((s, i) => (i === idx ? fn(s) : s));
    commit(next);
  }

  function addService() {
    const clean = newServiceName.trim().replace(/[^a-zA-Z0-9_.-]/g, '-');
    if (!clean || services.some((s) => s.name === clean)) return;
    commit([
      ...services,
      {
        name: clean,
        image: '',
        restart: 'unless-stopped',
        ports: [],
        volumes: [],
        env: [],
        networks: [],
        dependsOn: [],
        memLimit: '',
        healthTest: '',
        healthInterval: '',
        healthTimeout: '',
        healthRetries: '',
        moorageUrl: '',
        advancedKeys: [],
      },
    ]);
    newServiceName = '';
  }

  function removeService(idx: number) {
    commit(services.filter((_, i) => i !== idx));
  }
</script>

{#if parseErr}
  <div class="card card-pad form-err">
    <p class="error-text">This compose file has a YAML syntax error and cannot be edited in Form view yet: {parseErr}</p>
    {#if onSwitchYaml}
      <button type="button" class="btn btn-primary" onclick={onSwitchYaml}>Switch to YAML view to fix</button>
    {/if}
  </div>
{:else}
  <div class="cf">
    {#each services as s, idx (s.name)}
      <section class="card card-pad svc">
        <div class="svc-head">
          <div class="svc-title">
            <strong>{s.name}</strong>
            {#if s.advancedKeys.length > 0}
              <span class="meta adv-note" title="Preserved automatically when editing this form">
                +{s.advancedKeys.length} YAML field{s.advancedKeys.length === 1 ? '' : 's'} ({s.advancedKeys.slice(0, 3).join(', ')}{s.advancedKeys.length > 3 ? '…' : ''})
              </span>
            {/if}
          </div>
          {#if !readonly}
            <button type="button" class="btn btn-ghost" onclick={() => removeService(idx)} title="Remove service {s.name}">
              <Icon name="trash" /> Remove
            </button>
          {/if}
        </div>

        <div class="two">
          <div>
            <label class="label" for="img-{idx}">Image</label>
            <input
              id="img-{idx}"
              class="field mono"
              value={s.image}
              disabled={readonly}
              placeholder="e.g. nginx:1.27-alpine"
              oninput={(e) => mutateService(idx, (cur) => ({ ...cur, image: e.currentTarget.value }))}
            />
          </div>
          <div>
            <span class="label">Restart policy</span>
            <Select
              label="Restart policy for {s.name}"
              items={RESTART_OPTIONS}
              value={s.restart}
              disabled={readonly}
              onchange={(next) => mutateService(idx, (cur) => ({ ...cur, restart: next }))}
            />
          </div>
        </div>

        <div class="two">
          <div>
            <label class="label" for="mem-{idx}">Memory limit</label>
            <input
              id="mem-{idx}"
              class="field mono"
              value={s.memLimit}
              disabled={readonly}
              placeholder="e.g. 512m or 2g"
              oninput={(e) => mutateService(idx, (cur) => ({ ...cur, memLimit: e.currentTarget.value }))}
            />
          </div>
          <div>
            <label class="label" for="url-{idx}">Moorage quick-open URL</label>
            <input
              id="url-{idx}"
              class="field mono"
              value={s.moorageUrl}
              disabled={readonly}
              placeholder="https://app.example.com"
              oninput={(e) => mutateService(idx, (cur) => ({ ...cur, moorageUrl: e.currentTarget.value }))}
            />
          </div>
        </div>

        <div class="two">
          <div class="list-box">
            <span class="label">Ports</span>
            {#each s.ports as p, pi (pi)}
              <div class="chip-row">
                <input
                  class="field mono"
                  value={p}
                  disabled={readonly}
                  oninput={(e) =>
                    mutateService(idx, (cur) => ({
                      ...cur,
                      ports: cur.ports.map((x, k) => (k === pi ? e.currentTarget.value : x)),
                    }))}
                />
                {#if !readonly}
                  <button
                    type="button"
                    class="btn btn-ghost icon-only"
                    aria-label="Remove port"
                    onclick={() => mutateService(idx, (cur) => ({ ...cur, ports: cur.ports.filter((_, k) => k !== pi) }))}
                  >
                    <Icon name="x" />
                  </button>
                {/if}
              </div>
            {/each}
            {#if !readonly}
              <div class="chip-row">
                <input
                  class="field mono"
                  bind:value={newPort[s.name]}
                  placeholder="8080:80"
                  onkeydown={(e) => {
                    if (e.key === 'Enter' && (newPort[s.name] ?? '').trim()) {
                      e.preventDefault();
                      const v = newPort[s.name].trim();
                      newPort[s.name] = '';
                      mutateService(idx, (cur) => ({ ...cur, ports: [...cur.ports, v] }));
                    }
                  }}
                />
                <button
                  type="button"
                  class="btn btn-ghost"
                  disabled={!(newPort[s.name] ?? '').trim()}
                  onclick={() => {
                    const v = (newPort[s.name] ?? '').trim();
                    if (!v) return;
                    newPort[s.name] = '';
                    mutateService(idx, (cur) => ({ ...cur, ports: [...cur.ports, v] }));
                  }}
                >
                  <Icon name="plus" /> Add
                </button>
              </div>
            {/if}
          </div>

          <div class="list-box">
            <span class="label">Volumes</span>
            {#each s.volumes as v, vi (vi)}
              <div class="chip-row">
                <input
                  class="field mono"
                  value={v}
                  disabled={readonly}
                  oninput={(e) =>
                    mutateService(idx, (cur) => ({
                      ...cur,
                      volumes: cur.volumes.map((x, k) => (k === vi ? e.currentTarget.value : x)),
                    }))}
                />
                {#if !readonly}
                  <button
                    type="button"
                    class="btn btn-ghost icon-only"
                    aria-label="Remove volume"
                    onclick={() => mutateService(idx, (cur) => ({ ...cur, volumes: cur.volumes.filter((_, k) => k !== vi) }))}
                  >
                    <Icon name="x" />
                  </button>
                {/if}
              </div>
            {/each}
            {#if !readonly}
              <div class="chip-row">
                <input
                  class="field mono"
                  bind:value={newVol[s.name]}
                  placeholder="./data:/data"
                  onkeydown={(e) => {
                    if (e.key === 'Enter' && (newVol[s.name] ?? '').trim()) {
                      e.preventDefault();
                      const v = newVol[s.name].trim();
                      newVol[s.name] = '';
                      mutateService(idx, (cur) => ({ ...cur, volumes: [...cur.volumes, v] }));
                    }
                  }}
                />
                <button
                  type="button"
                  class="btn btn-ghost"
                  disabled={!(newVol[s.name] ?? '').trim()}
                  onclick={() => {
                    const v = (newVol[s.name] ?? '').trim();
                    if (!v) return;
                    newVol[s.name] = '';
                    mutateService(idx, (cur) => ({ ...cur, volumes: [...cur.volumes, v] }));
                  }}
                >
                  <Icon name="plus" /> Add
                </button>
              </div>
            {/if}
          </div>
        </div>

        <div class="list-box">
          <span class="label">Inline environment (consider keeping secrets in .env)</span>
          {#each s.env as ev, ei (ei)}
            <div class="env-row">
              <input
                class="field mono"
                value={ev.key}
                disabled={readonly}
                placeholder="KEY"
                oninput={(e) =>
                  mutateService(idx, (cur) => ({
                    ...cur,
                    env: cur.env.map((x, k) => (k === ei ? { ...x, key: e.currentTarget.value } : x)),
                  }))}
              />
              <input
                class="field mono"
                value={ev.value}
                disabled={readonly}
                placeholder="VALUE"
                oninput={(e) =>
                  mutateService(idx, (cur) => ({
                    ...cur,
                    env: cur.env.map((x, k) => (k === ei ? { ...x, value: e.currentTarget.value } : x)),
                  }))}
              />
              {#if !readonly}
                <button
                  type="button"
                  class="btn btn-ghost icon-only"
                  aria-label="Remove variable"
                  onclick={() => mutateService(idx, (cur) => ({ ...cur, env: cur.env.filter((_, k) => k !== ei) }))}
                >
                  <Icon name="x" />
                </button>
              {/if}
            </div>
          {/each}
          {#if !readonly}
            <div class="env-row">
              <input class="field mono" bind:value={newEnvKey[s.name]} placeholder="NEW_VAR" />
              <input class="field mono" bind:value={newEnvVal[s.name]} placeholder="value or '$'{'{NEW_VAR}'}" />
              <button
                type="button"
                class="btn btn-ghost"
                disabled={!(newEnvKey[s.name] ?? '').trim()}
                onclick={() => {
                  const k = (newEnvKey[s.name] ?? '').trim();
                  if (!k) return;
                  const v = newEnvVal[s.name] ?? '';
                  newEnvKey[s.name] = '';
                  newEnvVal[s.name] = '';
                  mutateService(idx, (cur) => ({ ...cur, env: [...cur.env, { key: k, value: v }] }));
                }}
              >
                <Icon name="plus" /> Add
              </button>
            </div>
          {/if}
        </div>

        <div class="two">
          <div class="list-box">
            <span class="label">Networks</span>
            <div class="tags">
              {#each s.networks as n, ni (ni)}
                <span class="tag mono">
                  {n}
                  {#if !readonly}
                    <button
                      type="button"
                      class="tag-x"
                      aria-label="Remove network {n}"
                      onclick={() => mutateService(idx, (cur) => ({ ...cur, networks: cur.networks.filter((_, k) => k !== ni) }))}
                    >
                      ×
                    </button>
                  {/if}
                </span>
              {/each}
            </div>
            {#if !readonly}
              <div class="chip-row">
                <input
                  class="field mono"
                  bind:value={newNet[s.name]}
                  placeholder="default or proxy"
                  onkeydown={(e) => {
                    if (e.key === 'Enter' && (newNet[s.name] ?? '').trim()) {
                      e.preventDefault();
                      const v = newNet[s.name].trim();
                      newNet[s.name] = '';
                      mutateService(idx, (cur) => ({ ...cur, networks: [...new Set([...cur.networks, v])] }));
                    }
                  }}
                />
                <button
                  type="button"
                  class="btn btn-ghost"
                  disabled={!(newNet[s.name] ?? '').trim()}
                  onclick={() => {
                    const v = (newNet[s.name] ?? '').trim();
                    if (!v) return;
                    newNet[s.name] = '';
                    mutateService(idx, (cur) => ({ ...cur, networks: [...new Set([...cur.networks, v])] }));
                  }}
                >
                  Add
                </button>
              </div>
            {/if}
          </div>

          <div class="list-box">
            <span class="label">Depends on</span>
            <div class="tags">
              {#each s.dependsOn as d, di (di)}
                <span class="tag mono">
                  {d}
                  {#if !readonly}
                    <button
                      type="button"
                      class="tag-x"
                      aria-label="Remove dependency {d}"
                      onclick={() => mutateService(idx, (cur) => ({ ...cur, dependsOn: cur.dependsOn.filter((_, k) => k !== di) }))}
                    >
                      ×
                    </button>
                  {/if}
                </span>
              {/each}
            </div>
            {#if !readonly}
              <div class="chip-row">
                <input
                  class="field mono"
                  bind:value={newDep[s.name]}
                  placeholder="db or redis"
                  onkeydown={(e) => {
                    if (e.key === 'Enter' && (newDep[s.name] ?? '').trim()) {
                      e.preventDefault();
                      const v = newDep[s.name].trim();
                      newDep[s.name] = '';
                      mutateService(idx, (cur) => ({ ...cur, dependsOn: [...new Set([...cur.dependsOn, v])] }));
                    }
                  }}
                />
                <button
                  type="button"
                  class="btn btn-ghost"
                  disabled={!(newDep[s.name] ?? '').trim()}
                  onclick={() => {
                    const v = (newDep[s.name] ?? '').trim();
                    if (!v) return;
                    newDep[s.name] = '';
                    mutateService(idx, (cur) => ({ ...cur, dependsOn: [...new Set([...cur.dependsOn, v])] }));
                  }}
                >
                  Add
                </button>
              </div>
            {/if}
          </div>
        </div>

        <details class="hc-details">
          <summary class="meta">Healthcheck {s.healthTest ? '· configured' : '· optional'}</summary>
          <div class="two hc-grid">
            <div>
              <label class="label" for="hct-{idx}">Test command</label>
              <input
                id="hct-{idx}"
                class="field mono"
                value={s.healthTest}
                disabled={readonly}
                placeholder="CMD-SHELL wget -qO- http://127.0.0.1:8080/ || exit 1"
                oninput={(e) => mutateService(idx, (cur) => ({ ...cur, healthTest: e.currentTarget.value }))}
              />
            </div>
            <div class="three">
              <div>
                <label class="label" for="hci-{idx}">Interval</label>
                <input
                  id="hci-{idx}"
                  class="field mono"
                  value={s.healthInterval}
                  disabled={readonly}
                  placeholder="30s"
                  oninput={(e) => mutateService(idx, (cur) => ({ ...cur, healthInterval: e.currentTarget.value }))}
                />
              </div>
              <div>
                <label class="label" for="hcto-{idx}">Timeout</label>
                <input
                  id="hcto-{idx}"
                  class="field mono"
                  value={s.healthTimeout}
                  disabled={readonly}
                  placeholder="5s"
                  oninput={(e) => mutateService(idx, (cur) => ({ ...cur, healthTimeout: e.currentTarget.value }))}
                />
              </div>
              <div>
                <label class="label" for="hcr-{idx}">Retries</label>
                <input
                  id="hcr-{idx}"
                  class="field mono"
                  value={s.healthRetries}
                  disabled={readonly}
                  placeholder="3"
                  oninput={(e) => mutateService(idx, (cur) => ({ ...cur, healthRetries: e.currentTarget.value }))}
                />
              </div>
            </div>
          </div>
        </details>
      </section>
    {/each}

    {#if !readonly}
      <div class="add-svc card card-pad">
        <input
          class="field mono"
          bind:value={newServiceName}
          placeholder="New service name (e.g. web, worker, db)"
          onkeydown={(e) => {
            if (e.key === 'Enter') {
              e.preventDefault();
              addService();
            }
          }}
        />
        <button type="button" class="btn btn-primary" disabled={!newServiceName.trim()} onclick={addService}>
          <Icon name="plus" /> Add service
        </button>
      </div>
    {/if}

    {#if onSwitchYaml}
      <p class="meta form-foot">
        Advanced Compose keys and comments are preserved automatically.
        <button type="button" class="link-btn" onclick={onSwitchYaml}>Edit raw YAML</button>
      </p>
    {/if}
  </div>
{/if}

<style>
  .cf {
    display: grid;
    gap: 12px;
    min-width: 0;
  }
  .svc {
    display: grid;
    gap: 12px;
    min-width: 0;
  }
  .svc-head {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
    flex-wrap: wrap;
  }
  .svc-title {
    display: flex;
    align-items: baseline;
    gap: 8px;
    flex-wrap: wrap;
  }
  .svc-title strong {
    font-size: 15px;
    color: var(--foam);
  }
  .adv-note {
    font-size: 12px;
  }
  .two {
    display: grid;
    grid-template-columns: repeat(2, minmax(0, 1fr));
    gap: 10px;
  }
  .three {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 8px;
  }
  @media (max-width: 720px) {
    .two,
    .three {
      grid-template-columns: minmax(0, 1fr);
    }
  }
  .list-box {
    display: grid;
    gap: 6px;
    min-width: 0;
  }
  .chip-row {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    gap: 6px;
    align-items: center;
  }
  .env-row {
    display: grid;
    grid-template-columns: minmax(0, 1fr) minmax(0, 1.3fr) auto;
    gap: 6px;
    align-items: center;
  }
  .icon-only {
    padding: 0 8px;
  }
  .tags {
    display: flex;
    flex-wrap: wrap;
    gap: 6px;
  }
  .tag {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    padding: 3px 8px;
    border-radius: 999px;
    background: var(--well);
    box-shadow: inset 0 0 0 1px var(--rope);
    font-size: 12px;
    color: var(--foam);
  }
  .tag-x {
    border: 0;
    background: none;
    color: var(--haze);
    cursor: pointer;
    padding: 0 2px;
    font-size: 14px;
    line-height: 1;
  }
  .tag-x:hover {
    color: var(--foam);
  }
  .hc-details summary {
    cursor: pointer;
    font-size: 12.5px;
  }
  .hc-grid {
    margin-top: 8px;
  }
  .add-svc {
    display: grid;
    grid-template-columns: minmax(0, 1fr) auto;
    gap: 8px;
    align-items: center;
  }
  .form-foot {
    margin: 0;
    font-size: 12px;
  }
  .link-btn {
    border: 0;
    background: none;
    padding: 0;
    color: var(--buoy-text);
    font: inherit;
    cursor: pointer;
    margin-left: 4px;
  }
  .form-err {
    display: grid;
    gap: 10px;
    justify-items: start;
  }
</style>

<script lang="ts">
  import { api, type StatusSnapshot, type StatusPage } from '../lib/api';
  import Icon from '../lib/Icon.svelte';
  import Select from '../lib/Select.svelte';

  let loading = $state(true);
  let busy = $state(false);
  let error = $state('');
  let message = $state('');
  let snap = $state<StatusSnapshot | null>(null);
  let pages = $state<StatusPage[]>([]);

  let provider = $state('s3');
  let selected = $state<number[]>([]);
  let intervalMinutes = $state(5);
  let staleMinutes = $state(15);
  let s3Endpoint = $state('');
  let s3Bucket = $state('');
  let s3Region = $state('');
  let s3AccessKey = $state('');
  let s3SecretKey = $state('');
  let s3Prefix = $state('');
  let webdavUrl = $state('');
  let webdavUsername = $state('');
  let webdavPassword = $state('');
  let githubRepo = $state('');
  let githubBranch = $state('gh-pages');
  let githubPath = $state('');
  let githubToken = $state('');

  function fill(s: StatusSnapshot) {
    snap = s;
    provider = s.provider || 's3';
    selected = [...(s.pages ?? [])];
    intervalMinutes = s.intervalMinutes || 5;
    staleMinutes = s.staleMinutes || 15;
    s3Endpoint = s.s3Endpoint ?? '';
    s3Bucket = s.s3Bucket ?? '';
    s3Region = s.s3Region ?? '';
    s3AccessKey = s.s3AccessKey ?? '';
    s3Prefix = s.s3Prefix ?? '';
    webdavUrl = s.webdavUrl ?? '';
    webdavUsername = s.webdavUsername ?? '';
    githubRepo = s.githubRepo ?? '';
    githubBranch = s.githubBranch || 'gh-pages';
    githubPath = s.githubPath ?? '';
    s3SecretKey = '';
    webdavPassword = '';
    githubToken = '';
  }

  async function load() {
    loading = true;
    error = '';
    try {
      const [s, p] = await Promise.all([api.statusSnapshot(), api.statusPages()]);
      pages = p.filter((x) => x.published);
      fill(s);
    } catch (e) {
      error = (e as Error).message;
    } finally {
      loading = false;
    }
  }
  load();

  function input() {
    return {
      provider,
      pages: selected,
      intervalMinutes: Number(intervalMinutes),
      staleMinutes: Number(staleMinutes),
      s3Endpoint,
      s3Bucket,
      s3Region,
      s3AccessKey,
      s3SecretKey,
      s3Prefix,
      webdavUrl,
      webdavUsername,
      webdavPassword,
      githubRepo,
      githubBranch,
      githubPath,
      githubToken,
    };
  }

  async function run(fn: () => Promise<void>) {
    busy = true;
    error = '';
    message = '';
    try {
      await fn();
    } catch (e) {
      error = (e as Error).message;
    } finally {
      busy = false;
    }
  }

  const save = (e: Event) => {
    e.preventDefault();
    return run(async () => {
      fill(await api.saveStatusSnapshot(input()));
      message = 'Saved. A snapshot will be published shortly.';
    });
  };
  const test = () =>
    run(async () => {
      const r = await api.testStatusSnapshot(input());
      message = `Write access to ${r.target} works.`;
    });
  const publish = () =>
    run(async () => {
      const l = await api.publishStatusSnapshot();
      message = `Published ${l.pages?.length ?? 0} page(s) to ${l.target ?? 'the target'}.`;
      fill(await api.statusSnapshot());
    });

  function togglePage(id: number) {
    selected = selected.includes(id) ? selected.filter((x) => x !== id) : [...selected, id];
  }
</script>

<div class="bpanel">
  {#if error}<p class="error-text" role="alert">{error}</p>{/if}
  {#if message}<p class="ok-msg"><Icon name="check" /> {message}</p>{/if}

  {#if loading}
    <p class="meta">Loading…</p>
  {:else}
    {#if snap?.last}
      <div class="secret-box">
        <span class="meta">
          Last publish {new Date(snap.last.at).toLocaleString()} ({snap.last.trigger ?? 'manual'}):
          {#if snap.last.ok}<span class="badge up">OK</span>{:else}<span class="badge down">Failed</span>{/if}
          {#if snap.stale}<span class="badge degraded">Snapshot is out of date</span>{/if}
        </span>
        {#if snap.last.error}<span class="meta">{snap.last.error}</span>{/if}
        {#if snap.last.nextRetry}<span class="meta">Next retry {new Date(snap.last.nextRetry).toLocaleTimeString()}</span>{/if}
      </div>
    {/if}

    <form class="snap-form" onsubmit={save}>
      <div class="sel-wrap">
        <Select
          label="Target"
          value={provider}
          items={[
            { value: 's3', label: 'S3-compatible (S3, R2, MinIO…)' },
            { value: 'webdav', label: 'WebDAV folder' },
            { value: 'github', label: 'GitHub Pages branch' },
          ]}
          onchange={(v) => (provider = v)}
        />
      </div>

      {#if provider === 's3'}
        <input class="field mono" bind:value={s3Endpoint} placeholder="Endpoint (https://<account>.r2.cloudflarestorage.com)" aria-label="S3 endpoint" />
        <input class="field mono" bind:value={s3Bucket} placeholder="Bucket (must already exist)" aria-label="S3 bucket" required />
        <input class="field mono" bind:value={s3Region} placeholder="Region (auto)" aria-label="S3 region" />
        <input class="field mono" bind:value={s3Prefix} placeholder="Prefix (optional folder)" aria-label="S3 prefix" />
        <input class="field mono" bind:value={s3AccessKey} placeholder="Access key ID" aria-label="S3 access key" autocomplete="off" />
        <input
          class="field mono"
          type="password"
          bind:value={s3SecretKey}
          placeholder={snap?.s3SecretKeySet ? 'Secret key (saved – leave empty to keep)' : 'Secret access key'}
          aria-label="S3 secret key"
          autocomplete="new-password"
        />
      {:else if provider === 'webdav'}
        <input class="field mono" bind:value={webdavUrl} placeholder="Folder URL (https://dav.example.com/status/)" aria-label="WebDAV URL" required />
        <input class="field mono" bind:value={webdavUsername} placeholder="Username" aria-label="WebDAV username" autocomplete="off" />
        <input
          class="field mono"
          type="password"
          bind:value={webdavPassword}
          placeholder={snap?.webdavPasswordSet ? 'Password (saved – leave empty to keep)' : 'Password'}
          aria-label="WebDAV password"
          autocomplete="new-password"
        />
      {:else}
        <input class="field mono" bind:value={githubRepo} placeholder="owner/repository (must already exist)" aria-label="GitHub repository" required />
        <input class="field mono" bind:value={githubBranch} placeholder="Branch (gh-pages)" aria-label="GitHub branch" />
        <input class="field mono" bind:value={githubPath} placeholder="Folder in the branch (optional)" aria-label="GitHub folder" />
        <input
          class="field mono"
          type="password"
          bind:value={githubToken}
          placeholder={snap?.githubTokenSet ? 'Token (saved – leave empty to keep)' : 'Fine-grained token with Contents: write'}
          aria-label="GitHub token"
          autocomplete="new-password"
        />
        <p class="meta full">GitHub Pages rebuilds on every push, so snapshots go out at most every 15 minutes.</p>
      {/if}

      <label class="num">
        <span class="meta">Publish every (min)</span>
        <input class="field" type="number" min="1" max="60" bind:value={intervalMinutes} />
      </label>
      <label class="num">
        <span class="meta">Show “may be out of date” after (min)</span>
        <input class="field" type="number" min="1" max="1440" bind:value={staleMinutes} />
      </label>

      <fieldset class="full pages">
        <legend class="meta">Pages (none selected = every published page)</legend>
        {#if pages.length === 0}
          <span class="meta">No published status pages yet.</span>
        {/if}
        {#each pages as p (p.id)}
          <label class="check">
            <input type="checkbox" checked={selected.includes(p.id)} onchange={() => togglePage(p.id)} />
            {p.title} <span class="meta mono">/{p.slug}</span>
          </label>
        {/each}
      </fieldset>

      <div class="ractions full">
        <button type="submit" class="btn btn-primary" disabled={busy}>Save</button>
        <button type="button" class="btn" onclick={test} disabled={busy}><Icon name="check" /> Test write access</button>
        <button type="button" class="btn" onclick={publish} disabled={busy || !snap?.target}>Publish now</button>
      </div>
      <p class="meta full">Moorage never creates the bucket, folder or repository – it only writes files into a target that exists.</p>
    </form>
  {/if}
</div>

<style>
  .bpanel {
    display: grid;
    gap: 10px;
    padding: 10px 0;
  }
  .snap-form {
    display: grid;
    grid-template-columns: repeat(auto-fill, minmax(220px, 1fr));
    gap: 8px;
    align-items: end;
  }
  .full {
    grid-column: 1 / -1;
  }
  .num {
    display: grid;
    gap: 4px;
  }
  .pages {
    display: flex;
    flex-wrap: wrap;
    gap: 10px;
    border: 0;
    padding: 0;
    margin: 0;
  }
  .check {
    display: inline-flex;
    align-items: center;
    gap: 6px;
  }
  .ractions {
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
  }
  .secret-box {
    padding: 10px 12px;
    border-radius: var(--r-md);
    background: var(--info-wash);
    box-shadow: inset 0 0 0 1px var(--info);
    display: grid;
    gap: 6px;
  }
  .ok-msg {
    display: flex;
    align-items: center;
    gap: 8px;
    color: var(--afloat-text);
  }
</style>

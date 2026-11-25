<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type ConnectionsConfig, type CoolifyHostConn, type SisterLink } from '../lib/api';
  import { app } from '../lib/state.svelte';
  import Icon from '../lib/Icon.svelte';
  import Select from '../lib/Select.svelte';
  import AddonGate from '../lib/AddonGate.svelte';
  import SetupHint from '../lib/SetupHint.svelte';

  let data = $state<ConnectionsConfig | null>(null);
  let error = $state('');

  // OIDC state
  let oidcIssuer = $state('');
  let oidcClientId = $state('');
  let oidcClientSecret = $state('');
  let oidcClearSecret = $state(false);
  let oidcScopes = $state('openid profile email groups');
  let oidcGroupsClaim = $state('groups');
  let oidcAdminGroups = $state('');
  let oidcViewerGroups = $state('');
  let oidcDefaultRole = $state('none');
  let oidcLabel = $state('Sign in with SSO');
  let oidcLocalLogin = $state(true);
  let oidcMsg = $state<{ ok: boolean; text: string } | null>(null);
  let oidcBusy = $state(false);

  // Coolify state
  let coolUrl = $state('');
  let coolToken = $state('');
  let coolClearToken = $state(false);
  let coolAllowPrivate = $state(false);
  let coolHosts = $state<(CoolifyHostConn & { clearToken?: boolean })[]>([]);
  let coolMsg = $state<{ ok: boolean; text: string } | null>(null);
  let coolBusy = $state(false);

  // Registries state
  let regRows = $state<{ server: string; username: string; password: string; passwordSet: boolean; fromEnv?: boolean; status?: string }[]>([]);
  let regBusy = $state(false);
  let regMsg = $state<{ ok: boolean; text: string } | null>(null);

  // Sister panels state
  let grafanaUrl = $state('');
  let grafanaToken = $state('');
  let grafanaClearToken = $state(false);
  let grafanaFolderUid = $state('');
  let grafanaAllowPrivate = $state(false);
  let beszelUrl = $state('');
  let customLinks = $state<SisterLink[]>([]);
  let panelBusy = $state(false);
  let panelMsg = $state<{ ok: boolean; text: string } | null>(null);

  async function load() {
    try {
      const c = await api.connections();
      data = c;
      oidcIssuer = c.oidc.issuer;
      oidcClientId = c.oidc.clientId;
      oidcClientSecret = '';
      oidcClearSecret = false;
      oidcScopes = (c.oidc.scopes ?? []).join(' ') || 'openid profile email groups';
      oidcGroupsClaim = c.oidc.groupsClaim || 'groups';
      oidcAdminGroups = (c.oidc.adminGroups ?? []).join(', ');
      oidcViewerGroups = (c.oidc.viewerGroups ?? []).join(', ');
      oidcDefaultRole = c.oidc.defaultRole || 'none';
      oidcLabel = c.oidc.label || 'Sign in with SSO';
      oidcLocalLogin = c.oidc.localLogin;

      coolUrl = c.coolify.url;
      coolToken = '';
      coolClearToken = false;
      coolAllowPrivate = c.coolify.allowPrivate;
      coolHosts = (c.coolify.hosts ?? []).map((h) => ({ ...h, token: '', clearToken: false }));

      regRows = (c.registries ?? []).map((r) => ({
        server: r.server,
        username: r.username,
        password: '',
        passwordSet: r.passwordSet,
        fromEnv: r.fromEnv,
      }));

      grafanaUrl = c.panels.grafanaUrl;
      grafanaToken = '';
      grafanaClearToken = false;
      grafanaFolderUid = c.panels.grafanaFolderUid ?? '';
      grafanaAllowPrivate = c.panels.grafanaAllowPrivate;
      beszelUrl = c.panels.beszelUrl;
      customLinks = (c.panels.links ?? []).map((l) => ({ ...l }));
      error = '';
    } catch (e) {
      error = (e as Error).message;
    }
  }

  $effect(() => {
    untrack(() => void load());
  });

  function splitCSV(s: string): string[] {
    return s.split(',').map((x) => x.trim()).filter(Boolean);
  }

  async function testOIDCNow() {
    oidcBusy = true;
    oidcMsg = null;
    try {
      const r = await api.testOIDC({
        issuer: oidcIssuer.trim(),
        clientId: oidcClientId.trim(),
        clientSecret: oidcClientSecret || undefined,
        scopes: oidcScopes.split(/\s+/).filter(Boolean),
      });
      oidcMsg = { ok: true, text: `Discovery verified.${r.redirectUri ? ` Callback: ${r.redirectUri}` : ''}` };
    } catch (e) {
      oidcMsg = { ok: false, text: (e as Error).message };
    } finally {
      oidcBusy = false;
    }
  }

  async function saveOIDC(e: SubmitEvent) {
    e.preventDefault();
    oidcBusy = true;
    oidcMsg = null;
    try {
      await api.saveConnections({
        oidc: {
          issuer: oidcIssuer.trim(),
          clientId: oidcClientId.trim(),
          clientSecret: oidcClearSecret ? '' : oidcClientSecret,
          clearClientSecret: oidcClearSecret,
          scopes: oidcScopes.split(/\s+/).filter(Boolean),
          groupsClaim: oidcGroupsClaim.trim() || 'groups',
          adminGroups: splitCSV(oidcAdminGroups),
          viewerGroups: splitCSV(oidcViewerGroups),
          defaultRole: oidcDefaultRole,
          label: oidcLabel.trim() || 'Sign in with SSO',
          localLogin: oidcLocalLogin,
        },
      });
      await load();
      oidcMsg = { ok: true, text: 'OIDC configuration saved and applied live.' };
    } catch (err) {
      oidcMsg = { ok: false, text: (err as Error).message };
    } finally {
      oidcBusy = false;
    }
  }

  async function testCoolifyNow(hostRow?: CoolifyHostConn) {
    coolBusy = true;
    coolMsg = null;
    try {
      const r = await api.testCoolify({
        hostId: hostRow?.hostId,
        url: (hostRow ? hostRow.url : coolUrl).trim(),
        token: (hostRow ? hostRow.token : coolToken) || undefined,
        allowPrivate: hostRow ? hostRow.allowPrivate : coolAllowPrivate,
      });
      coolMsg = { ok: true, text: `Connected to Coolify${r.version ? ` (${r.version})` : ''}.` };
    } catch (e) {
      coolMsg = { ok: false, text: (e as Error).message };
    } finally {
      coolBusy = false;
    }
  }

  async function saveCoolify(e: SubmitEvent) {
    e.preventDefault();
    coolBusy = true;
    coolMsg = null;
    try {
      await api.saveConnections({
        coolify: {
          url: coolUrl.trim(),
          token: coolClearToken ? '' : coolToken,
          clearToken: coolClearToken,
          allowPrivate: coolAllowPrivate,
          hosts: coolHosts.map((h) => ({
            ...h,
            token: h.clearToken ? '' : h.token,
            clearToken: !!h.clearToken,
          })),
        },
      });
      await load();
      coolMsg = { ok: true, text: 'Coolify API connection saved.' };
    } catch (err) {
      coolMsg = { ok: false, text: (err as Error).message };
    } finally {
      coolBusy = false;
    }
  }

  async function testRegRow(idx: number) {
    const r = regRows[idx];
    if (!r) return;
    r.status = 'Testing…';
    try {
      await api.testRegistry({ server: r.server, username: r.username, password: r.password || undefined, allowPrivate: true });
      r.status = 'Verified';
    } catch (e) {
      r.status = `Failed: ${(e as Error).message}`;
    }
  }

  async function saveRegistries(e: SubmitEvent) {
    e.preventDefault();
    regBusy = true;
    regMsg = null;
    try {
      await api.saveConnections({
        registries: regRows
          .filter((r) => !r.fromEnv && r.server.trim())
          .map((r) => ({ server: r.server.trim(), username: r.username.trim(), password: r.password })),
      });
      await load();
      regMsg = { ok: true, text: 'Registry logins saved and synced to connected agents over mTLS.' };
    } catch (err) {
      regMsg = { ok: false, text: (err as Error).message };
    } finally {
      regBusy = false;
    }
  }

  async function testGrafanaNow() {
    panelBusy = true;
    panelMsg = null;
    try {
      await api.testGrafana({ url: grafanaUrl.trim(), token: grafanaToken || undefined, allowPrivate: grafanaAllowPrivate });
      panelMsg = { ok: true, text: 'Grafana connection verified.' };
    } catch (e) {
      panelMsg = { ok: false, text: (e as Error).message };
    } finally {
      panelBusy = false;
    }
  }

  async function savePanels(e: SubmitEvent) {
    e.preventDefault();
    panelBusy = true;
    panelMsg = null;
    try {
      await api.saveConnections({
        panels: {
          grafanaUrl: grafanaUrl.trim(),
          grafanaToken: grafanaClearToken ? '' : grafanaToken,
          clearGrafanaToken: grafanaClearToken,
          grafanaFolderUid: grafanaFolderUid.trim(),
          grafanaAllowPrivate,
          beszelUrl: beszelUrl.trim(),
          links: customLinks.filter((l) => l.label.trim() && l.url.trim()),
        },
      });
      await load();
      panelMsg = { ok: true, text: 'Sister panel links saved.' };
    } catch (err) {
      panelMsg = { ok: false, text: (err as Error).message };
    } finally {
      panelBusy = false;
    }
  }
</script>

<div class="page">
  <section class="card card-pad">
    <h1 class="title">Connections</h1>
    <p class="meta">
      External services paired with Moorage. Every secret is write-only (<code>********</code>), included in encrypted <code>.mbk</code> backups,
      and overridable via <code>MOORAGE_*</code> environment variables.
    </p>
  </section>

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  {#if !data}
    <section class="card card-pad"><div class="skeleton" style="height:14px;width:45%"></div></section>
  {:else}
    <!-- 1. OIDC / SSO -->
    <form class="card card-pad block" onsubmit={saveOIDC}>
      <div class="head-row">
        <div>
          <h2 class="section-title">OIDC / Single Sign-On</h2>
          {#if data.oidc.redirectUri}<div class="meta mono">Callback URI: {data.oidc.redirectUri}</div>{/if}
        </div>
        <div class="badges">
          {#if data.oidc.fromEnv}<span class="pill env">Set via environment</span>{/if}
          {#if data.oidc.ready}<span class="pill on">Ready</span>{:else if data.oidc.issuer}<span class="pill warn-pill">Not ready</span>{/if}
        </div>
      </div>

      <div class="grid-3">
        <label class="field-wrap">
          <span>Issuer URL</span>
          <input class="field mono" bind:value={oidcIssuer} placeholder="https://auth.example.com/application/o/moorage/" disabled={data.oidc.fromEnv} />
        </label>
        <label class="field-wrap">
          <span>Client ID</span>
          <input class="field mono" bind:value={oidcClientId} disabled={data.oidc.fromEnv} autocomplete="off" />
        </label>
        <div class="field-wrap">
          <span class="label-row">
            <span>Client Secret</span>
            {#if oidcClearSecret}
              <span class="pill warn-pill">Will clear on save</span>
            {:else if data.oidc.clientSecretSet}
              <span class="pill on">Set (********)</span>
              {#if !data.oidc.fromEnv}
                <button
                  type="button"
                  class="link-btn"
                  onclick={() => {
                    oidcClientSecret = '';
                    oidcClearSecret = true;
                  }}>Clear</button
                >
              {/if}
            {/if}
          </span>
          <input
            class="field mono"
            type="password"
            bind:value={oidcClientSecret}
            oninput={() => (oidcClearSecret = false)}
            placeholder={data.oidc.clientSecretSet && !oidcClearSecret ? '********' : ''}
            disabled={data.oidc.fromEnv}
            autocomplete="new-password"
          />
        </div>
        <label class="field-wrap">
          <span>Scopes</span>
          <input class="field mono" bind:value={oidcScopes} disabled={data.oidc.fromEnv} />
        </label>
        <label class="field-wrap">
          <span>Groups claim</span>
          <input class="field mono" bind:value={oidcGroupsClaim} disabled={data.oidc.fromEnv} />
        </label>
        <label class="field-wrap">
          <span>Button label</span>
          <input class="field" bind:value={oidcLabel} disabled={data.oidc.fromEnv} />
        </label>
        <label class="field-wrap">
          <span>Admin groups (comma-separated)</span>
          <input class="field mono" bind:value={oidcAdminGroups} placeholder="moorage-admins" disabled={data.oidc.fromEnv} />
        </label>
        <label class="field-wrap">
          <span>Viewer groups (comma-separated)</span>
          <input class="field mono" bind:value={oidcViewerGroups} placeholder="moorage-viewers" disabled={data.oidc.fromEnv} />
        </label>
        <div class="field-wrap">
          <span>Default role (outside groups)</span>
          <Select
            label="Default OIDC role"
            value={oidcDefaultRole}
            disabled={data.oidc.fromEnv}
            items={[
              { value: 'none', label: 'Deny sign-in (none)' },
              { value: 'viewer', label: 'Viewer' },
            ]}
            onchange={(v) => (oidcDefaultRole = v)}
          />
        </div>
      </div>

      <label class="check-row">
        <input type="checkbox" bind:checked={oidcLocalLogin} disabled={data.oidc.fromEnv} />
        <span>Allow local username/password + TOTP login alongside SSO</span>
      </label>

      {#if data.oidc.lastError}<p class="error-text">{data.oidc.lastError}</p>{/if}
      {#if oidcMsg}<p class={oidcMsg.ok ? 'meta ok' : 'error-text'}>{oidcMsg.text}</p>{/if}

      <div class="actions">
        {#if !data.oidc.fromEnv}
          <button type="submit" class="btn btn-primary" disabled={oidcBusy}>Save OIDC</button>
        {/if}
        <button type="button" class="btn" disabled={oidcBusy || !oidcIssuer.trim() || !oidcClientId.trim()} onclick={testOIDCNow}>
          Test OIDC discovery
        </button>
      </div>
    </form>

    <!-- 2. Coolify API -->
    <AddonGate
      addonId="coolify-integration"
      featureTitle="Coolify API"
      featureDescription="Live stack names, start/stop/redeploy, build logs, and .env sync via Coolify API v1."
    >
      <form class="card card-pad block" onsubmit={saveCoolify}>
        <div class="head-row">
          <div>
            <h2 class="section-title">Coolify API</h2>
            <p class="meta">Powers the <code>coolify-integration</code> add-on (live names, start/stop/redeploy, build logs and .env sync).</p>
          </div>
          {#if data.coolify.fromEnv}<span class="pill env">Set via environment</span>{/if}
        </div>

        {#if !coolUrl.trim() && !data.coolify.tokenSet}
          <SetupHint
            title="Create a Coolify API token"
            detail="In Coolify open Keys & Tokens → API tokens, create a token with read + deploy permissions, then paste your Coolify URL and token below."
          />
        {/if}

        <div class="grid-2">
          <label class="field-wrap">
            <span>Default Coolify URL</span>
            <input class="field mono" bind:value={coolUrl} placeholder="https://coolify.example.com" disabled={data.coolify.fromEnv} />
          </label>
          <div class="field-wrap">
            <span class="label-row">
              <span>Bearer API Token</span>
              {#if coolClearToken}
                <span class="pill warn-pill">Will clear on save</span>
              {:else if data.coolify.tokenSet}
                <span class="pill on">Set (********)</span>
                {#if !data.coolify.fromEnv}
                  <button
                    type="button"
                    class="link-btn"
                    onclick={() => {
                      coolToken = '';
                      coolClearToken = true;
                    }}>Clear token</button
                  >
                {/if}
              {/if}
            </span>
            <input
              class="field mono"
              type="password"
              bind:value={coolToken}
              oninput={() => (coolClearToken = false)}
              placeholder={data.coolify.tokenSet && !coolClearToken ? '********' : ''}
              disabled={data.coolify.fromEnv}
              autocomplete="new-password"
            />
          </div>
        </div>

        <label class="check-row">
          <input type="checkbox" bind:checked={coolAllowPrivate} disabled={data.coolify.fromEnv} />
          <span>Allow private / Tailscale IP addresses (e.g. <code>http://100.x.y.z:8000</code>)</span>
        </label>

        {#if coolHosts.length > 0}
          <div class="sub-box">
            <span class="sub-lbl">Per-host Coolify overrides</span>
            {#each coolHosts as hc, idx (idx)}
              <div class="inline-row">
                <div class="host-sel">
                  <Select
                    label="Host for Coolify override"
                    value={hc.hostId}
                    items={app.hosts.map((h) => ({ value: h.id, label: h.name }))}
                    onchange={(v) => (hc.hostId = v)}
                  />
                </div>
                <input class="field mono grow" bind:value={hc.url} placeholder="https://coolify.host.example" />
                <input
                  class="field mono"
                  type="password"
                  bind:value={hc.token}
                  oninput={() => (hc.clearToken = false)}
                  placeholder={hc.clearToken ? '(clearing token on save)' : hc.tokenSet ? '********' : 'API token'}
                  autocomplete="new-password"
                />
                {#if hc.tokenSet && !hc.clearToken}
                  <button
                    type="button"
                    class="btn btn-sm btn-ghost"
                    onclick={() => {
                      hc.token = '';
                      hc.clearToken = true;
                    }}>Clear token</button
                  >
                {/if}
                <label class="check-row"><input type="checkbox" bind:checked={hc.allowPrivate} /><span>Private IP</span></label>
                <button type="button" class="btn btn-sm" onclick={() => testCoolifyNow(hc)}>Test</button>
                <button type="button" class="btn btn-sm btn-ghost danger" onclick={() => (coolHosts = coolHosts.filter((_, i) => i !== idx))}><Icon name="trash" /></button>
              </div>
            {/each}
          </div>
        {/if}

        {#if coolMsg}<p class={coolMsg.ok ? 'meta ok' : 'error-text'}>{coolMsg.text}</p>{/if}

        <div class="actions">
          <button type="submit" class="btn btn-primary" disabled={coolBusy}>Save Coolify</button>
          <button type="button" class="btn" disabled={coolBusy || !coolUrl.trim()} onclick={() => testCoolifyNow()}>Test connection</button>
          {#if app.hosts.length > 0}
            <button
              type="button"
              class="btn btn-ghost"
              onclick={() => (coolHosts = [...coolHosts, { hostId: app.hosts[0].id, url: '', token: '', allowPrivate: false }])}
            >
              <Icon name="plus" /> Per-host override
            </button>
          {/if}
        </div>
      </form>
    </AddonGate>

    <!-- 3. Docker Registries -->
    <form class="card card-pad block" onsubmit={saveRegistries}>
      <div class="head-row">
        <div>
          <h2 class="section-title">Docker Registries</h2>
          <p class="meta">Passed securely over mTLS to connected agents for image update checks and <code>docker compose pull</code>.</p>
        </div>
        <button
          type="button"
          class="btn btn-sm"
          onclick={() => (regRows = [...regRows, { server: 'ghcr.io', username: '', password: '', passwordSet: false }])}
        >
          <Icon name="plus" /> Add registry
        </button>
      </div>

      {#if regRows.length === 0}
        <p class="meta">No private registry credentials configured.</p>
      {:else}
        <div class="sub-box">
          {#each regRows as r, idx (idx)}
            <div class="inline-row">
              <input class="field mono" bind:value={r.server} placeholder="ghcr.io / docker.io / gitea.example.com" disabled={r.fromEnv} />
              <input class="field mono" bind:value={r.username} placeholder="Username" disabled={r.fromEnv} autocomplete="off" />
              <input
                class="field mono grow"
                type="password"
                bind:value={r.password}
                placeholder={r.passwordSet ? '******** (token set)' : 'Personal access token / password'}
                disabled={r.fromEnv}
                autocomplete="new-password"
              />
              {#if r.fromEnv}<span class="pill env">Set via environment</span>{/if}
              <button type="button" class="btn btn-sm" onclick={() => testRegRow(idx)}>Test</button>
              {#if !r.fromEnv}
                <button type="button" class="btn btn-sm btn-ghost danger" onclick={() => (regRows = regRows.filter((_, i) => i !== idx))}><Icon name="trash" /></button>
              {/if}
              {#if r.status}<span class="meta">{r.status}</span>{/if}
            </div>
          {/each}
        </div>
      {/if}

      {#if regMsg}<p class={regMsg.ok ? 'meta ok' : 'error-text'}>{regMsg.text}</p>{/if}

      <div class="actions">
        <button type="submit" class="btn btn-primary" disabled={regBusy}>Save registries</button>
      </div>
    </form>

    <!-- 4. Sister Panels -->
    <AddonGate
      addonId="prometheus-grafana"
      featureTitle="Sister panels (Grafana, Beszel & shortcuts)"
      featureDescription="Link Grafana, Beszel, and custom dashboards or push Prometheus dashboards directly."
    >
      <form class="card card-pad block" onsubmit={savePanels}>
        <div class="head-row">
          <div>
            <h2 class="section-title">Sister panels (Grafana, Beszel &amp; shortcuts)</h2>
            <p class="meta">Surfaces quick links on Host cards, Monitoring, <code>Ctrl+K</code> Command Palette, and enables 1-click Grafana dashboard push.</p>
          </div>
        </div>

        <div class="grid-3">
          <label class="field-wrap">
            <span class="label-row">
              <span>Grafana URL</span>
              {#if data.panels.fromEnv?.grafanaUrl}<span class="pill env">Set via environment</span>{/if}
            </span>
            <input class="field mono" bind:value={grafanaUrl} placeholder="https://grafana.example.com" disabled={data.panels.fromEnv?.grafanaUrl} />
          </label>

          <div class="field-wrap">
            <span class="label-row">
              <span>Grafana Service Account Token</span>
              {#if data.panels.fromEnv?.grafanaToken}
                <span class="pill env">Set via environment</span>
              {:else if grafanaClearToken}
                <span class="pill warn-pill">Will clear on save</span>
              {:else if data.panels.grafanaTokenSet}
                <span class="pill on">Set (********)</span>
                <button
                  type="button"
                  class="link-btn"
                  onclick={() => {
                    grafanaToken = '';
                    grafanaClearToken = true;
                  }}>Clear token</button
                >
              {/if}
            </span>
            <input
              class="field mono"
              type="password"
              bind:value={grafanaToken}
              oninput={() => (grafanaClearToken = false)}
              placeholder={data.panels.grafanaTokenSet && !grafanaClearToken ? '********' : 'glsa_... (optional, for Push to Grafana)'}
              disabled={data.panels.fromEnv?.grafanaToken}
              autocomplete="new-password"
            />
          </div>

          <label class="field-wrap">
            <span>Grafana Folder UID (optional)</span>
            <input class="field mono" bind:value={grafanaFolderUid} placeholder="general" />
          </label>

          <label class="field-wrap">
            <span class="label-row">
              <span>Beszel URL</span>
              {#if data.panels.fromEnv?.beszelUrl}<span class="pill env">Set via environment</span>{/if}
            </span>
            <input class="field mono" bind:value={beszelUrl} placeholder="https://beszel.example.com" disabled={data.panels.fromEnv?.beszelUrl} />
          </label>
        </div>

        <label class="check-row">
          <input type="checkbox" bind:checked={grafanaAllowPrivate} />
          <span>Allow private / Tailscale IP addresses when pushing dashboards to Grafana</span>
        </label>

        {#if customLinks.length > 0}
          <div class="sub-box">
            <span class="sub-lbl">Custom links in Command Palette</span>
            {#each customLinks as lk, idx (idx)}
              <div class="inline-row">
                <input class="field" bind:value={lk.label} placeholder="VictoriaLogs" />
                <input class="field mono grow" bind:value={lk.url} placeholder="https://logs.example.com" />
                <button type="button" class="btn btn-sm btn-ghost danger" onclick={() => (customLinks = customLinks.filter((_, i) => i !== idx))}><Icon name="trash" /></button>
              </div>
            {/each}
          </div>
        {/if}

        {#if panelMsg}<p class={panelMsg.ok ? 'meta ok' : 'error-text'}>{panelMsg.text}</p>{/if}

        <div class="actions">
          <button type="submit" class="btn btn-primary" disabled={panelBusy}>Save panels</button>
          {#if grafanaUrl.trim()}
            <button type="button" class="btn" disabled={panelBusy} onclick={testGrafanaNow}>Test Grafana</button>
          {/if}
          <button type="button" class="btn btn-ghost" onclick={() => (customLinks = [...customLinks, { label: '', url: '' }])}>
            <Icon name="plus" /> Custom link
          </button>
        </div>
      </form>
    </AddonGate>
  {/if}
</div>

<style>
  .page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
  }
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .block {
    display: grid;
    gap: 14px;
  }
  .head-row {
    display: flex;
    justify-content: space-between;
    align-items: flex-start;
    gap: 12px;
    flex-wrap: wrap;
  }
  .badges {
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
  }
  .grid-2 {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(260px, 1fr));
    gap: 12px 16px;
  }
  .grid-3 {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
    gap: 12px 16px;
  }
  .field-wrap {
    display: grid;
    gap: 5px;
    font-size: 13px;
    color: var(--mist);
    align-content: start;
  }
  .label-row {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .sub-box {
    display: grid;
    gap: 8px;
    padding: 10px 12px;
    border-radius: var(--r-md);
    background: var(--well);
  }
  .sub-lbl {
    font-size: 12px;
    font-weight: 600;
    color: var(--haze);
  }
  .inline-row {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .host-sel {
    min-width: 150px;
  }
  .link-btn {
    background: none;
    border: 0;
    padding: 0;
    color: var(--buoy-text);
    font: inherit;
    font-size: 12px;
    cursor: pointer;
  }
  .inline-row .field {
    width: auto;
    min-width: 150px;
  }
  .inline-row .grow {
    flex: 1 1 220px;
  }
  .check-row {
    display: flex;
    align-items: center;
    gap: 8px;
    font-size: 13px;
    color: var(--foam);
    cursor: pointer;
  }
  .actions {
    display: flex;
    align-items: center;
    gap: 8px;
    flex-wrap: wrap;
  }
  .ok {
    color: var(--afloat-text);
  }
  .danger {
    color: var(--adrift-text);
  }
  .pill {
    display: inline-flex;
    align-items: center;
    font-size: 11px;
    font-weight: 500;
    padding: 2px 8px;
    border-radius: 999px;
    background: var(--rope);
    color: var(--mist);
  }
  .pill.on {
    background: color-mix(in srgb, var(--afloat) 16%, transparent);
    color: var(--afloat-text, var(--afloat));
  }
  .pill.warn-pill {
    background: var(--signal-wash);
    color: var(--signal-text);
  }
  .pill.env {
    background: var(--buoy-wash);
    color: var(--buoy-text);
  }
</style>

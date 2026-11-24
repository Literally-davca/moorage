<script lang="ts">
  import { untrack } from 'svelte';
  import { api, type LinkResult, type Role, type UserInfo, type UsersPage } from '../lib/api';
  import Icon from '../lib/Icon.svelte';
  import Confirm from '../lib/Confirm.svelte';

  let page = $state<UsersPage | null>(null);
  let error = $state('');

  let username = $state('');
  let displayName = $state('');
  let role = $state<Role>('viewer');
  let creating = $state(false);
  let createError = $state('');

  /** The last issued link, shown once. */
  let issued = $state<LinkResult | null>(null);
  let copied = $state('');
  let rowError = $state<Record<number, string>>({});

  let confirming = $state<{ user: UserInfo; kind: 'delete' | 'disable' | 'sessions' } | null>(null);
  let confirmOpen = $state(false);

  async function load() {
    try {
      page = await api.users();
      error = '';
    } catch (e) {
      error = (e as Error).message;
    }
  }
  $effect(() => {
    untrack(() => void load());
    const t = setInterval(() => !document.hidden && void load(), 30000);
    return () => clearInterval(t);
  });

  function linkURL(r: LinkResult): string {
    return r.url ?? `${location.origin}/#/invite/${r.link.token}`;
  }

  async function copy(text: string, what: string) {
    try {
      await navigator.clipboard.writeText(text);
      copied = what;
    } catch {
      copied = 'Select the text and copy it by hand.';
    }
  }

  async function create(e: SubmitEvent) {
    e.preventDefault();
    creating = true;
    createError = '';
    try {
      issued = await api.createUser(username.trim(), role, displayName.trim());
      copied = '';
      username = displayName = '';
      role = 'viewer';
      await load();
    } catch (err) {
      createError = (err as Error).message;
    } finally {
      creating = false;
    }
  }

  async function act(u: UserInfo, fn: () => Promise<unknown>) {
    rowError = { ...rowError, [u.id]: '' };
    try {
      await fn();
    } catch (err) {
      rowError = { ...rowError, [u.id]: (err as Error).message };
    }
    // Reload either way, so a refused role change puts the select back to the real role.
    page = null;
    await load();
  }

  async function newLink(u: UserInfo) {
    rowError = { ...rowError, [u.id]: '' };
    try {
      issued = await api.resetUser(u.id);
      copied = '';
      await load();
    } catch (err) {
      rowError = { ...rowError, [u.id]: (err as Error).message };
    }
  }

  function confirmed() {
    const c = confirming;
    if (!c) return;
    if (c.kind === 'delete') act(c.user, () => api.deleteUser(c.user.id));
    if (c.kind === 'disable') act(c.user, () => api.disableUser(c.user.id));
    if (c.kind === 'sessions') act(c.user, () => api.revokeUserSessions(c.user.id));
  }

  const confirmText = $derived.by(() => {
    const c = confirming;
    if (!c) return { title: '', body: '', label: '' };
    const n = c.user.username;
    if (c.kind === 'delete')
      return c.user.status === 'invited'
        ? { title: `Revoke the invite for ${n}?`, body: 'The link stops working and the user is removed.', label: 'Revoke invite' }
        : { title: `Delete ${n}?`, body: 'Their sessions end now. The audit log keeps their past actions.', label: 'Delete user' };
    if (c.kind === 'disable')
      return { title: `Disable ${n}?`, body: 'Their sessions end now and they can’t sign in, also not through SSO or the proxy, until you enable them.', label: 'Disable' };
    return { title: `Sign ${n} out everywhere?`, body: 'Every session of this user ends now.', label: 'Sign out' };
  });

  const when = new Intl.DateTimeFormat(undefined, { month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
  const fmt = (iso?: string) => (iso ? when.format(new Date(iso)) : '');

  const statusClass: Record<UserInfo['status'], string> = { active: 'up', invited: 'degraded', disabled: 'down' };
  const statusText: Record<UserInfo['status'], string> = { active: 'Active', invited: 'Invited', disabled: 'Disabled' };
  const sourceText: Record<string, string> = { local: 'Local', oidc: 'SSO', proxy: 'Proxy' };
  const list = (v?: string[]) => (v && v.length ? v.join(', ') : 'none');
</script>

<div class="page">
  <section class="card card-pad head">
    <h1 class="title">Users</h1>
    <p class="meta">
      Admins change things, viewers only read. A new local user gets a one-time link to choose their own password and add an
      authenticator app; you never see their password. SSO and proxy users appear here after their first sign-in.
    </p>
  </section>

  {#if error}<p class="error-text" role="alert">{error}</p>{/if}

  <section class="card card-pad grid1" aria-label="Invite a user">
    <p class="section-title">Invite a local user</p>
    <form class="add" onsubmit={create}>
      <div class="fields">
        <div>
          <label class="label" for="nu">Username</label>
          <input id="nu" class="field" bind:value={username} autocomplete="off" spellcheck="false" required />
        </div>
        <div>
          <label class="label" for="nd">Display name <span class="meta">(optional)</span></label>
          <input id="nd" class="field" bind:value={displayName} autocomplete="off" />
        </div>
        <div>
          <label class="label" for="nr">Role</label>
          <select id="nr" class="field" bind:value={role}>
            <option value="viewer">Viewer</option>
            <option value="admin">Admin</option>
          </select>
        </div>
      </div>
      {#if createError}<p class="error-text" role="alert">{createError}</p>{/if}
      <div class="row"><button class="btn btn-primary" aria-busy={creating}><Icon name="plus" /> Create invite link</button></div>
    </form>
    {#if issued}
      <div class="issued" role="status">
        <p class="name">
          {issued.link.kind === 'reset' ? 'Reset link' : 'Invite link'} for {issued.user.username}
        </p>
        <p class="meta">
          Shown only now. Send it to them yourself; it works once, until {fmt(issued.link.expires)}.
          {#if issued.link.kind === 'reset'}Their password and code change, and their sessions end, only when they finish it.{/if}
        </p>
        <div class="copyrow">
          <code class="mono linkval">{linkURL(issued)}</code>
          <button type="button" class="btn" onclick={() => copy(linkURL(issued!), 'Link copied')}><Icon name="copy" /> Copy</button>
        </div>
        {#if copied}<p class="meta">{copied}</p>{/if}
        <div><button type="button" class="btn btn-ghost" onclick={() => (issued = null)}>Done</button></div>
      </div>
    {/if}
  </section>

  <section class="card card-pad grid1" aria-label="All users">
    <p class="section-title">All users</p>
    {#if page === null}
      <span class="skeleton" style="height:12px;width:220px;display:block"></span>
    {:else}
      <ul class="list">
        {#each page.users as u (u.id)}
          {@const self = u.id === page.self}
          <li class="item">
            <div class="item-main">
              <p class="name">
                {u.username}
                {#if u.displayName}<span class="meta">· {u.displayName}</span>{/if}
                {#if self}<span class="badge">You</span>{/if}
              </p>
              <p class="meta">
                <span class="badge {statusClass[u.status]}">{statusText[u.status]}</span>
                · {u.role === 'admin' ? 'Admin' : 'Viewer'} · {sourceText[u.source] ?? u.source}
                {#if u.source === 'local' && u.status === 'active'} · {u.totp ? 'two-factor on' : 'no two-factor'}{/if}
              </p>
              <p class="meta">
                {u.lastLoginAt ? `Last sign-in ${fmt(u.lastLoginAt)}` : 'Never signed in'}
                {#if u.createdBy} · invited by {u.createdBy}{/if}
                {#if u.email} · {u.email}{/if}
              </p>
              {#if u.linkExpires}
                <p class="meta">Open {u.linkKind} link until {fmt(u.linkExpires)}</p>
              {/if}
              {#if rowError[u.id]}<p class="error-text" role="alert">{rowError[u.id]}</p>{/if}
            </div>
            {#if !self}
              <div class="item-actions">
                {#if u.source === 'local'}
                  <label class="sr-only" for="role-{u.id}">Role of {u.username}</label>
                  <select id="role-{u.id}" class="field rolesel" value={u.role}
                    onchange={(e) => act(u, () => api.setUserRole(u.id, (e.currentTarget as HTMLSelectElement).value as Role))}>
                    <option value="viewer">Viewer</option>
                    <option value="admin">Admin</option>
                  </select>
                  <button type="button" class="btn btn-ghost" onclick={() => newLink(u)}>
                    <Icon name="key" /> {u.status === 'invited' ? 'New invite link' : 'Reset link'}
                  </button>
                {/if}
                {#if u.status !== 'invited'}
                  {#if u.status === 'disabled'}
                    <button type="button" class="btn btn-ghost" onclick={() => act(u, () => api.enableUser(u.id))}>Enable</button>
                  {:else}
                    <button type="button" class="btn btn-ghost" onclick={() => { confirming = { user: u, kind: 'disable' }; confirmOpen = true; }}>Disable</button>
                    <button type="button" class="btn btn-ghost" onclick={() => { confirming = { user: u, kind: 'sessions' }; confirmOpen = true; }}>
                      <Icon name="logout" /> Sign out everywhere
                    </button>
                  {/if}
                {/if}
                <button type="button" class="btn btn-ghost btn-danger btn-icon"
                  aria-label={u.status === 'invited' ? `Revoke invite for ${u.username}` : `Delete ${u.username}`}
                  title={u.status === 'invited' ? 'Revoke invite' : 'Delete user'}
                  onclick={() => { confirming = { user: u, kind: 'delete' }; confirmOpen = true; }}><Icon name="trash" /></button>
              </div>
            {/if}
          </li>
        {/each}
      </ul>
      {#if page.users.some((u) => u.source !== 'local')}
        <p class="meta">The role of SSO and proxy users comes from their groups at every sign-in. Disabling them wins over the identity provider.</p>
      {/if}
    {/if}
  </section>

  {#if page}
    {@const o = page.oidc}
    <section class="card card-pad grid1" aria-label="Single sign-on">
      <p class="section-title">Single sign-on (OIDC)</p>
      {#if !o.configured}
        <p class="meta">Not set up. Set <code>MOORAGE_OIDC_ISSUER</code>, <code>MOORAGE_OIDC_CLIENT_ID</code> and the client secret, then restart Moorage.</p>
      {:else}
        <p class="meta">
          <span class="badge {o.ready ? 'up' : 'down'}">{o.ready ? 'Working' : 'Not working'}</span>
          {o.ready ? ' The sign-in page shows “' + (o.label ?? '') + '”.' : ' The sign-in page hides the SSO button.'}
        </p>
        {#if o.lastError}
          <p class="error-text">{o.lastError}{o.lastErrorAt ? ` (${fmt(o.lastErrorAt)})` : ''}</p>
        {/if}
        <dl class="kv">
          <dt>Issuer</dt><dd class="mono">{o.issuer}</dd>
          <dt>Client ID</dt><dd class="mono">{o.clientId}</dd>
          <dt>Redirect URI</dt>
          <dd>
            {#if o.redirectUri}
              <span class="copyrow"><code class="mono linkval">{o.redirectUri}</code>
                <button type="button" class="btn btn-ghost" onclick={() => copy(o.redirectUri!, 'Redirect URI copied')}><Icon name="copy" /> Copy</button></span>
            {:else}<span class="meta">Set MOORAGE_PUBLIC_URL</span>{/if}
          </dd>
          <dt>Scopes</dt><dd class="mono">{(o.scopes ?? []).join(' ')}</dd>
          <dt>Groups claim</dt><dd class="mono">{o.groupsClaim}</dd>
          <dt>Admin groups</dt><dd>{list(o.adminGroups)}</dd>
          <dt>Viewer groups</dt><dd>{list(o.viewerGroups)}</dd>
          <dt>Other users</dt><dd>{o.defaultRole === 'viewer' ? 'Viewer' : 'No access'}</dd>
          <dt>Password sign-in</dt><dd>{o.localLogin ? 'Shown' : 'Hidden; local admins use /#/login?local=1'}</dd>
        </dl>
      {/if}
    </section>
    {#if page.proxy.enabled}
      {@const p = page.proxy}
      <section class="card card-pad grid1" aria-label="Forward-auth">
        <p class="section-title">Forward-auth (trusted proxy)</p>
        <dl class="kv">
          <dt>Trusted proxies</dt><dd class="mono">{p.trusted.join(', ')}</dd>
          <dt>User header</dt><dd class="mono">{p.userHeader}</dd>
          <dt>Groups header</dt><dd class="mono">{p.groupsHeader || 'none'}</dd>
          <dt>Admin groups</dt><dd>{list(p.adminGroups)}</dd>
          <dt>Viewer groups</dt><dd>{list(p.viewerGroups)}</dd>
          <dt>Other users</dt><dd>{p.defaultRole === 'viewer' ? 'Viewer' : 'No access'}</dd>
        </dl>
      </section>
    {/if}
  {/if}
</div>

<Confirm bind:open={confirmOpen} title={confirmText.title} body={confirmText.body} confirmLabel={confirmText.label}
  danger={confirming?.kind !== 'sessions'} onConfirm={confirmed} />

<style>
  .page {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
  }
  .head {
    display: grid;
    gap: 6px;
  }
  .title {
    font-size: 22px;
    font-weight: 600;
    letter-spacing: -0.015em;
  }
  .grid1 {
    display: grid;
    grid-template-columns: minmax(0, 1fr);
    gap: 12px;
  }
  .add {
    display: grid;
    gap: 10px;
  }
  .fields {
    display: grid;
    grid-template-columns: repeat(auto-fit, minmax(180px, 1fr));
    gap: 10px;
  }
  .row {
    display: flex;
    justify-content: flex-end;
  }
  .issued {
    display: grid;
    gap: 8px;
    padding: 12px;
    border-radius: var(--r-md);
    background: var(--well);
  }
  .copyrow {
    display: flex;
    gap: 8px;
    align-items: center;
    flex-wrap: wrap;
    min-width: 0;
  }
  .linkval {
    font-size: 12px;
    overflow-wrap: anywhere;
    min-width: 0;
    flex: 1 1 200px;
    user-select: all;
  }
  .list {
    list-style: none;
    margin: 0;
    padding: 0;
    display: grid;
  }
  .item {
    display: flex;
    justify-content: space-between;
    gap: 12px;
    flex-wrap: wrap;
    padding: 12px 0;
    border-top: 1px solid var(--rope-strong);
  }
  .item:first-child {
    border-top: 0;
    padding-top: 0;
  }
  .item-main {
    display: grid;
    gap: 4px;
    min-width: 0;
    flex: 1 1 260px;
  }
  .item-main .error-text {
    overflow-wrap: anywhere;
  }
  .name {
    font-weight: 600;
    font-size: 14px;
    overflow-wrap: anywhere;
  }
  .item-actions {
    display: flex;
    gap: 6px;
    flex-wrap: wrap;
    align-items: flex-start;
  }
  .rolesel {
    width: auto;
    height: 32px;
  }
  .kv {
    display: grid;
    grid-template-columns: max-content minmax(0, 1fr);
    gap: 6px 16px;
    margin: 0;
    font-size: 13px;
  }
  .kv dt {
    color: var(--haze);
  }
  .kv dd {
    margin: 0;
    overflow-wrap: anywhere;
    min-width: 0;
  }
  @media (max-width: 480px) {
    .kv {
      grid-template-columns: minmax(0, 1fr);
    }
    .kv dd {
      margin-bottom: 6px;
    }
  }
</style>

<script lang="ts">
  import { untrack } from 'svelte';
  import { hostPath, wsURL, type ShellName } from '../lib/api';
  import Icon from '../lib/Icon.svelte';

  let {
    mode = 'stack',
    stack = '',
    containerId = '',
    containerName = '',
    service = '',
    hostId = '',
    hostName = '',
    onClose,
  }: {
    mode?: 'stack' | 'container' | 'host';
    stack?: string;
    containerId?: string;
    containerName?: string;
    service?: string;
    hostId?: string;
    hostName?: string;
    onClose: () => void;
  } = $props();

  let shell = $state<ShellName>(untrack(() => (mode === 'host' ? 'bash' : 'sh')));
  let phase = $state<'connecting' | 'connected' | 'ended'>('connecting');
  let endText = $state('');
  let box: HTMLDivElement;

  const shells = $derived<ShellName[]>(mode === 'host' ? ['bash', 'sh', 'ash'] : ['sh', 'bash', 'ash']);

  const targetKey = $derived(JSON.stringify([mode, stack, containerId, containerName, hostId, shell]));

  $effect(() => {
    const [m, st, cid, ctr, hid, sh] = JSON.parse(targetKey) as [
      'stack' | 'container' | 'host',
      string,
      string,
      string,
      string,
      ShellName,
    ];

    phase = 'connecting';
    endText = '';

    let disposed = false;
    let ws: WebSocket | null = null;
    let term: import('@xterm/xterm').Terminal | null = null;
    let ro: ResizeObserver | null = null;
    const enc = new TextEncoder();

    (async () => {
      const [{ Terminal }, { FitAddon }] = await Promise.all([
        import('@xterm/xterm'),
        import('@xterm/addon-fit'),
        import('@xterm/xterm/css/xterm.css'),
      ]);
      if (disposed || !box) return;

      box.innerHTML = '';
      const t = new Terminal({
        cursorBlink: true,
        fontFamily: "'JetBrains Mono', ui-monospace, SFMono-Regular, Menlo, Consolas, monospace",
        fontSize: 13,
        lineHeight: 1.35,
        scrollback: 2000,
        theme: {
          background: '#0b1418',
          foreground: '#e8eeee',
          cursor: '#ff7a45',
          cursorAccent: '#0b1418',
          selectionBackground: 'rgba(255, 122, 69, 0.28)',
          black: '#0b1418',
          red: '#f2555a',
          green: '#4cc38a',
          yellow: '#f5c451',
          blue: '#6cb6d9',
          magenta: '#c59be0',
          cyan: '#5fc4c4',
          white: '#b7c4c6',
          brightBlack: '#5e7074',
          brightRed: '#ff8a8e',
          brightGreen: '#7fd8ab',
          brightYellow: '#f7d27a',
          brightBlue: '#9fd0e8',
          brightMagenta: '#dcbcef',
          brightCyan: '#8fdada',
          brightWhite: '#e8eeee',
        },
      });
      const fit = new FitAddon();
      t.loadAddon(fit);
      t.open(box);
      try {
        fit.fit();
      } catch {
        /* container may still be laying out */
      }
      t.focus();
      term = t;

      const cols = Math.max(1, Math.min(500, t.cols || 80));
      const rows = Math.max(1, Math.min(500, t.rows || 24));
      let url = '';
      if (m === 'host') {
        const q = new URLSearchParams({
          shell: sh,
          cols: String(cols),
          rows: String(rows),
        });
        const path = hid ? `/api/hosts/${encodeURIComponent(hid)}/ws/host-terminal?${q.toString()}` : hostPath(`/ws/host-terminal?${q.toString()}`);
        url = wsURL(path);
      } else if (m === 'container') {
        const ref = cid || ctr;
        const q = new URLSearchParams({
          shell: sh,
          cols: String(cols),
          rows: String(rows),
        });
        const path = hid
          ? `/api/hosts/${encodeURIComponent(hid)}/containers/${encodeURIComponent(ref)}/terminal?${q.toString()}`
          : hostPath(`/containers/${encodeURIComponent(ref)}/terminal?${q.toString()}`);
        url = wsURL(path);
      } else {
        const q = new URLSearchParams({
          container: ctr,
          shell: sh,
          cols: String(cols),
          rows: String(rows),
        });
        url = wsURL(hostPath(`/stacks/${encodeURIComponent(st)}/terminal?${q.toString()}`));
      }

      const sock = new WebSocket(url);
      sock.binaryType = 'arraybuffer';
      ws = sock;

      let sawExit = false;
      let sawError = '';

      sock.onopen = () => {
        if (disposed) return;
        phase = 'connected';
        try {
          fit.fit();
          sock.send(JSON.stringify({ type: 'resize', cols: t.cols, rows: t.rows }));
        } catch {
          /* ignore */
        }
      };

      sock.onmessage = (ev) => {
        if (disposed) return;
        if (ev.data instanceof ArrayBuffer) {
          t.write(new Uint8Array(ev.data));
          return;
        }
        if (typeof ev.data === 'string') {
          try {
            const ctrl = JSON.parse(ev.data) as { type: string; code?: number; message?: string };
            if (ctrl.type === 'error' && ctrl.message) {
              sawError = ctrl.message;
              t.write(`\r\n\x1b[31m${ctrl.message}\x1b[0m\r\n`);
            } else if (ctrl.type === 'exit') {
              sawExit = true;
              const code = ctrl.code ?? 0;
              phase = 'ended';
              endText = sawError ? `${sawError} (exit ${code})` : `Ended (exit ${code})`;
            }
          } catch {
            /* ignore malformed control frame */
          }
        }
      };

      sock.onerror = () => {
        if (disposed) return;
        if (!sawExit && !sawError) {
          sawError = 'Connection refused or closed.';
        }
      };

      sock.onclose = () => {
        if (disposed) return;
        phase = 'ended';
        if (!endText) {
          endText = sawError || 'Disconnected';
        }
      };

      t.onData((data) => {
        if (sock.readyState === WebSocket.OPEN) {
          sock.send(enc.encode(data));
        }
      });
      t.onBinary((data) => {
        if (sock.readyState === WebSocket.OPEN) {
          const bytes = new Uint8Array(data.length);
          for (let i = 0; i < data.length; i++) bytes[i] = data.charCodeAt(i) & 0xff;
          sock.send(bytes);
        }
      });

      ro = new ResizeObserver(() => {
        if (disposed || !term) return;
        try {
          fit.fit();
          if (sock.readyState === WebSocket.OPEN) {
            sock.send(JSON.stringify({ type: 'resize', cols: t.cols, rows: t.rows }));
          }
        } catch {
          /* ignore */
        }
      });
      ro.observe(box);
    })();

    return () => {
      disposed = true;
      ro?.disconnect();
      if (ws && (ws.readyState === WebSocket.OPEN || ws.readyState === WebSocket.CONNECTING)) {
        ws.close();
      }
      term?.dispose();
    };
  });

  const titleLabel = $derived(mode === 'host' ? hostName || 'Host OS' : containerName || containerId);
</script>

<section class="card term-card" aria-label={mode === 'host' ? 'Host terminal' : 'Container terminal'}>
  <header class="toolbar">
    <div class="ident">
      <Icon name="terminal" />
      <span class="cname mono">{titleLabel}</span>
      {#if mode === 'host'}
        <span class="badge">Host OS</span>
      {:else if service && service !== containerName}
        <span class="meta">· {service}</span>
      {/if}
    </div>

    <div class="controls">
      <div class="seg" role="group" aria-label="Shell">
        {#each shells as s (s)}
          <button
            type="button"
            class="mono"
            class:active={shell === s}
            onclick={() => (shell = s)}
          >{s}</button>
        {/each}
      </div>

      <span class="status-pill {phase}">
        <span class="dot {phase === 'connected' ? 'up' : phase === 'connecting' ? 'degraded' : 'paused'}"></span>
        <span>
          {#if phase === 'connecting'}
            Connecting…
          {:else if phase === 'connected'}
            Connected ({shell})
          {:else}
            {endText || 'Ended'}
          {/if}
        </span>
      </span>

      <button type="button" class="btn" onclick={onClose}>
        Disconnect
      </button>
    </div>
  </header>

  <div class="term-wrap" bind:this={box}></div>
</section>

<style>
  .term-card {
    overflow: hidden;
    display: grid;
    grid-template-rows: auto 1fr;
  }
  .toolbar {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 10px;
    padding: 10px 14px;
    border-bottom: 1px solid var(--rope);
    flex-wrap: wrap;
  }
  .ident {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
    flex-wrap: wrap;
  }
  .cname {
    font-weight: 600;
    font-size: 13px;
    color: var(--foam);
  }
  .controls {
    display: flex;
    align-items: center;
    gap: 10px;
    flex-wrap: wrap;
  }
  .seg {
    display: inline-flex;
    background: var(--well);
    border-radius: var(--r-md);
    padding: 2px;
    box-shadow: inset 0 0 0 1px var(--rope);
  }
  .seg button {
    border: 0;
    background: transparent;
    color: var(--mist);
    font: 500 12px var(--mono);
    padding: 4px 10px;
    border-radius: var(--r-sm);
    cursor: pointer;
  }
  .seg button.active {
    background: var(--buoy-wash);
    color: var(--foam);
  }
  .status-pill {
    display: inline-flex;
    align-items: center;
    gap: 6px;
    font-size: 12px;
    color: var(--mist);
    padding: 3px 10px;
    border-radius: 999px;
    background: var(--well);
  }
  .status-pill.ended {
    color: var(--signal-text);
    background: var(--signal-wash);
  }
  .term-wrap {
    height: 380px;
    min-height: 240px;
    padding: 8px 10px;
    background: #0b1418;
    overflow: hidden;
  }

  @media (max-width: 767px) {
    .toolbar {
      padding: 8px 10px;
    }
    .term-wrap {
      height: 300px;
      padding: 6px;
    }
  }
</style>

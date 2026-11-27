// Commands that install a Moorage agent (M19F, Hosts → Add host). Every tab is built from
// the same fields: the install script, a heredoc without any script, compose.yaml,
// docker run and the token alone. The token goes in the environment, never as an
// argument (ps shows arguments to every user of the host).

export type AgentRuntime = 'docker' | 'podman' | 'podman-rootless';

export interface AgentFields {
  hub: string;
  token: string;
  tag: string; // image tag: the hub's version, or latest for a dev build
  runtime: AgentRuntime;
  stacks: string;
  network: 'bridge' | 'host';
  hostIp: string;
  readOnly: boolean;
  tailscale: boolean; // Tailscale Serve: mount the tailscaled socket (M19J)
  data: string; // '' = named volume, else a folder
  dir: string;
  scriptRef: string; // git ref the script comes from (the hub's tag, main for dev)
}

export const defaultFields = (hub: string, token: string, version: string): AgentFields => {
  const release = /^\d+\.\d+\.\d+$/.test(version);
  return {
    hub,
    token,
    tag: release ? version : 'latest',
    runtime: 'docker',
    stacks: '/opt/stacks',
    network: 'bridge',
    hostIp: '',
    readOnly: false,
    tailscale: false,
    data: '',
    dir: '/opt/moorage-agent',
    scriptRef: release ? 'v' + version : 'main',
  };
};

/** fieldError returns a message for an invalid field, or ''. */
export function fieldError(f: AgentFields): string {
  if (!/^[^\s:]+:\d{1,5}$/.test(f.hub) && !/^\[[0-9a-fA-F:]+\]:\d{1,5}$/.test(f.hub)) return 'Hub address must be HOST:PORT.';
  if (!f.stacks.startsWith('/') || /\s/.test(f.stacks)) return 'The stacks folder must be an absolute path without spaces.';
  if (f.data && (!f.data.startsWith('/') || /\s/.test(f.data))) return 'The data folder must be an absolute path without spaces.';
  if (!f.dir.startsWith('/') || /\s/.test(f.dir)) return 'The agent folder must be an absolute path without spaces.';
  if (f.hostIp && /\s/.test(f.hostIp)) return 'The host address has a space in it.';
  if (!/^[\w.-]+$/.test(f.tag)) return 'The image tag is invalid.';
  return '';
}

/** The host's tailscaled socket, mounted at the same path (MOORAGE_TAILSCALE_SOCKET default). */
const TS_SOCK = '/var/run/tailscale/tailscaled.sock';

const q = (s: string) => `'${s.replace(/'/g, `'\\''`)}'`;

function socket(r: AgentRuntime): string {
  if (r === 'podman') return '/run/podman/podman.sock';
  if (r === 'podman-rootless') return '/run/user/1000/podman/podman.sock';
  return '/var/run/docker.sock';
}

function composeCmd(r: AgentRuntime): string {
  return r === 'docker' ? 'docker compose' : 'podman compose';
}

export function image(f: AgentFields): string {
  return `ghcr.io/literally-davca/moorage:${f.tag}`;
}

export function installCommand(f: AgentFields): string {
  const args = [`--hub ${q(f.hub)}`];
  if (f.runtime !== 'docker') args.push(`--runtime ${f.runtime}`);
  if (f.stacks !== '/opt/stacks') args.push(`--stacks ${q(f.stacks)}`);
  if (f.network === 'host') args.push('--network host');
  if (f.hostIp) args.push(`--host-ip ${q(f.hostIp)}`);
  if (f.readOnly) args.push('--read-only');
  if (f.tailscale) args.push('--tailscale-serve');
  if (f.data) args.push(`--data ${q(f.data)}`);
  if (f.dir !== '/opt/moorage-agent') args.push(`--dir ${q(f.dir)}`);
  if (f.tag !== 'latest') args.push(`--image ${q(image(f))}`);
  const url = `https://raw.githubusercontent.com/Literally-davca/moorage/${f.scriptRef}/scripts/install-agent.sh`;
  return `curl -fsSL ${url} | MOORAGE_JOIN_TOKEN=${q(f.token)} sh -s -- ${args.join(' ')}`;
}

export function composeYaml(f: AgentFields, withToken = true): string {
  const lines = [
    'services:',
    '  moorage-agent:',
    `    image: ${image(f)}`,
    '    container_name: moorage-agent',
    '    command: agent',
    '    restart: unless-stopped',
  ];
  if (f.network === 'host') lines.push('    network_mode: host');
  lines.push(
    '    volumes:',
    `      - ${socket(f.runtime)}:/var/run/docker.sock`,
    `      - ${f.data ? f.data : 'moorage-agent'}:/data`,
    '      # same path inside and outside, like on the hub',
    `      - ${f.stacks}:${f.stacks}`,
    ...(f.tailscale ? [`      - ${TS_SOCK}:${TS_SOCK}`] : []),
    '    environment:',
    `      MOORAGE_HUB: "${f.hub}"`,
    `      MOORAGE_JOIN_TOKEN: ${withToken ? `"${f.token}"` : '${MOORAGE_JOIN_TOKEN:-}'}`,
    `      MOORAGE_STACKS_DIR: "${f.stacks}"`,
  );
  if (f.readOnly) lines.push('      MOORAGE_READ_ONLY: "true"');
  if (f.tailscale) lines.push('      MOORAGE_TAILSCALE_SERVE: "true"');
  if (f.hostIp) lines.push(`      MOORAGE_HOST_IP: "${f.hostIp}"`);
  if (!f.data) lines.push('', 'volumes:', '  moorage-agent:');
  return lines.join('\n') + '\n';
}

/** A heredoc that writes compose.yaml and starts it: nothing else is downloaded. */
export function noScriptCommand(f: AgentFields): string {
  const sudo = f.runtime === 'podman-rootless' ? '' : 'sudo ';
  return [
    `${sudo}mkdir -p ${q(f.dir)} ${q(f.stacks)} && cd ${q(f.dir)}`,
    `${sudo}tee compose.yaml >/dev/null <<'EOF'`,
    composeYaml(f, false).trimEnd(),
    'EOF',
    `MOORAGE_JOIN_TOKEN=${q(f.token)} ${sudo}${composeCmd(f.runtime)} up -d`,
  ].join('\n');
}

export function dockerRun(f: AgentFields): string {
  const cli = f.runtime === 'docker' ? 'docker' : 'podman';
  const parts = [
    `${cli} run -d --name moorage-agent --restart unless-stopped`,
    f.network === 'host' ? '--network host' : '',
    `-v ${socket(f.runtime)}:/var/run/docker.sock`,
    `-v ${f.data ? f.data : 'moorage-agent'}:/data`,
    `-v ${f.stacks}:${f.stacks}`,
    f.tailscale ? `-v ${TS_SOCK}:${TS_SOCK}` : '',
    `-e MOORAGE_HUB=${q(f.hub)}`,
    `-e MOORAGE_JOIN_TOKEN=${q(f.token)}`,
    `-e MOORAGE_STACKS_DIR=${q(f.stacks)}`,
    f.readOnly ? '-e MOORAGE_READ_ONLY=true' : '',
    f.tailscale ? '-e MOORAGE_TAILSCALE_SERVE=true' : '',
    f.hostIp ? `-e MOORAGE_HOST_IP=${q(f.hostIp)}` : '',
    `${image(f)} agent`,
  ].filter(Boolean);
  return parts.join(' \\\n  ');
}

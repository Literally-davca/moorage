// Ready-made config files for MCP clients (MCP Server add-on): the whole JSON a user can
// download or paste into the client's file. The token is written in the file because
// these clients read headers only from it; the UI says so and warns about committing it.

export type MCPClientId = 'claude-code' | 'claude-desktop' | 'cursor' | 'vscode';

export interface MCPClient {
  id: MCPClientId;
  label: string;
  /** file name used for the download */
  file: string;
  /** where the file goes, one line per OS or scope */
  where: string[];
  json: string;
}

/** Placeholder used until a token is generated (tokens are shown only once). */
export const TOKEN_PLACEHOLDER = 'PASTE-YOUR-MOORAGE-MCP-TOKEN';

const SERVER = 'moorage';

/** absoluteEndpoint turns "/mcp" into a full URL (the public URL isn't always set). */
export function absoluteEndpoint(endpoint: string, origin: string): string {
  const e = endpoint || '/mcp';
  return /^https?:\/\//.test(e) ? e : origin.replace(/\/+$/, '') + (e.startsWith('/') ? e : '/' + e);
}

const pretty = (v: unknown) => JSON.stringify(v, null, 2) + '\n';

export function mcpClients(endpoint: string, token: string): MCPClient[] {
  const auth = `Bearer ${token || TOKEN_PLACEHOLDER}`;
  return [
    {
      id: 'claude-code',
      label: 'Claude Code',
      file: '.mcp.json',
      where: [
        'Project: .mcp.json in the project folder (shared with everyone who has the folder).',
        'Or run: claude mcp add --transport http moorage <url> --header "Authorization: Bearer <token>"',
      ],
      json: pretty({ mcpServers: { [SERVER]: { type: 'http', url: endpoint, headers: { Authorization: auth } } } }),
    },
    {
      id: 'claude-desktop',
      label: 'Claude Desktop',
      file: 'claude_desktop_config.json',
      where: [
        'Windows: %APPDATA%\\Claude\\claude_desktop_config.json',
        'macOS: ~/Library/Application Support/Claude/claude_desktop_config.json',
        'Needs Node.js (npx runs mcp-remote, which connects to Moorage). Restart Claude Desktop after saving.',
      ],
      json: pretty({
        mcpServers: {
          [SERVER]: {
            command: 'npx',
            // the header value comes from env: a space in an argument breaks on Windows
            args: ['-y', 'mcp-remote', endpoint, '--header', 'Authorization:${AUTH_HEADER}'],
            env: { AUTH_HEADER: auth },
          },
        },
      }),
    },
    {
      id: 'cursor',
      label: 'Cursor',
      file: 'mcp.json',
      where: ['Everywhere: ~/.cursor/mcp.json', 'One project: .cursor/mcp.json in the project folder'],
      json: pretty({ mcpServers: { [SERVER]: { url: endpoint, headers: { Authorization: auth } } } }),
    },
    {
      id: 'vscode',
      label: 'VS Code',
      file: 'mcp.json',
      where: ['One project: .vscode/mcp.json in the project folder', 'Everywhere: command palette → MCP: Open User Configuration'],
      json: pretty({ servers: { [SERVER]: { type: 'http', url: endpoint, headers: { Authorization: auth } } } }),
    },
  ];
}

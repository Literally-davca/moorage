// Thin client for the Moorage API. Every write sends the X-Moorage header (CSRF guard).
import { app } from './state.svelte';

export type Role = 'admin' | 'viewer';
export type AuthSource = 'local' | 'oidc' | 'proxy';
export type ExperienceMode = 'beginner' | 'advanced' | 'custom';
export type ComposeViewMode = 'form' | 'yaml-comments' | 'yaml-clean';
export type EnvViewMode = 'vars' | 'raw';

export interface UserPreferences {
  mode: ExperienceMode;
  composeView: ComposeViewMode;
  envView: EnvViewMode;
  linterShowTips: boolean;
  newStackDefault: 'templates' | 'blank';
  guidesMode: 'extended' | 'compact';
  confirmActions: 'all' | 'destructive';
  onboardingDone: boolean;
  manualOverrides?: Record<string, boolean>;
}

export interface CoolifyNameMeta {
  uuid: string;
  name: string;
  project?: string;
  environment?: string;
  type?: string;
  url?: string;
  fqdn?: string;
}

export interface APIToken {
  id: number;
  name: string;
  prefix: string;
  role: Role;
  scopes: string[];
  createdBy: string;
  createdAt: string;
  expiresAt?: string;
  lastUsedAt?: string;
}

export interface User {
  id: number;
  username: string;
  role: Role;
  source?: AuthSource;
  displayName?: string;
  preferences?: UserPreferences;
}
/** Which ways to sign in the login page offers. */
export interface LoginInfo {
  sso: boolean;
  ssoLabel?: string;
  localLogin: boolean;
  proxy: boolean;
}
export interface Session {
  setupRequired: boolean;
  user: User | null;
  version: string;
  bootId?: string;
  login?: LoginInfo;
  /** Why the trusted proxy's user was refused. */
  proxyError?: string;
}
export interface UserInfo {
  id: number;
  username: string;
  displayName: string;
  email: string;
  source: AuthSource;
  role: Role;
  status: 'active' | 'invited' | 'disabled';
  totp: boolean;
  lastLoginAt?: string;
  createdBy: string;
  createdAt: string;
  linkKind?: 'invite' | 'reset';
  linkExpires?: string;
}
export interface UserLink {
  token: string;
  kind: 'invite' | 'reset';
  expires: string;
}
export interface DeletedStack {
  name: string;
  deletedAt: string;
  hash: string;
  author: string;
}
export interface PrunePreview {
  imagesCount: number;
  imagesBytes: number;
  volumesCount: number;
  volumesBytes: number;
  containersCount: number;
  containersBytes: number;
  buildCacheBytes: number;
  totalBytes: number;
}
export interface PruneRequest {
  images: boolean;
  volumes: boolean;
  containers: boolean;
  buildCache: boolean;
}
export interface PruneResult {
  reclaimedBytes: number;
  deleted?: string[];
}
export interface AutoRollback {
  id: number;
  hostId: string;
  stack: string;
  updateId: number;
  checkName: string;
  reason: string;
  at: string;
  acknowledged: boolean;
}
export interface DiagnosticItem {
  level: 'warn' | 'info';
  id: string;
  title: string;
  detail: string;
}
export interface ImportPreview {
  token: string;
  exportedAt: string;
  version: string;
  database: { schemaVersion: number; users: number; agents: number; checks: number; channels: number; statusPages: number };
  hosts: {
    id: string;
    name: string;
    known: boolean;
    writable: boolean;
    history: boolean;
    stacks: { name: string; exists: boolean }[];
  }[];
  skipped?: string[];
}

async function rawError(res: Response): Promise<string> {
  const text = await res.text();
  try {
    return (JSON.parse(text) as { error?: string }).error || res.statusText;
  } catch {
    return text || res.statusText;
  }
}

export interface HostLockRow {
  id: string;
  name: string;
  kind: string;
  connected: boolean;
  readOnly: boolean;
  manageExternal: boolean;
  containerTerminal: boolean;
  hostTerminal: boolean;
  exec: boolean;
  selfUpdate: boolean;
}
export interface SettingsConfig {
  publicUrl: string;
  publicUrlRestart: boolean;
  statusPublicUrl?: string;
  agentPublicAddr?: string;
  metricsTokenSet: boolean;
  metricsUrl: string;
  updateChannel: 'stable' | 'pre';
  cleanupEnabled: boolean;
  cleanupSchedule: string;
  cleanupVolumes: boolean;
  fromEnv: {
    publicUrl?: boolean;
    statusPublicUrl?: boolean;
    agentPublicAddr?: boolean;
    metricsToken?: boolean;
    updateChannel?: boolean;
  };
  envLocks: {
    readOnly?: boolean;
    manageExternal?: boolean;
    terminal?: boolean;
    hostTerminal?: boolean;
    exec?: boolean;
    trustedProxies?: boolean;
    oidc?: boolean;
    selfUpdate?: boolean;
  };
  hostLocks?: HostLockRow[];
  diagnostics: DiagnosticItem[];
}
export interface SharedVar {
  id: number;
  key: string;
  value?: string;
  isSet: boolean;
  hostId: string;
  secret: boolean;
  fromEnv?: boolean;
}
export interface SisterLink {
  label: string;
  url: string;
}
export interface SisterLinks {
  grafanaUrl: string;
  beszelUrl: string;
  links: SisterLink[];
}
export interface CoolifyHostConn {
  hostId: string;
  url: string;
  token?: string;
  clearToken?: boolean;
  tokenSet?: boolean;
  allowPrivate: boolean;
}
export interface ConnectionsConfig {
  oidc: {
    issuer: string;
    clientId: string;
    clientSecretSet: boolean;
    scopes: string[];
    groupsClaim: string;
    adminGroups: string[];
    viewerGroups: string[];
    defaultRole: string;
    label: string;
    localLogin: boolean;
    fromEnv: boolean;
    ready: boolean;
    redirectUri?: string;
    lastError?: string;
  };
  coolify: {
    url: string;
    tokenSet: boolean;
    allowPrivate: boolean;
    fromEnv: boolean;
    hosts: CoolifyHostConn[];
  };
  registries: {
    server: string;
    username: string;
    passwordSet: boolean;
    fromEnv?: boolean;
  }[];
  panels: {
    grafanaUrl: string;
    grafanaTokenSet: boolean;
    grafanaFolderUid?: string;
    grafanaAllowPrivate: boolean;
    beszelUrl: string;
    links: SisterLink[];
    fromEnv: Record<string, boolean>;
  };
}
export interface RemoteBackupLastStatus {
  at: string;
  ok: boolean;
  target?: string;
  sizeBytes?: number;
  pruned?: number;
  error?: string;
}
export interface RemoteBackupConfig {
  enabled: boolean;
  provider: 'off' | 's3' | 'webdav';
  schedule: string;
  retention: number;
  backupPasswordSet: boolean;
  s3Endpoint: string;
  s3Bucket: string;
  s3Region: string;
  s3AccessKey: string;
  s3SecretKeySet: boolean;
  s3Prefix: string;
  s3AllowPrivate: boolean;
  webdavUrl: string;
  webdavUsername: string;
  webdavPasswordSet: boolean;
  webdavAllowPrivate: boolean;
  fromEnv: Record<string, boolean>;
  last?: RemoteBackupLastStatus;
}
export interface OIDCStatus {
  configured: boolean;
  ready: boolean;
  issuer?: string;
  clientId?: string;
  redirectUri?: string;
  scopes?: string[];
  groupsClaim?: string;
  adminGroups: string[];
  viewerGroups: string[];
  defaultRole?: string;
  localLogin: boolean;
  label?: string;
  lastError?: string;
  lastErrorAt?: string;
}
export interface ProxyStatus {
  enabled: boolean;
  trusted: string[];
  userHeader?: string;
  groupsHeader?: string;
  adminGroups: string[];
  viewerGroups: string[];
  defaultRole?: string;
}
export interface UsersPage {
  users: UserInfo[];
  self: number;
  oidc: OIDCStatus;
  proxy: ProxyStatus;
  publicUrl: string;
}
export interface LinkResult {
  user: UserInfo;
  link: UserLink;
  url?: string;
}
export interface SessionInfo {
  id: number;
  createdAt: string;
  lastSeen: string;
  expiresAt: string;
  ip: string;
  userAgent: string;
  current: boolean;
}
export interface Account {
  user: User;
  email?: string;
  totp: boolean;
  sessions: SessionInfo[];
}
export interface InviteInfo {
  username: string;
  displayName?: string;
  role: Role;
  kind: 'invite' | 'reset';
  expires: string;
}
export interface TOTPEnrolment {
  secret: string;
  otpauthUrl: string;
  ticket: string;
}
export type Status = 'up' | 'degraded' | 'down' | 'stopped' | 'inactive';
export interface ImageCandidate {
  source: 'template' | 'repo' | 'generated';
  title: string;
  detail?: string;
  url?: string;
  compose?: string;
  env?: string;
  warnings?: string[];
  templateSource?: string;
  templateId?: string;
}
export interface ImageLookup {
  image: string;
  name: string;
  candidates: ImageCandidate[];
  problems?: string[];
}
export interface StackSummary {
  name: string;
  managed: boolean;
  status: Status;
  running: number;
  total: number;
  busy: boolean;
  location?: string;
  /** Moorage itself runs from this stack: no stop/down/delete, changes go through Apply & restart. */
  self?: boolean;
  urls?: string[];
  /** The same port links on the host's other addresses (M17B, see hostnet.ts). */
  altUrls?: AltUrl[];
  displayName?: string;
  coolifyUuid?: string;
  coolifyType?: string;
  coolifyProject?: string;
  coolifyEnv?: string;
  coolifyUrl?: string;
  coolifyPr?: string;
  coolifyDeploying?: boolean;
}
export interface Container {
  id: string;
  name: string;
  service: string;
  image: string;
  state: string;
  status: string;
  exitCode?: number;
  health?: string;
  created: number;
  ports?: string[];
  urls?: string[];
  altUrls?: AltUrl[];
  hostNetwork?: boolean;
}
export interface Stack extends StackSummary {
  composeFile?: string;
  compose: string;
  env: string;
  containers: Container[];
  /** Outcome of the last Apply & restart of Moorage's own stack. */
  selfResult?: { at: string; ok: boolean; rolledBack: boolean; message: string };
}
export interface Stat {
  id: string;
  cpu: number;
  memUsed: number;
  /** 0 = unknown */
  memLimit: number;
  /** Why the engine gave no CPU value; show "–" with this reason, never the 0. */
  cpuNote?: string;
  /** Why the engine gave no memory value. */
  memNote?: string;
}
/** Engine fields shared by HostInfo and HostEntry. Older agents send only dockerVersion. */
export interface EngineFields {
  dockerVersion?: string;
  engine?: 'docker' | 'podman' | string;
  engineVersion?: string;
  rootless?: boolean;
}
export interface HostInfo extends EngineFields {
  name: string;
  dockerVersion: string;
  stacksDir: string;
  reachable: boolean;
  error?: string;
  locks?: HostLocks;
  hostIp?: string;
  baseDomain?: string;
  hostIpFromEnv?: boolean;
  baseDomainFromEnv?: boolean;
}
/** Whether the Prometheus endpoint is on; url only when MOORAGE_PUBLIC_URL is set. Never the token. */
export interface MetricsInfo {
  enabled: boolean;
  url?: string;
}
export interface HostEntry {
  id: string;
  name: string;
  kind: 'local' | 'agent';
  connected: boolean;
  lastSeen?: string;
  readOnly: boolean;
  terminal?: boolean;
  version?: string;
  dockerVersion?: string;
  engine?: string;
  engineVersion?: string;
  rootless?: boolean;
  hostname?: string;
  stacksDir?: string;
  watchdog?: boolean;
  watchdogAddr?: string;
  /** The agent runs checks for the hub (multi-location checks). Older agents don't say. */
  probes?: boolean;
  /** The host's security switches from its own env (older agents send none). */
  locks?: HostLocks;
  hostIp?: string;
  baseDomain?: string;
  hostIpFromEnv?: boolean;
  baseDomainFromEnv?: boolean;
  /** M17B: how Moorage sees this host's network and its named addresses. */
  netMode?: NetMode;
  addresses?: HostAddress[];
}
export interface WatchdogAgent {
  id: string;
  name: string;
  connected: boolean;
  participates: boolean;
  addr?: string;
  version?: string;
  supported: boolean;
}
export interface WatchdogSettings {
  enabled: boolean;
  channelId: number;
  grace: number;
  maintenanceUntil?: string;
  agents: WatchdogAgent[];
}
export type CheckKind = 'http' | 'tcp' | 'ping' | 'dns' | 'stack' | 'log';
export type CheckStatus = 'up' | 'down' | 'degraded' | 'pending' | 'paused' | 'unknown' | 'maintenance';
export interface LogLine {
  t: string;
  c: string;
  s: string;
  m: string;
}
export interface CheckSettings {
  method?: string;
  status?: string;
  keyword?: string;
  keywordAbsent?: boolean;
  jsonPath?: string;
  jsonValue?: string;
  ignoreTls?: boolean;
  recordType?: string;
  resolver?: string;
  expect?: string;
  allowLinkLocal?: boolean;
  pattern?: string;
  regex?: boolean;
  caseSensitive?: boolean;
  threshold?: number;
  window?: number;
  /** HTTP: 2–20 URLs of one check (one URL lives in target only). */
  urls?: string[];
  /** Stack: 2–20 stacks on any hosts (one stack lives in hostId/target only). */
  stacks?: StackTarget[];
}
export interface CheckInput {
  name: string;
  kind: CheckKind;
  target: string;
  hostId: string;
  stack: string;
  interval: number;
  timeout: number;
  retries: number;
  settings: CheckSettings;
  enabled: boolean;
  channels: number[];
  /** Where the check runs from: 'hub' and agent ids. Empty = the hub only. */
  locations: string[];
  /** majority | any | all, only with 2+ locations. */
  quorum: string;
}
export type Quorum = 'majority' | 'any' | 'all';
/** One location's answer in a multi-location run; vote 'none' = didn't answer. */
export interface LocationResult {
  location: string;
  name: string;
  vote: 'up' | 'down' | 'none';
  latencyMs: number;
  msg: string;
}
export interface CheckTestResult {
  ok: boolean;
  paused: boolean;
  unknown?: boolean;
  msg: string;
  latencyMs: number;
  matches?: LogLine[];
  locations?: LocationResult[];
}
export interface Tick {
  at: string;
  status: 'up' | 'down' | 'degraded' | 'silenced' | 'paused' | 'maintenance' | 'unknown';
  latencyMs: number;
  msg?: string;
}
/** One target's answer in the last run of a multi-URL or multi-stack check. */
export interface TargetResult {
  target: string;
  label: string;
  status: 'up' | 'down' | 'maintenance' | 'silenced' | 'paused' | 'unknown';
  latencyMs: number;
  msg: string;
}
export interface StackTarget {
  hostId: string;
  stack: string;
}
export type LintSeverity = 'error' | 'deploy_error' | 'warning' | 'info';
export interface LintFix {
  kind: string;
  label: string;
  service?: string;
  line?: number;
  keys?: string[];
  targetTag?: string;
}
export interface LintIssue {
  severity: LintSeverity;
  code: string;
  service?: string;
  line?: number;
  message: string;
  fix?: LintFix;
}
export interface LintResult {
  compose: string;
  env: string;
  moved?: string[];
  conflicts?: { key: string; envValue: string; composeValue: string; service: string }[];
  changed: boolean;
}
export type DeployStrategy = 'standard' | 'rolling';
export interface HostLocks {
  readOnly: boolean;
  manageExternal: boolean;
  extraStacksDirs?: string[];
  terminal: boolean;
  hostTerminal: boolean;
  exec: boolean;
  selfUpdate: boolean;
}
/** One container of a host, compose or standalone (GET /api/hosts/{host}/containers). */
export interface ContainerInfo extends Container {
  project?: string;
  standalone?: boolean;
  managed?: boolean;
  workingDir?: string;
  labels?: Record<string, string>;
}
/** Feeds the badges of the top navigation (GET /api/nav-summary). */
export interface NavSummary {
  checksFailing: number;
  maintenanceActive: number;
  updatesFailed: number;
  updatesAvailable: number;
  failing: { id: number; name: string; status: CheckStatus; msg?: string }[];
  mcpDrafts?: number;
}
export interface Anchor {
  at: string;
  kind: string;
  actor: string;
  detail?: string;
}
export interface Incident {
  id: number;
  startedAt: string;
  endedAt?: string;
  cause: string;
  context?: {
    host?: string;
    stack?: string;
    stackDisplayName?: string;
    containers?: Container[];
    matches?: LogLine[];
    logs?: LogLine[];
    note?: string;
    locations?: LocationResult[];
    targets?: TargetResult[];
  };
}
export interface Check extends CheckInput {
  id: number;
  state: {
    status: CheckStatus;
    since?: string;
    last?: { at: string; status: string; latencyMs: number; msg: string };
    locations?: LocationResult[];
    targets?: TargetResult[];
  };
  uptime24h: number | null;
  uptime7d?: number | null;
  uptime30d?: number | null;
  ticks: Tick[];
  hostName?: string;
  targetDisplayName?: string;
  incidents?: Incident[];
  anchors?: Anchor[];
  /** The planned maintenance window running over the check now. */
  maintenance?: ActiveMaintenance;
  /** Newest results per location of a multi-location check (detail view only). */
  locationTicks?: Record<string, Tick[]>;
}
export interface ActiveMaintenance {
  id: number;
  title: string;
  until: string;
}
export interface MaintenanceStack {
  hostId: string;
  stack: string;
}
export interface MaintenanceScope {
  all?: boolean;
  stacks?: MaintenanceStack[];
  checks?: number[];
}
export type MaintenanceKind = 'once' | 'weekly';
export interface MaintenanceInput {
  title: string;
  message: string;
  scope: MaintenanceScope;
  kind: MaintenanceKind;
  startsAt?: string;
  endsAt?: string;
  /** 0 = Sunday … 6 = Saturday */
  weekdays: number[];
  startTime?: string;
  /** minutes */
  duration?: number;
  timeZone?: string;
  enabled: boolean;
}
export interface Occurrence {
  start: string;
  end: string;
}
export type MaintenanceState = 'active' | 'upcoming' | 'past' | 'disabled';
export interface MaintenanceWindow extends MaintenanceInput {
  id: number;
  createdBy: string;
  createdAt: string;
  state: MaintenanceState;
  current?: Occurrence;
  next?: Occurrence;
  last?: Occurrence;
  scopeText: string[];
}
export interface StackMaintenance {
  active?: ActiveMaintenance;
  events: { at: string; detail: string }[];
}
export type ChannelKind = 'webhook' | 'ntfy' | 'telegram' | 'discord' | 'smtp' | 'slack' | 'gotify' | 'pushover' | 'matrix';
export interface ChannelConfig {
  url?: string;
  token?: string;
  chatId?: string;
  host?: string;
  port?: number;
  security?: string;
  username?: string;
  password?: string;
  from?: string;
  to?: string;
  headers?: Record<string, string>;
  events?: string[];
  /** M17B: content preset, Custom parts and rich / plain format (missing = Standard + rich). */
  preset?: NotifyPreset;
  include?: Partial<Record<NotifyPart, boolean>>;
  format?: 'rich' | 'plain';
}
export interface Channel {
  id: number;
  name: string;
  kind: ChannelKind;
  config: ChannelConfig;
  enabled: boolean;
}
export interface LogLine {
  t: string;
  c: string;
  p?: string;
  svc?: string;
  s: string;
  m: string;
}

export interface Op {
  id: string;
  stack: string;
  action: string;
  done: boolean;
  error?: string;
}
export type VersionKind = 'create' | 'edit' | 'restore' | 'external' | 'import' | 'delete';
export interface Version {
  hash: string;
  short: string;
  message: string;
  author: string;
  time: string;
  kind: VersionKind;
  files: string[];
  restored?: string;
  current: boolean;
}
export interface DiffLine {
  op: ' ' | '+' | '-';
  text: string;
  old?: number;
  new?: number;
}
export interface FileDiff {
  name: string;
  status: 'added' | 'modified' | 'deleted';
  hunks: { oldStart: number; newStart: number; lines: DiffLine[] }[] | null;
  binary?: boolean;
}
export interface VersionDetail extends Version {
  diff: FileDiff[];
}
export interface ConvertResult {
  yaml: string;
  service: string;
  warnings: string[];
}
export interface StatusPageItem {
  section: string;
  displayName: string;
  checkId?: number;
  hostId?: string;
  stack?: string;
}
export interface StatusPageInput {
  slug: string;
  title: string;
  description: string;
  domain: string;
  published: boolean;
  showIncidents: boolean;
  items: StatusPageItem[];
}
export interface StatusPage extends StatusPageInput {
  id: number;
  status: 'operational' | 'down' | 'maintenance' | 'paused' | 'nodata';
  statusText: string;
  publicUrl: string;
  statusListen?: string;
}
export interface PublicStatusDay {
  date: string;
  status: 'operational' | 'down' | 'maintenance' | 'paused' | 'nodata';
  uptime?: number;
  tooltip: string;
}
export interface PublicStatusItem {
  displayName: string;
  status: 'operational' | 'down' | 'maintenance' | 'paused' | 'nodata';
  statusLabel: string;
  uptime24h: number | null;
  uptime30d: number | null;
  uptime24hText: string;
  uptime30dText: string;
  days: PublicStatusDay[];
}
export interface PublicStatusSection {
  name?: string;
  items: PublicStatusItem[];
}
export interface PublicStatusIncident {
  displayName: string;
  startedAt: string;
  startedText: string;
  endedAt?: string;
  endedText?: string;
  ongoing: boolean;
  duration: string;
}
export interface PublicStatusPage {
  slug: string;
  title: string;
  description?: string;
  bannerStatus: 'operational' | 'down' | 'maintenance' | 'paused' | 'nodata';
  bannerText: string;
  updatedAt: string;
  updatedText: string;
  showIncidents: boolean;
  sections: PublicStatusSection[];
  incidents?: PublicStatusIncident[];
}
export type ImagePolicy = 'off' | 'notify' | 'auto';
export type ImageStatus =
  | 'up-to-date'
  | 'update-available'
  | 'pulled-not-applied'
  | 'pinned'
  | 'built'
  | 'local-only'
  | 'unreachable'
  | 'unsupported';
export interface UpdateSettings {
  enabled: boolean;
  interval: number;
  windowStart: string;
  windowEnd: string;
  channels: number[];
  timezone: string;
}
export interface ImageCheckRow {
  hostId: string;
  stack: string;
  service: string;
  ref: string;
  localId?: string;
  localDigest?: string;
  runningId?: string;
  remoteDigest?: string;
  status: ImageStatus;
  error?: string;
  newerTag?: string;
  newerMajor?: string;
  checkedAt?: string;
}
export interface ImageUpdateService {
  service: string;
  ref: string;
  oldId?: string;
  oldDigest?: string;
  newId?: string;
  newDigest?: string;
}
export interface ImageUpdate {
  id: number;
  hostId: string;
  stack: string;
  startedAt: string;
  finishedAt?: string;
  actor: string;
  result: 'ok' | 'pull-failed' | 'up-failed' | 'rolled-back';
  services: ImageUpdateService[];
  logTail?: string;
}
export interface ReleaseSummary {
  version: string;
  tag: string;
  prerelease: boolean;
  publishedAt?: string;
  htmlUrl?: string;
  notes?: string;
  notesTruncated?: boolean;
  image?: string;
  indexDigest?: string;
  minHub?: string;
  minAgent?: string;
}
export interface UpdateJob {
  id: number;
  nodeId: string;
  action: 'apply' | 'rollback';
  fromVersion: string;
  toVersion: string;
  status: 'running' | 'restarting' | 'done' | 'failed' | 'rolled-back';
  message?: string;
  startedAt?: string;
  finishedAt?: string;
}
export interface NodeUpdateStatus {
  id: string;
  name: string;
  kind: 'hub' | 'all-in-one' | 'agent';
  connected: boolean;
  readOnly?: boolean;
  version: string;
  mode: 'container' | 'binary' | 'unknown';
  reason?: string;
  supported: boolean;
  selfUpdate: boolean;
  olderAgent?: boolean;
  manualCommand?: string;
  platform?: string;
  composeProject?: string;
  containerName?: string;
  image?: string;
  latest?: string;
  updateAvailable: boolean;
  canUpdate: boolean;
  compatibility?: 'ok' | 'update-hub-first' | 'agent-too-old' | 'dev-build';
  compatNote?: string;
  prevVersion?: string;
  prevSavedAt?: string;
  lastRollback?: string;
  job?: UpdateJob;
}
export interface SystemUpdatesResponse {
  channel: string;
  updateUrl: string;
  lastChecked?: string;
  checkError?: string;
  release?: ReleaseSummary;
  keyReady: boolean;
  nodes: NodeUpdateStatus[];
}
export interface ApplyUpdatesResult {
  jobs: UpdateJob[];
  restartingHub: boolean;
}
export type TimelineKind = 'version' | 'event' | 'incident' | 'update' | 'audit' | 'terminal';
export interface TimelineEntry {
  key: string;
  at: string;
  kind: TimelineKind;
  title: string;
  actor?: string;
  ip?: string;
  detail?: string;
  outcome?: string;
  ref: {
    hash?: string;
    checkId?: number;
    incidentId?: number;
    updateId?: number;
    eventId?: number;
    auditId?: number;
    sessionId?: number;
  };
  merged?: ('event' | 'audit')[];
  eventKind?: string;
  action?: string;
  version?: { short: string; kind: VersionKind; current: boolean; restored?: string; files?: string[] };
  update?: { result: ImageUpdate['result']; canRollback: boolean };
  incident?: {
    phase: 'open' | 'close';
    checkName: string;
    checkKind: string;
    startedAt: string;
    endedAt?: string;
    cause: string;
    /** upstream stack the alert was folded into (cascade) */
    causedBy?: StackRef;
  };
}
export type AutoRestartMode = 'off' | 'on_recover' | 'on_recover_and_deploy';
export interface StackRef {
  hostId: string;
  stack: string;
}
export interface StackDep {
  id: number;
  from: StackRef;
  to: StackRef;
  source: 'detected' | 'manual';
  reason?: 'network' | 'hostname';
  detail?: string;
  ignored: boolean;
  createdBy?: string;
  createdAt: string;
  autoRestart?: AutoRestartMode;
}
export interface DepNode extends StackRef {
  hostName?: string;
  displayName?: string;
  coolifyProject?: string;
  coolifyUuid?: string;
  status?: Status;
  running?: number;
  total?: number;
  checks?: CheckStatus | 'silenced';
  inCycle?: boolean;
  external?: boolean;
}
export interface DepDetection {
  at?: string;
  running?: boolean;
  unsupported?: boolean;
  error?: string;
  stacks: number;
  broken?: string[];
}
export interface DepHost {
  id: string;
  name: string;
  kind: 'local' | 'agent';
  connected: boolean;
  readOnly?: boolean;
  lastSeen?: string;
}
export interface HostDeps {
  edges: StackDep[];
  nodes: DepNode[];
  hosts?: DepHost[];
  cycles: StackRef[][];
  detection: DepDetection;
  stacksError?: string;
}
export interface TimelinePage {
  entries: TimelineEntry[];
  more: boolean;
  next?: string;
  versionsError?: string;
}
export interface TimelineQuery {
  before?: string;
  until?: number;
  since?: number;
  limit?: number;
  kinds?: TimelineKind[];
}
export interface StackImages {
  hostId: string;
  hostName: string;
  readOnly: boolean;
  stack: string;
  displayName?: string;
  coolifyProject?: string;
  coolifyUuid?: string;
  managed: boolean;
  status: Status;
  running: number;
  policy: ImagePolicy;
  autoReason?: string;
  checkedAt?: string;
  services: ImageCheckRow[];
  lastUpdate?: ImageUpdate;
  updates?: ImageUpdate[];
}
export interface UpdatesOverview {
  settings: UpdateSettings;
  stacks: StackImages[];
  updates: ImageUpdate[];
}
export type ShellName = 'sh' | 'bash' | 'ash';
export interface TerminalAuditRow {
  id: number;
  user: string;
  ip: string;
  hostId: string;
  hostName?: string;
  stack: string;
  containerName: string;
  service?: string;
  shell: ShellName;
  startedAt: string;
  endedAt?: string;
  reason?: string;
  exitCode?: number;
}
export type AuditOutcome = 'ok' | 'failed';
export interface AuditEntry {
  id: number;
  at: string;
  actor: string;
  ip: string;
  action: string;
  hostId: string;
  hostName?: string;
  stack: string;
  stackDisplayName?: string;
  target: string;
  outcome: AuditOutcome;
}
export interface AuditFilter {
  host?: string;
  stack?: string;
  actor?: string;
  /** action prefix, e.g. "stack." */
  action?: string;
  /** paging: entries older than this id */
  before?: number;
  limit?: number;
}

/** A compose template in the catalogue. error = listed but unusable (shown, not hidden). */
export interface TemplateEntry {
  source: string;
  sourceLabel: string;
  id: string;
  name: string;
  version?: string;
  description: string;
  category: string;
  website?: string;
  error?: string;
}
export interface TemplateSourceInfo {
  key: string;
  kind: 'bundled' | 'local' | 'remote';
  label: string;
  name?: string;
  dir?: string;
  count: number;
  error?: string;
  lastOk?: string;
  refreshing?: boolean;
}
export interface TemplateCatalog {
  templates: TemplateEntry[];
  sources: TemplateSourceInfo[];
  localDir?: string;
}
export interface TemplateVariable {
  name: string;
  label: string;
  description?: string;
  default?: string;
  secret: boolean;
  generate?: 'password' | 'hex32' | 'uuid';
  required: boolean;
  pattern?: string;
}
export interface TemplateDetail {
  template: TemplateEntry;
  notes?: string;
  variables: TemplateVariable[];
  compose: string;
  env: string;
  warnings: string[];
  /** Fresh values for generated variables (admins only). */
  generated?: Record<string, string>;
}
export interface RenderedTemplate {
  compose: string;
  env: string;
  values: Record<string, string>;
  secrets: string[];
  notes?: string;
  warnings: string[];
}
export interface TemplateRef {
  source: string;
  id: string;
}
export interface AddonInfo {
  id: string;
  name: string;
  description: string;
  category: string;
  kind: 'builtin' | 'sidecar';
  version?: string;
  ram?: string;
  cpu?: string;
  hostId?: string;
  hostName?: string;
  stackName?: string;
  enabled: boolean;
  installed: boolean;
  toggleable: boolean;
  removable: boolean;
  custom?: boolean;
  source?: string;
  detail?: string;
  linkHref?: string;
  linkLabel?: string;
  docsSlug?: string;
  docsUrl?: string;
}
export interface AddonCatalogEntry {
  id: string;
  name: string;
  version: string;
  kind: 'builtin' | 'sidecar';
  category: string;
  description: string;
  resources: { ram: string; cpu: string };
  readme?: string;
  compose?: string;
  sourceId?: number;
  sourceName?: string;
  sourceUrl?: string;
}
export interface AddonSource {
  id: number;
  name: string;
  url: string;
  allowPrivate: boolean;
  enabled: boolean;
  fetchedAt?: string;
  error?: string;
  createdAt: string;
}
export interface CustomAddonDetail {
  id: string;
  hostId: string;
  name: string;
  category: string;
  description: string;
  compose: string;
  env: string;
  custom: boolean;
}
export interface ScheduledTask {
  id?: number;
  name: string;
  hostId: string;
  stack: string;
  stackDisplayName?: string;
  cron: string;
  action: 'restart' | 'start' | 'stop' | 'pull-up' | 'exec';
  service?: string;
  command?: string;
  enabled: boolean;
  channels: number[];
  lastRunAt?: string;
  lastStatus?: string;
  lastOutput?: string;
}
export interface SocketFinding {
  hostId: string;
  hostName: string;
  stack: string;
  service: string;
  severity: 'critical' | 'warning';
  issue: string;
  detail: string;
}
export interface RegistryWebhook {
  id: number;
  name: string;
  provider: string;
  secretSet: boolean;
  applyNow: boolean;
  enabled: boolean;
  createdAt: string;
  lastHitAt?: string;
  lastResult?: string;
}
export interface MCPACLRule {
  hostId: string;
  stackPattern: string;
  scope: 'read' | 'operate' | 'deploy';
}
export interface MCPToken {
  id: number;
  name: string;
  acl: MCPACLRule[];
  requireApproval: boolean;
  expiresAt?: string;
  createdAt: string;
  createdBy: string;
  lastUsedAt?: string;
}
export interface MCPDraft {
  id: number;
  tokenName: string;
  tool: string;
  hostId: string;
  stack: string;
  stackDisplayName?: string;
  compose: string;
  envKeys: string[];
  note: string;
  deploy: boolean;
  createNew: boolean;
  status: string;
  createdAt: string;
}
export interface RemoteTemplateSource {
  id: number;
  key: string;
  url: string;
  host: string;
  allowLocal: boolean;
  name?: string;
  lastAttempt?: string;
  lastOk?: string;
  lastError?: string;
  count: number;
  refreshing: boolean;
}

export class ApiError extends Error {
  constructor(
    public status: number,
    message: string,
  ) {
    super(message);
  }
}

let onUnauthorized: () => void = () => {};
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn;
}
/** signInAgain shows the sign-in page, e.g. after a live connection closed with 4401. */
export function signInAgain() {
  onUnauthorized();
}

/** A pairing token that is still open: not used yet, or used by an agent that hasn't connected. */
export type JoinToken = {
  id: number;
  name: string;
  createdAt: string;
  expiresAt: string;
  agentId?: string;
  redeemedAt?: string;
};

/** Close code of a WebSocket whose sign-in was revoked (sign-out, password or role change). */
export const CLOSE_SESSION_ENDED = 4401;

export type TerminalTicketReq = {
  kind: 'stack' | 'container' | 'host';
  host: string;
  stack?: string;
  container?: string;
};

export async function request<T>(method: string, path: string, body?: unknown): Promise<T> {
  const headers: Record<string, string> = {};
  if (method !== 'GET') headers['X-Moorage'] = '1';
  if (body !== undefined) headers['Content-Type'] = 'application/json';
  let res: Response;
  try {
    res = await fetch(path, {
      method,
      headers,
      body: body === undefined ? undefined : JSON.stringify(body),
      credentials: 'same-origin',
    });
  } catch {
    throw new ApiError(0, "Can't reach Moorage. Check that it's running.");
  }
  if (res.status === 204) return undefined as T;
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    if (res.status === 401 && !path.startsWith('/api/login') && !path.startsWith('/api/setup') && !path.startsWith('/api/invite'))
      onUnauthorized();
    throw new ApiError(res.status, data.error ?? `Request failed (${res.status}).`);
  }
  return data as T;
}

const enc = encodeURIComponent;
/** H is the API prefix of the host shown in the UI. */
const H = () => `/api/hosts/${enc(app.host)}`;
/** hostPath turns a host-relative API path into a full one (for WebSockets). */
export const hostPath = (p: string) => H() + p;

import type { AltUrl, HostAddress, NetMode } from './hostnet';
import type { NotifyPart, NotifyPreset } from './notifications';
export type { AltUrl, HostAddress, NetMode, NotifyPart, NotifyPreset };

export const api = {
  session: () => request<Session>('GET', '/api/session'),
  setupStart: (token: string, username: string, password: string) =>
    request<{ secret: string; otpauthUrl: string }>('POST', '/api/setup/start', { token, username, password }),
  setupFinish: (token: string, username: string, code: string) =>
    request<User>('POST', '/api/setup/finish', { token, username, code }),
  login: (username: string, password: string, code: string) =>
    request<User>('POST', '/api/login', { username, password, code }),
  logout: () => request<{ redirect?: string } | undefined>('POST', '/api/logout'),
  inviteInspect: (token: string) => request<InviteInfo>('POST', '/api/invite/inspect', { token }),
  inviteStart: (token: string, password: string) =>
    request<{ secret: string; otpauthUrl: string }>('POST', '/api/invite/start', { token, password }),
  inviteFinish: (token: string, code: string) => request<User>('POST', '/api/invite/finish', { token, code }),
  users: () => request<UsersPage>('GET', '/api/users'),
  createUser: (username: string, role: Role, displayName: string) =>
    request<LinkResult>('POST', '/api/users', { username, role, displayName }),
  setUserRole: (id: number, role: Role) => request<void>('PUT', `/api/users/${id}/role`, { role }),
  disableUser: (id: number) => request<void>('POST', `/api/users/${id}/disable`),
  enableUser: (id: number) => request<void>('POST', `/api/users/${id}/enable`),
  deleteUser: (id: number) => request<void>('DELETE', `/api/users/${id}`),
  resetUser: (id: number) => request<LinkResult>('POST', `/api/users/${id}/reset`),
  revokeUserSessions: (id: number) => request<void>('DELETE', `/api/users/${id}/sessions`),
  account: () => request<Account>('GET', '/api/account'),
  changePassword: (current: string, password: string) => request<void>('PUT', '/api/account/password', { current, password }),
  totpStart: (password: string, code: string) => request<TOTPEnrolment>('POST', '/api/account/totp/start', { password, code }),
  totpFinish: (secret: string, ticket: string, code: string) =>
    request<void>('PUT', '/api/account/totp', { secret, ticket, code }),
  revokeSession: (id: number) => request<void>('DELETE', `/api/account/sessions/${id}`),
  revokeOtherSessions: () => request<{ revoked: number }>('DELETE', '/api/account/sessions'),
  hosts: () =>
    request<{ hosts: HostEntry[]; agentPort?: string; agentPublicAddr?: string; acceptsAgents?: boolean; metrics?: MetricsInfo }>('GET', '/api/hosts'),
  addAgent: (name: string) => request<{ token: string; expires: string; agentPort: string }>('POST', '/api/agents', { name }),
  joinTokens: () => request<{ tokens: JoinToken[]; agentPort: string; agentPortOff: boolean }>('GET', '/api/agents/join-tokens'),
  cancelJoinToken: (id: number) => request<void>('DELETE', `/api/agents/join-tokens/${id}`),
  renameHost: (id: string, name: string) => request<void>('PATCH', `/api/hosts/${enc(id)}`, { name }),
  updateHost: (id: string, patch: { name?: string; hostIp?: string; baseDomain?: string }) =>
    request<void>('PATCH', `/api/hosts/${enc(id)}`, patch),
  removeHost: (id: string) => request<void>('DELETE', `/api/hosts/${enc(id)}`),
  host: () => request<HostInfo>('GET', `${H()}/info`),
  stacks: (host?: string) => request<{ stacks: StackSummary[]; error?: string }>('GET', `/api/hosts/${enc(host ?? app.host)}/stacks`),
  stack: (name: string) => request<Stack>('GET', `${H()}/stacks/${enc(name)}`),
  stats: (name: string) => request<Stat[]>('GET', `${H()}/stacks/${enc(name)}/stats`),
  validate: (name: string, compose: string, env: string, deploy = false) =>
    request<{ ok: boolean; error?: string; issues?: LintIssue[] }>('POST', `${H()}/stacks/${enc(name)}/validate`, { compose, env, deploy }),
  lintFix: (name: string, compose: string, env: string, fix: LintFix) =>
    request<LintResult>('POST', `${H()}/stacks/${enc(name || 'new')}/lint/fix`, { compose, env, fix }),
  extractEnv: (name: string, compose: string, env: string, overwrite = false) =>
    request<LintResult>('POST', `${H()}/stacks/${enc(name || 'new')}/env/extract`, { compose, env, overwrite }),
  stackStrategy: (name: string) => request<{ strategy: DeployStrategy }>('GET', `${H()}/stacks/${enc(name)}/strategy`),
  setStackStrategy: (name: string, strategy: DeployStrategy) =>
    request<void>('PUT', `${H()}/stacks/${enc(name)}/strategy`, { strategy }),
  containers: (host?: string) => request<ContainerInfo[]>('GET', `/api/hosts/${enc(host ?? app.host)}/containers`),
  containerAction: (id: string, action: string, host?: string) =>
    request<Op>('POST', `/api/hosts/${enc(host ?? app.host)}/containers/${enc(id)}/actions/${enc(action)}`),
  externalAction: (name: string, action: string) => request<Op>('POST', `${H()}/external/${enc(name)}/actions/${enc(action)}`),
  create: (name: string, compose: string, env: string, template?: TemplateRef) =>
    request<void>('POST', `${H()}/stacks`, { name, compose, env, template }),
  templates: () => request<TemplateCatalog>('GET', '/api/templates'),
  template: (source: string, id: string, host?: string) =>
    request<TemplateDetail>('GET', `/api/templates/${enc(source)}/${enc(id)}?host=${enc(host ?? app.host)}`),
  renderTemplate: (source: string, id: string, values: Record<string, string>, hostId?: string) =>
    request<{ template: TemplateEntry; result: RenderedTemplate }>('POST', '/api/templates/render', {
      source,
      id,
      hostId: hostId ?? app.host,
      values,
    }),
  templateSources: () => request<{ sources: RemoteTemplateSource[]; dir: string }>('GET', '/api/templates/sources'),
  addTemplateSource: (url: string, allowLocal: boolean) =>
    request<{ id: number }>('POST', '/api/templates/sources', { url, allowLocal }),
  saveTemplateSource: (id: number, url: string, allowLocal: boolean) =>
    request<void>('PUT', `/api/templates/sources/${id}`, { url, allowLocal }),
  deleteTemplateSource: (id: number) => request<void>('DELETE', `/api/templates/sources/${id}`),
  refreshTemplateSource: (id: number) => request<void>('POST', `/api/templates/sources/${id}/refresh`),
  save: (name: string, compose: string, env: string) => request<void>('PUT', `${H()}/stacks/${enc(name)}`, { compose, env }),
  selfApply: (name: string, compose: string, env: string) =>
    request<void>('POST', `${H()}/stacks/${enc(name)}/self-apply`, { compose, env }),
  remove: (name: string) => request<void>('DELETE', `${H()}/stacks/${enc(name)}`),
  action: (name: string, action: string, strategy?: DeployStrategy) =>
    request<Op>('POST', `${H()}/stacks/${enc(name)}/actions/${enc(action)}${strategy ? `?strategy=${strategy}` : ''}`),
  timeline: (name: string, q: TimelineQuery = {}) => {
    const p = new URLSearchParams();
    if (q.before) p.set('before', q.before);
    if (q.until) p.set('until', String(q.until));
    if (q.since) p.set('since', String(q.since));
    if (q.limit) p.set('limit', String(q.limit));
    if (q.kinds?.length) p.set('kinds', q.kinds.join(','));
    const qs = p.toString();
    return request<TimelinePage>('GET', `${H()}/stacks/${enc(name)}/timeline${qs ? '?' + qs : ''}`);
  },
  history: (name: string) => request<Version[]>('GET', `${H()}/stacks/${enc(name)}/history`),
  version: (name: string, hash: string) =>
    request<VersionDetail>('GET', `${H()}/stacks/${enc(name)}/history/${enc(hash)}`),
  restore: (name: string, hash: string, deploy: boolean) =>
    request<{ version: Version | null; op?: Op; deployError?: string }>('POST', `${H()}/stacks/${enc(name)}/restore`, {
      hash,
      deploy,
    }),
  stacksOf: (host: string) => request<{ stacks: StackSummary[]; error?: string }>('GET', `/api/hosts/${enc(host)}/stacks`),
  health: (name: string) => request<{ checks: Check[]; anchors: Anchor[] }>('GET', `${H()}/stacks/${enc(name)}/health`),
  checks: () => request<Check[]>('GET', '/api/checks'),
  navSummary: () => request<NavSummary>('GET', '/api/nav-summary'),
  check: (id: number) => request<Check>('GET', `/api/checks/${id}`),
  saveCheck: (id: number | null, c: CheckInput) =>
    request<{ id: number }>(id ? 'PUT' : 'POST', id ? `/api/checks/${id}` : '/api/checks', c),
  deleteCheck: (id: number) => request<void>('DELETE', `/api/checks/${id}`),
  testCheck: (c: CheckInput) => request<CheckTestResult>('POST', '/api/checks/test', c),
  channels: () => request<Channel[]>('GET', '/api/channels'),
  saveChannel: (c: Omit<Channel, 'id'> & { id?: number }) =>
    request<{ id: number }>(c.id ? 'PUT' : 'POST', c.id ? `/api/channels/${c.id}` : '/api/channels', c),
  deleteChannel: (id: number) => request<void>('DELETE', `/api/channels/${id}`),
  testChannel: (c: Omit<Channel, 'id'> & { id?: number }) => request<void>('POST', '/api/channels/test', c),
  watchdog: () => request<WatchdogSettings>('GET', '/api/watchdog'),
  saveWatchdog: (w: { enabled: boolean; channelId: number; grace: number; agents: Record<string, boolean> }) =>
    request<void>('PUT', '/api/watchdog', w),
  watchdogMaintenance: (minutes: number) =>
    request<{ maintenanceUntil?: string }>('POST', '/api/watchdog/maintenance', { minutes }),
  testWatchdog: (channelId?: number) =>
    request<{ agent: string }>('POST', '/api/watchdog/test', channelId ? { channelId } : {}),
  maintenance: () => request<{ windows: MaintenanceWindow[]; hubTimeZone: string }>('GET', '/api/maintenance'),
  maintenanceWindow: (id: number) => request<MaintenanceWindow>('GET', `/api/maintenance/${id}`),
  saveMaintenance: (id: number | null, w: MaintenanceInput) =>
    request<{ id: number }>(id ? 'PUT' : 'POST', id ? `/api/maintenance/${id}` : '/api/maintenance', w),
  deleteMaintenance: (id: number) => request<void>('DELETE', `/api/maintenance/${id}`),
  stackMaintenance: (name: string) => request<StackMaintenance>('GET', `${H()}/stacks/${enc(name)}/maintenance`),
  statusPages: () => request<StatusPage[]>('GET', '/api/status-pages'),
  statusPage: (id: number) => request<StatusPage>('GET', `/api/status-pages/${id}`),
  previewStatusPage: (id: number) => request<PublicStatusPage>('GET', `/api/status-pages/${id}/preview`),
  previewStatusDraft: (id: number | null, p: StatusPageInput) =>
    request<PublicStatusPage>('POST', '/api/status-pages/preview', { ...p, id: id ?? 0 }),
  saveStatusPage: (id: number | null, p: StatusPageInput) =>
    request<{ id: number }>(id ? 'PUT' : 'POST', id ? `/api/status-pages/${id}` : '/api/status-pages', p),
  deleteStatusPage: (id: number) => request<void>('DELETE', `/api/status-pages/${id}`),
  updates: () => request<UpdatesOverview>('GET', '/api/updates'),
  saveUpdateSettings: (s: Omit<UpdateSettings, 'timezone'>) => request<UpdateSettings>('PUT', '/api/updates/settings', s),
  checkAllUpdates: (hostId?: string, stack?: string) =>
    request<{ services: ImageCheckRow[] }>('POST', '/api/updates/check', hostId && stack ? { hostId, stack } : {}),
  stackImages: (name: string) => request<StackImages>('GET', `${H()}/stacks/${enc(name)}/images`),
  saveStackImagePolicy: (name: string, policy: ImagePolicy, hostId?: string) =>
    request<{ policy: ImagePolicy }>(
      'PUT',
      `${hostId ? `/api/hosts/${enc(hostId)}` : H()}/stacks/${enc(name)}/images/policy`,
      { policy },
    ),
  checkStackImages: (name: string) =>
    request<{ services: ImageCheckRow[] }>('POST', `${H()}/stacks/${enc(name)}/images/check`),
  updateStackImages: (name: string) => request<Op>('POST', `${H()}/stacks/${enc(name)}/images/update`),
  changeStackImageTag: (name: string, service: string, oldRef: string, newTag: string) =>
    request<{ version: Version | null; op?: Op; newRef: string; updateError?: string }>(
      'POST',
      `${H()}/stacks/${enc(name)}/images/change-tag`,
      { service, oldRef, newTag },
    ),
  rollbackStackImages: (name: string, updateId?: number) =>
    request<Op>('POST', `${H()}/stacks/${enc(name)}/images/rollback`, updateId ? { updateId } : {}),
  systemUpdates: () => request<SystemUpdatesResponse>('GET', '/api/system/updates'),
  checkSystemUpdates: () => request<SystemUpdatesResponse>('POST', '/api/system/updates/check', {}),
  applySystemUpdates: (targets: string[]) =>
    request<ApplyUpdatesResult>('POST', '/api/system/updates/apply', { targets }),
  rollbackSystemUpdate: (node: string) =>
    request<{ job: UpdateJob; restartingHub: boolean }>('POST', `/api/system/updates/${enc(node)}/rollback`, {}),
  terminalSessions: (host?: string, stack?: string) => {
    const q = new URLSearchParams();
    if (host) q.set('host', host);
    if (stack) q.set('stack', stack);
    const qs = q.toString();
    return request<TerminalAuditRow[]>('GET', `/api/terminal/sessions${qs ? '?' + qs : ''}`);
  },
  stackTerminalSessions: (name: string) =>
    request<TerminalAuditRow[]>('GET', `${H()}/stacks/${enc(name)}/terminal/sessions`),
  audit: (f: AuditFilter = {}) => {
    const q = new URLSearchParams();
    for (const [k, v] of Object.entries(f)) if (v !== undefined && v !== '') q.set(k, String(v));
    const qs = q.toString();
    return request<{ entries: AuditEntry[]; more: boolean }>('GET', `/api/audit${qs ? '?' + qs : ''}`);
  },
  convert: (command: string) => request<ConvertResult>('POST', '/api/convert', { command }),
  imageLookup: (image: string) => request<ImageLookup>('POST', '/api/image-lookup', { image }),
  allDeps: () => request<HostDeps>('GET', '/api/deps'),
  addons: (opts?: { catalog?: boolean; refresh?: boolean }) => {
    const q = new URLSearchParams();
    if (opts?.catalog) q.set('catalog', '1');
    if (opts?.refresh) q.set('refresh', '1');
    const qs = q.toString();
    return request<{ addons: AddonInfo[]; catalog?: AddonCatalogEntry[]; catalogError?: string }>('GET', `/api/addons${qs ? '?' + qs : ''}`);
  },
  installAddon: (id: string, hostIds?: string | string[], source?: string) => {
    const list = Array.isArray(hostIds) ? hostIds : hostIds ? [hostIds] : [];
    return request<{ envAdded?: string[]; generatedSecrets?: string[] } | undefined>('POST', '/api/addons/install', {
      id,
      hostId: list[0] ?? '',
      hostIds: list,
      source: source ?? '',
    });
  },
  setAddon: (id: string, enabled: boolean, hostId?: string) =>
    request<void>('PUT', `/api/addons/${enc(id)}${hostId ? `?host=${enc(hostId)}` : ''}`, { enabled, hostId }),
  uninstallAddon: (id: string, hostId?: string) =>
    request<void>('DELETE', `/api/addons/${enc(id)}${hostId ? `?host=${enc(hostId)}` : ''}`),
  createCustomAddon: (body: {
    id: string;
    name: string;
    category: string;
    description: string;
    hostIds: string[];
    compose: string;
    env: string;
  }) => request<void>('POST', '/api/addons/custom', body),
  getCustomAddon: (id: string, hostId?: string) =>
    request<CustomAddonDetail>('GET', `/api/addons/custom/${enc(id)}${hostId ? `?host=${enc(hostId)}` : ''}`),
  updateCustomAddon: (
    id: string,
    hostId: string,
    body: { name: string; category: string; description: string; compose: string; env: string },
  ) => request<void>('PUT', `/api/addons/custom/${enc(id)}${hostId ? `?host=${enc(hostId)}` : ''}`, { ...body, hostId }),
  addonSources: () => request<{ sources: AddonSource[] }>('GET', '/api/addons/sources'),
  addAddonSource: (body: { name: string; url: string; allowPrivate: boolean }) =>
    request<AddonSource>('POST', '/api/addons/sources', body),
  updateAddonSource: (id: number, body: { name: string; url: string; allowPrivate: boolean; enabled: boolean }) =>
    request<AddonSource>('PUT', `/api/addons/sources/${id}`, body),
  deleteAddonSource: (id: number) => request<void>('DELETE', `/api/addons/sources/${id}`),
  refreshAddonSource: (id: number) => request<AddonSource>('POST', `/api/addons/sources/${id}/refresh`),
  moveStackToAddons: (name: string, hostId?: string) =>
    request<{ stack: string; addon: string }>(
      'POST',
      `${hostId ? `/api/hosts/${enc(hostId)}` : H()}/stacks/${enc(name)}/move-to-addons`,
    ),
  promDashboard: (opts: { scope: string; host?: string; stacks?: string; datasourceUid?: string }) => {
    const q = new URLSearchParams({ scope: opts.scope });
    if (opts.host) q.set('host', opts.host);
    if (opts.stacks) q.set('stacks', opts.stacks);
    if (opts.datasourceUid) q.set('datasourceUid', opts.datasourceUid);
    return request<Record<string, unknown>>('GET', `/api/addons/prometheus-grafana/dashboard?${q.toString()}`);
  },
  pushGrafanaDashboard: (body: { scope: string; hostId?: string; stacks?: string[]; datasourceUid?: string }) =>
    request<{ uid: string; url: string; status: string }>('POST', '/api/addons/prometheus-grafana/push', body),
  promScrape: () =>
    request<{
      victoriametrics: { scrape: string; alerts: string };
      prometheus: { scrape: string; alerts: string };
    }>('GET', '/api/addons/prometheus-grafana/scrape'),
  promHomelab: () =>
    request<{ homepage: string; glance: string; homarr: string }>('GET', '/api/addons/prometheus-grafana/homelab'),
  coolifyAction: (uuid: string, action: string, opts?: { force?: boolean; type?: string; hostId?: string }) => {
    const base = opts?.hostId ? `/api/hosts/${enc(opts.hostId)}` : H();
    return request<{ ok: boolean }>('POST', `${base}/coolify/${enc(uuid)}/actions/${enc(action)}`, {
      force: !!opts?.force,
      type: opts?.type ?? '',
    });
  },
  coolifyDeployments: (uuid: string, hostId?: string) => {
    const base = hostId ? `/api/hosts/${enc(hostId)}` : H();
    return request<{
      deployments: { uuid: string; status: string; commit?: string; logs?: string; createdAt?: string }[];
      error?: string;
    }>('GET', `${base}/coolify/${enc(uuid)}/deployments`);
  },
  coolifyGetEnvs: (uuid: string, hostId?: string) => {
    const base = hostId ? `/api/hosts/${enc(hostId)}` : H();
    return request<{ env: string }>('GET', `${base}/coolify/${enc(uuid)}/envs`);
  },
  coolifyPutEnvs: (uuid: string, env: string, redeploy: boolean, hostId?: string) => {
    const base = hostId ? `/api/hosts/${enc(hostId)}` : H();
    return request<{ ok: boolean; redeployed: boolean }>('PUT', `${base}/coolify/${enc(uuid)}/envs`, { env, redeploy });
  },
  schedulerTasks: () => request<{ tasks: ScheduledTask[] }>('GET', '/api/addons/scheduler/tasks'),
  saveSchedulerTask: (t: ScheduledTask) =>
    request<ScheduledTask>(t.id ? 'PUT' : 'POST', t.id ? `/api/addons/scheduler/tasks/${t.id}` : '/api/addons/scheduler/tasks', t),
  deleteSchedulerTask: (id: number) => request<void>('DELETE', `/api/addons/scheduler/tasks/${id}`),
  runSchedulerTask: (id: number) => request<void>('POST', `/api/addons/scheduler/tasks/${id}/run`),
  socketProxyScan: (mode: 'ro' | 'write' = 'ro') =>
    request<{ findings: SocketFinding[]; snippet: string }>('GET', `/api/addons/socket-proxy/scan?mode=${mode}`),
  registryWebhooks: () => request<{ webhooks: RegistryWebhook[] }>('GET', '/api/addons/webhooks'),
  createRegistryWebhook: (body: { name: string; provider: string; secret: string; applyNow: boolean }) =>
    request<{ webhook: RegistryWebhook; token: string; url: string }>('POST', '/api/addons/webhooks', body),
  deleteRegistryWebhook: (id: number) => request<void>('DELETE', `/api/addons/webhooks/${id}`),
  mcpTokens: () => request<{ tokens: MCPToken[]; endpoint: string }>('GET', '/api/addons/mcp/tokens'),
  createMCPToken: (body: { name: string; acl: MCPACLRule[]; requireApproval: boolean; expiresDays: number }) =>
    request<{ token: MCPToken; secret: string }>('POST', '/api/addons/mcp/tokens', body),
  deleteMCPToken: (id: number) => request<void>('DELETE', `/api/addons/mcp/tokens/${id}`),
  mcpDrafts: (status = '') =>
    request<{ drafts: MCPDraft[] }>('GET', `/api/addons/mcp/drafts${status ? `?status=${enc(status)}` : ''}`),
  approveMCPDraft: (id: number) => request<{ ok: boolean }>('POST', `/api/addons/mcp/drafts/${id}/approve`),
  rejectMCPDraft: (id: number) => request<void>('POST', `/api/addons/mcp/drafts/${id}/reject`),
  settingsConfig: () => request<SettingsConfig>('GET', '/api/settings/config'),
  saveSettingsConfig: (
    patch: Partial<{
      publicUrl: string;
      statusPublicUrl: string;
      agentPublicAddr: string;
      metricsToken: string;
      updateChannel: string;
      cleanupEnabled: boolean;
      cleanupSchedule: string;
      cleanupVolumes: boolean;
    }>,
  ) => request<SettingsConfig>('PUT', '/api/settings/config', patch),
  generateMetricsToken: () => request<{ token: string }>('POST', '/api/settings/metrics-token'),
  sisterLinks: () => request<SisterLinks>('GET', '/api/sister-links'),
  sharedVars: () => request<{ vars: SharedVar[] }>('GET', '/api/settings/shared-vars'),
  saveSharedVar: (v: { key: string; value: string; hostId: string; secret: boolean }) =>
    request<SharedVar>('POST', '/api/settings/shared-vars', v),
  deleteSharedVar: (id: number) => request<void>('DELETE', `/api/settings/shared-vars/${id}`),
  connections: () => request<ConnectionsConfig>('GET', '/api/settings/connections'),
  saveConnections: (patch: Record<string, unknown>) =>
    request<ConnectionsConfig>('PUT', '/api/settings/connections', patch),
  testOIDC: (body: { issuer: string; clientId: string; clientSecret?: string; scopes?: string[] }) =>
    request<{ ok: boolean; redirectUri?: string }>('POST', '/api/settings/connections/oidc/test', body),
  testCoolify: (body: { hostId?: string; url: string; token?: string; allowPrivate: boolean }) =>
    request<{ ok: boolean; version?: string }>('POST', '/api/settings/connections/coolify/test', body),
  testRegistry: (body: { server: string; username: string; password?: string; allowPrivate?: boolean }) =>
    request<{ ok: boolean }>('POST', '/api/settings/connections/registry/test', body),
  testGrafana: (body: { url: string; token?: string; allowPrivate: boolean }) =>
    request<{ ok: boolean }>('POST', '/api/settings/connections/grafana/test', body),
  remoteBackup: () => request<RemoteBackupConfig>('GET', '/api/settings/backup/remote'),
  saveRemoteBackup: (body: Record<string, unknown>) =>
    request<RemoteBackupConfig>('PUT', '/api/settings/backup/remote', body),
  runRemoteBackupNow: () => request<RemoteBackupLastStatus>('POST', '/api/settings/backup/remote/run'),
  /** Downloads an encrypted backup (.mbk) of the hub database, stacks and histories. */
  exportBackup: async (password: string) => {
    const res = await fetch('/api/settings/backup/export', {
      method: 'POST',
      headers: { 'X-Moorage': '1', 'Content-Type': 'application/json' },
      body: JSON.stringify({ password }),
    });
    if (!res.ok) throw new Error(await rawError(res));
    const blob = await res.blob();
    const name = /filename="([^"]+)"/.exec(res.headers.get('Content-Disposition') ?? '')?.[1] ?? 'moorage-backup.mbk';
    const url = URL.createObjectURL(blob);
    const a = document.createElement('a');
    a.href = url;
    a.download = name;
    a.click();
    setTimeout(() => URL.revokeObjectURL(url), 10_000);
  },
  /** Uploads and checks a backup; nothing changes until importBackupApply. */
  importBackupPreview: async (file: File, password: string) => {
    const res = await fetch('/api/settings/backup/import/preview', {
      method: 'POST',
      headers: { 'X-Moorage': '1', 'Content-Type': 'application/octet-stream', 'X-Backup-Password': password },
      body: file,
    });
    if (!res.ok) throw new Error(await rawError(res));
    return (await res.json()) as ImportPreview;
  },
  pushKey: () => request<{ publicKey: string; endpoints: string[] }>('GET', '/api/push/key'),
  pushSubscribe: (sub: { endpoint: string; keys: { p256dh: string; auth: string } }) =>
    request<void>('POST', '/api/push/subscriptions', sub),
  pushUnsubscribe: (endpoint: string) => request<void>('POST', '/api/push/subscriptions/delete', { endpoint }),
  pushTest: () => request<{ sent: number }>('POST', '/api/push/test'),
  importBackupApply: (token: string, database: boolean, stacks: boolean) =>
    request<{ restoredStacks: number; failedStacks: string[]; dbStaged: boolean }>('POST', '/api/settings/backup/import/apply', { token, database, stacks }),
  deletedStacks: (host?: string) => request<DeletedStack[]>('GET', `/api/hosts/${enc(host ?? app.host)}/deleted-stacks`),
  restoreDeletedStack: (name: string, hash: string, deploy = false, host?: string) =>
    request<{ deployError?: string }>('POST', `/api/hosts/${enc(host ?? app.host)}/deleted-stacks/${enc(name)}/restore`, { hash, deploy }),
  prunePreview: (hostId: string) => request<PrunePreview>('GET', `/api/hosts/${enc(hostId)}/prune/preview`),
  pruneDisk: (hostId: string, body: PruneRequest) => request<PruneResult>('POST', `/api/hosts/${enc(hostId)}/prune`, body),
  autoRollbacks: () => request<AutoRollback[]>('GET', '/api/auto-rollbacks'),
  ackAutoRollback: (id: number) => request<void>('POST', `/api/auto-rollbacks/${id}/ack`),
  logsHistory: (opts: { stack?: string; container?: string; since?: string; until?: string; tail?: number; hostId?: string }) => {
    const q = new URLSearchParams();
    if (opts.stack) q.set('stack', opts.stack);
    if (opts.container) q.set('container', opts.container);
    if (opts.since) q.set('since', opts.since);
    if (opts.until) q.set('until', opts.until);
    if (opts.tail !== undefined) q.set('tail', String(opts.tail));
    const base = opts.hostId ? `/api/hosts/${enc(opts.hostId)}` : H();
    return request<{ lines: LogLine[]; truncated: boolean }>('GET', `${base}/logs/history?${q.toString()}`);
  },
  logsDownloadUrl: (opts: { stack?: string; container?: string; since?: string; until?: string; hostId?: string }) => {
    const q = new URLSearchParams();
    if (opts.stack) q.set('stack', opts.stack);
    if (opts.container) q.set('container', opts.container);
    if (opts.since) q.set('since', opts.since);
    if (opts.until) q.set('until', opts.until);
    const base = opts.hostId ? `/api/hosts/${enc(opts.hostId)}` : H();
    const qs = q.toString();
    return `${base}/logs/download${qs ? '?' + qs : ''}`;
  },
  deps: (host?: string) => request<HostDeps>('GET', `/api/hosts/${enc(host ?? app.host)}/deps`),
  detectDeps: (host?: string) => request<DepDetection>('POST', `/api/hosts/${enc(host ?? app.host)}/deps/detect`),
  addDep: (name: string, to: StackRef, fromHost?: string) =>
    request<StackDep>('POST', `/api/hosts/${enc(fromHost ?? app.host)}/stacks/${enc(name)}/deps`, to),
  removeDep: (name: string, id: number, fromHost?: string) =>
    request<void>('DELETE', `/api/hosts/${enc(fromHost ?? app.host)}/stacks/${enc(name)}/deps/${id}`),
  ignoreDep: (name: string, id: number, ignored: boolean, fromHost?: string) =>
    request<void>('PUT', `/api/hosts/${enc(fromHost ?? app.host)}/stacks/${enc(name)}/deps/${id}/ignored`, { ignored }),
  setDepAutoRestart: (name: string, id: number, mode: AutoRestartMode, fromHost?: string) =>
    request<void>('PUT', `/api/hosts/${enc(fromHost ?? app.host)}/stacks/${enc(name)}/deps/${id}/auto-restart`, { mode }),
  savePreferences: (prefs: UserPreferences) => request<UserPreferences>('PUT', '/api/account/preferences', prefs),
  coolifyNames: () => request<{ names: Record<string, CoolifyNameMeta> }>('GET', '/api/coolify/names'),
  apiTokens: () => request<{ tokens: APIToken[] }>('GET', '/api/tokens'),
  createAPIToken: (body: { name: string; role: Role; scopes: string[]; expiresDays: number }) =>
    request<{ token: APIToken; secret: string }>('POST', '/api/tokens', body),
  deleteAPIToken: (id: number) => request<void>('DELETE', `/api/tokens/${id}`),
  /** A one-time ticket (30 s) for one terminal WebSocket; the upgrade sends it as ?ticket=. */
  terminalTicket: (req: TerminalTicketReq) =>
    request<{ ticket: string; expiresAt: string }>('POST', '/api/terminal/ticket', req),
};

export function wsURL(path: string): string {
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  return `${proto}//${location.host}${path}`;
}

/** "Docker 29.0.1", "Podman 5.4.2 (rootless)"; "" when the engine version is unknown. */
export function engineLabel(h: EngineFields | null | undefined): string {
  if (!h) return '';
  const version = h.engineVersion || h.dockerVersion;
  if (!version) return '';
  const name = h.engine === 'podman' ? 'Podman' : 'Docker';
  return `${name} ${version}${h.rootless ? ' (rootless)' : ''}`;
}

export function formatBytes(n: number): string {
  if (!n) return '0 B';
  const u = ['B', 'KB', 'MB', 'GB', 'TB'];
  const i = Math.min(Math.floor(Math.log(n) / Math.log(1024)), u.length - 1);
  const v = n / 1024 ** i;
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(1)} ${u[i]}`;
}

export const statusLabel: Record<Status, string> = {
  up: 'Up',
  degraded: 'Degraded',
  down: 'Down',
  stopped: 'Stopped',
  inactive: 'Not deployed',
};

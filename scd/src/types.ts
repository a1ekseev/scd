export type ResourceKind = 'outbound' | 'inbound' | 'routing';

export interface SourceLine {
  line: number;
  raw: string;
  trimmed: string;
}

export interface CountryInfo {
  emoji: string;
  iso2: string;
  nameEn: string;
  nameRu: string;
}

export type SkipReasonCode =
  | 'empty_line'
  | 'unsupported_scheme'
  | 'invalid_uri'
  | 'missing_required_field'
  | 'unsupported_param'
  | 'unsupported_value'
  | 'unsupported_combo';

export interface SkippedEntry {
  line: number;
  raw: string;
  label?: string;
  reasonCode: SkipReasonCode;
  reason: string;
  details?: string[];
}

export interface ParseSuccess {
  ok: true;
  kind: 'outbound';
  protocol: 'vless';
  line: number;
  raw: string;
  uri: URL;
  label: string;
  uuid: string;
  address: string;
  port: number;
  params: Record<string, string>;
}

export interface ParseFailure {
  ok: false;
  skipped: SkippedEntry;
}

export type ParseResult = ParseSuccess | ParseFailure;

export type ProfileKind = 'tcp-tls' | 'ws-tls' | 'tcp-reality-vision';
export type NetworkKind = 'tcp' | 'ws';
export type SecurityKind = 'tls' | 'reality';

export interface NormalizedOutbound {
  kind: 'outbound';
  protocol: 'vless';
  profile: ProfileKind;
  tag: string;
  line: number;
  raw: string;
  label: string;
  address: string;
  port: number;
  uuid: string;
  encryption: 'none';
  network: NetworkKind;
  security: SecurityKind;
  flow?: string;
  alpn: string[];
  sni?: string;
  host?: string;
  path?: string;
  fingerprint?: string;
  allowInsecure?: boolean;
  headerType?: string;
  publicKey?: string;
  shortId?: string;
  country?: CountryInfo;
  city?: string;
  query: Record<string, string>;
}

export interface JsonOutbound {
  tag: string;
  protocol: 'vless';
  settings: {
    vnext: Array<{
      address: string;
      port: number;
      users: Array<{
        id: string;
        encryption: 'none';
        flow?: string;
      }>;
    }>;
  };
  streamSettings: Record<string, unknown>;
}

export interface ManifestEntry {
  kind: 'outbound';
  tag: string;
  label: string;
  profile: ProfileKind;
  line: number;
  country?: CountryInfo;
  city?: string;
  normalized: NormalizedOutbound;
  jsonOutbound: JsonOutbound;
}

export interface ManifestSummary {
  totalLines: number;
  parsed: number;
  skipped: number;
  filtered: number;
  filteredByCountry: number;
  filteredByLabelRegex: number;
  unsupportedScheme: number;
  unsupportedParam: number;
  unsupportedValue: number;
  unsupportedCombo: number;
  invalidUri: number;
  missingRequiredField: number;
}

export interface OutboundManifest {
  version: 1;
  generatedAt: string;
  sourceFile: string;
  entries: ManifestEntry[];
  skipped: SkippedEntry[];
  summary: ManifestSummary;
}

export type ApplyStatus = 'added' | 'replaced' | 'removed' | 'failed';

export interface ApplyReportItem {
  id: string;
  status: ApplyStatus;
  message?: string;
}

export interface ApplyReport {
  kind: ResourceKind;
  sourceId: string;
  subscriptionId: string;
  targetAddress: string;
  appliedAt: string;
  durationMs: number;
  added: number;
  replaced: number;
  removed: number;
  failed: number;
  deletedIds: string[];
  appliedIds: string[];
  items: ApplyReportItem[];
  skipped: SkippedEntry[];
}

export interface TargetSyncReport {
  subscriptionId: string;
  targetAddress: string;
  sourceId: string;
  unchangedKinds: ResourceKind[];
  failed: number;
  resources: ApplyReport[];
  skipped: SkippedEntry[];
}

export interface SyncReport {
  appliedAt: string;
  durationMs: number;
  added: number;
  replaced: number;
  removed: number;
  unchanged: number;
  failed: number;
  targets: TargetSyncReport[];
  skipped: SkippedEntry[];
}

export interface GenerateOptions {
  input: string;
  output: string;
  log: string;
}

export type SubscriptionInputFormat = 'auto' | 'plain' | 'base64';
export type RuntimeMode = 'run-once' | 'daemon';
export type LogFormat = 'json' | 'pretty';
export type LogLevel = 'fatal' | 'error' | 'warn' | 'info' | 'debug' | 'trace' | 'silent';
export type MonitorHttpMethod = 'GET' | 'HEAD' | 'POST';

export interface InboundSocksConfig {
  listen: string;
  portRange: {
    start: number;
    end: number;
  };
}

export interface TargetMonitorRequestConfig {
  url: string;
  method: MonitorHttpMethod;
  expectedStatus: number;
  timeoutMs: number;
}

export interface BalancerMonitorSocks5Config {
  host: string;
  port: number;
}

export interface BalancerMonitorSuccessGetConfig {
  url: string;
  expectedStatus: number;
  timeoutMs: number;
}

export interface TargetBalancerMonitorConfig {
  enabled: boolean;
  schedule?: string;
  socks5?: BalancerMonitorSocks5Config;
  request?: TargetMonitorRequestConfig;
  successGet?: BalancerMonitorSuccessGetConfig;
}

export interface TargetMonitorConfig {
  enabled: boolean;
  schedule?: string;
  maxParallel: number;
  request?: TargetMonitorRequestConfig;
}

export interface TargetSpeedtestConfig {
  enabled: boolean;
  schedule?: string;
  urls?: string[];
  method: 'GET';
  expectedSizeBytes?: number;
  timeoutMs: number;
  maxParallel: number;
}

export interface SubscriptionTargetConfig {
  address: string;
  timeoutMs: number;
  fixedOutbounds: string[];
  fixedInbounds: string[];
  fixedRouting: string[];
  inboundSocks?: InboundSocksConfig;
  monitor: TargetMonitorConfig;
  balancerMonitor: TargetBalancerMonitorConfig;
  speedtest: TargetSpeedtestConfig;
  observatorySubjectSelectorPrefix?: string;
}

export interface SubscriptionFiltersConfig {
  countryAllowlist?: string[];
  labelIncludeRegex?: string;
}

export interface SubscriptionConfig {
  id: string;
  input: string;
  enabled: boolean;
  format: SubscriptionInputFormat;
  fetchTimeoutMs: number;
  filters?: SubscriptionFiltersConfig;
  targets: SubscriptionTargetConfig[];
}

export interface RuntimeConfig {
  mode: RuntimeMode;
  schedule?: string;
}

export interface LoggingConfig {
  level: LogLevel;
  format: LogFormat;
}

export interface ResourceConfig {
  outbounds: {
    enabled: boolean;
  };
  inbounds: {
    enabled: boolean;
  };
  routing: {
    enabled: boolean;
  };
}

export interface StatusServerConfig {
  enabled: boolean;
  listen?: string;
  runtimeState: {
    enabled: boolean;
    includeRaw: boolean;
    includeSecrets: boolean;
  };
}

export interface AppConfig {
  subscriptions: SubscriptionConfig[];
  runtime: RuntimeConfig;
  logging: LoggingConfig;
  resources: ResourceConfig;
  statusServer: StatusServerConfig;
}

export interface LoadedConfig {
  configPath: string;
  config: AppConfig;
}

export interface PreparedTunnelOutbound {
  tag: string;
  normalized: NormalizedOutbound;
  jsonOutbound: JsonOutbound;
}

export interface TunnelMapping {
  baseTunnelId: string;
  displayName: string;
  countryIso2?: string;
  baseOutboundTag: string;
  outboundTagInitial: string;
  outboundTagCurrent: string;
  inboundTag: string;
  routeTag: string;
  listen: string;
  port: number;
  outboundInitial: PreparedTunnelOutbound;
  outboundWithoutPrefix: PreparedTunnelOutbound;
}

export interface TargetTopology {
  tunnels: TunnelMapping[];
}

export type TunnelMonitorStatus = 'healthy' | 'degraded' | 'repairing' | 'idle';

export interface TunnelMonitorState {
  state: TunnelMonitorStatus;
  lastCheckedAt?: string;
  lastSuccessAt?: string;
  lastFailureAt?: string;
  lastStatusCode?: number;
  lastLatencyMs?: number;
  lastError?: string;
  consecutiveFailures: number;
}

export interface TunnelSpeedtestState {
  lastRunAt?: string;
  lastSuccessAt?: string;
  lastFailureAt?: string;
  lastBytes?: number;
  lastDurationMs?: number;
  lastBitsPerSecond?: number;
  lastError?: string;
}

export interface TargetBalancerMonitorState {
  state: 'idle' | 'healthy' | 'degraded';
  lastCheckedAt?: string;
  lastSuccessAt?: string;
  lastFailureAt?: string;
  lastStatusCode?: number;
  lastLatencyMs?: number;
  lastError?: string;
  consecutiveFailures: number;
  successGetLastRunAt?: string;
  successGetLastStatusCode?: number;
  successGetLastLatencyMs?: number;
  successGetLastError?: string;
}

export interface TunnelRuntimeState {
  tunnel: TunnelMapping;
  monitor: TunnelMonitorState;
  speedtest: TunnelSpeedtestState;
}

export interface StatusSnapshotTunnel {
  subscriptionId: string;
  targetAddress: string;
  displayName: string;
  countryIso2?: string;
  endpoint: string;
  state: TunnelMonitorStatus;
  lastHttpStatus?: number;
  lastLatencyMs?: number;
  lastBitsPerSecond?: number;
  balancerMonitorState?: TargetBalancerMonitorState['state'];
  balancerMonitorLastStatusCode?: number;
  balancerMonitorLastLatencyMs?: number;
  balancerMonitorSuccessGetLastStatusCode?: number;
}

export interface GroupedStatusTarget {
  subscriptionId: string;
  targetAddress: string;
  tunnels: StatusSnapshotTunnel[];
  balancerMonitor?: {
    state: TargetBalancerMonitorState['state'];
    lastStatusCode?: number;
    lastLatencyMs?: number;
    successGetLastStatusCode?: number;
  };
}

export interface GroupedStatusSubscription {
  subscriptionId: string;
  targets: GroupedStatusTarget[];
}

export interface ParsedRuntimeOutbound {
  protocol: 'vless';
  address?: string;
  port?: number;
  uuid?: string;
  encryption?: string;
  flow?: string;
  network?: string;
  security?: 'tls' | 'reality';
  sni?: string;
  alpn?: string[];
  host?: string;
  path?: string;
  fingerprint?: string;
  publicKey?: string;
  shortId?: string;
}

export interface ParsedRuntimeInbound {
  protocol: 'socks';
  listen?: string;
  portStart?: number;
  portEnd?: number;
  udp?: boolean;
}

export interface ParsedRuntimeRoutingRule {
  ruleTag: string;
  outboundTag: string;
  inboundTags: string[];
}

export interface RuntimeOutboundSnapshot {
  tag: string;
  classification: 'fixed' | 'managed-initial' | 'managed-fallback' | 'unmanaged';
  parsed?: ParsedRuntimeOutbound;
  rawBase64?: string;
}

export interface RuntimeInboundSnapshot {
  tag: string;
  classification: 'fixed' | 'managed' | 'unmanaged';
  parsed?: ParsedRuntimeInbound;
  rawBase64?: string;
}

export interface RuntimeRoutingRuleSnapshot {
  ruleTag: string;
  classification: 'fixed' | 'managed' | 'unmanaged';
  parsed: ParsedRuntimeRoutingRule;
  rawBase64?: string;
}

export interface ExpectedOutboundSnapshot {
  baseTunnelId: string;
  displayName: string;
  countryIso2?: string;
  initialTag: string;
  fallbackTag: string;
}

export interface ExpectedInboundSnapshot {
  baseTunnelId: string;
  displayName: string;
  countryIso2?: string;
  tag: string;
  endpoint: string;
}

export interface ExpectedRoutingRuleSnapshot {
  baseTunnelId: string;
  displayName: string;
  countryIso2?: string;
  ruleTag: string;
  inboundTag: string;
  outboundInitialTag: string;
  outboundFallbackTag: string;
}

export interface RuntimeStateDiff {
  matched: string[];
  missing: string[];
  unexpected: string[];
}

export interface RuntimeStateOutboundDiff extends RuntimeStateDiff {
  matchedFallback: string[];
}

export interface CurrentRuntimeStateSnapshot {
  subscriptionId: string;
  targetAddress: string;
  capturedAt: string;
  config: {
    subscription: SubscriptionConfig;
    target: SubscriptionTargetConfig;
    resources: ResourceConfig;
  };
  serviceState?: {
    balancerMonitor?: TargetBalancerMonitorState;
  };
  expected: {
    source: string;
    outbounds: ExpectedOutboundSnapshot[];
    inbounds: ExpectedInboundSnapshot[];
    routingRules: ExpectedRoutingRuleSnapshot[];
    error?: string;
  };
  runtime: {
    outbounds: RuntimeOutboundSnapshot[];
    inbounds: RuntimeInboundSnapshot[];
    routingRules: RuntimeRoutingRuleSnapshot[];
  };
  diff: {
    outbounds: RuntimeStateOutboundDiff;
    inbounds: RuntimeStateDiff;
    routingRules: RuntimeStateDiff;
  };
}

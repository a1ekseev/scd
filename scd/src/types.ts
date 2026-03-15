export type ResourceKind = 'outbound';

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
  kind: ResourceKind;
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
  kind: ResourceKind;
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
  kind: ResourceKind;
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

export interface SubscriptionTargetConfig {
  address: string;
  timeoutMs: number;
  fixedOutbounds: string[];
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
}

export interface AppConfig {
  subscriptions: SubscriptionConfig[];
  runtime: RuntimeConfig;
  logging: LoggingConfig;
  resources: ResourceConfig;
}

export interface LoadedConfig {
  configPath: string;
  config: AppConfig;
}

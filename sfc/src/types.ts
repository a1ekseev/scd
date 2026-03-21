export type SubscriptionInputFormat = 'auto' | 'plain' | 'base64';
export type LoggingLevel = 'fatal' | 'error' | 'warn' | 'info' | 'debug' | 'trace' | 'silent';
export type LoggingFormat = 'json' | 'pretty';

export interface OutputConfig {
  id: string;
  enabled: boolean;
  name?: string;
  labelIncludeRegex: string;
}

export interface SubscriptionConfig {
  id: string;
  input: string;
  enabled: boolean;
  format: SubscriptionInputFormat;
  fetchTimeoutMs: number;
  outputs: OutputConfig[];
}

export interface RuntimeConfig {
  refreshSchedule: string;
}

export interface ServerConfig {
  listen: string;
}

export interface LoggingConfig {
  level: LoggingLevel;
  format: LoggingFormat;
}

export interface AppConfig {
  subscriptions: SubscriptionConfig[];
  runtime: RuntimeConfig;
  server: ServerConfig;
  logging: LoggingConfig;
}

export interface LoadedConfig {
  configPath: string;
  config: AppConfig;
}

export interface LoadedInput {
  source: string;
  content: string;
  encoding: 'plain' | 'base64';
}

export interface LoadInputOptions {
  format?: SubscriptionInputFormat;
  fetchTimeoutMs?: number;
}

export interface SourceLine {
  line: number;
  raw: string;
  trimmed: string;
}

export interface ParsedSubscriptionEntry {
  line: number;
  raw: string;
  label: string;
}

export interface OutputRuntimeState {
  id: string;
  subscriptionId: string;
  name?: string;
  regex: string;
  lastGoodBase64?: string;
  lastGoodPlain?: string;
  lastGoodLineCount?: number;
  lastRefreshAt?: string;
  lastSuccessAt?: string;
  lastFailureAt?: string;
  lastError?: string;
}

export interface AppState {
  outputs: Record<string, OutputRuntimeState>;
  refreshInProgress: boolean;
  lastRefreshStartedAt?: string;
  lastRefreshFinishedAt?: string;
}

export interface RefreshOutputReport {
  id: string;
  subscriptionId: string;
  name?: string;
  matchedLines: number;
  ok: boolean;
  usedCachedValue: boolean;
  error?: string;
}

export interface RefreshReport {
  refreshedAt: string;
  sourceCount: number;
  outputCount: number;
  successful: number;
  failed: number;
  outputs: RefreshOutputReport[];
}

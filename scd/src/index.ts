export { applyOutbounds } from './apply/apply-outbounds.ts';
export { createOutboundApplicator, outboundApplicator } from './apply/outbound-applicator.ts';
export type { ResourceApplicator, ResourceApplyContext, ResourcePlan } from './apply/resource-applicator.ts';
export { buildOutboundGrpc } from './builders/build-outbound-grpc.ts';
export { buildOutboundJson } from './builders/build-outbound-json.ts';
export { loadConfig } from './config/load-config.ts';
export { FLAG_COUNTRY_MAP, extractCountryInfo } from './flag-country-map/index.ts';
export { decodeSubscriptionContent, loadInputSource } from './input/load-input.ts';
export { buildManifest } from './manifest.ts';
export { createLogger } from './logging/create-logger.ts';
export { normalizeVless } from './normalize/normalize-vless.ts';
export { generateManifestFromSubscription, loadSubscriptions } from './runtime/generate-manifest-from-source.ts';
export { runDaemon } from './runtime/run-daemon.ts';
export { syncOnce } from './runtime/sync-once.ts';
export { parseSubscriptionLine } from './subscription/parse-subscription-line.ts';
export { scanLines } from './subscription/scan-lines.ts';
export type {
  AppConfig,
  ApplyReport,
  CountryInfo,
  JsonOutbound,
  LoadedConfig,
  ManifestEntry,
  ManifestSummary,
  NormalizedOutbound,
  OutboundManifest,
  ParseResult,
  ResourceKind,
  SubscriptionTargetConfig,
  SubscriptionFiltersConfig,
  SubscriptionConfig,
  SkippedEntry,
  SourceLine,
  SyncReport,
  TargetSyncReport,
} from './types.ts';

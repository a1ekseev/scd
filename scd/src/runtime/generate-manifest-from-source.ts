import { buildParseLog } from '../logging/format.ts';
import { buildManifest } from '../manifest.ts';
import { loadInputSource } from '../input/load-input.ts';
import { ManifestValidationError } from '../errors.ts';
import type { OutboundManifest, SubscriptionConfig, SubscriptionFiltersConfig, SubscriptionTargetConfig } from '../types.ts';
import { applyManifestFilters } from './filter-manifest.ts';

export interface LoadedSubscription {
  id: string;
  input: string;
  source: string;
  content: string;
  encoding: 'plain' | 'base64';
  filters?: SubscriptionFiltersConfig;
  targets: SubscriptionTargetConfig[];
}

export interface FailedSubscriptionLoad {
  id: string;
  input: string;
  source: string;
  error: string;
  targets: SubscriptionTargetConfig[];
}

export interface LoadSubscriptionsResult {
  loaded: LoadedSubscription[];
  failed: FailedSubscriptionLoad[];
}

function buildNoValidEntriesMessage(manifest: OutboundManifest, sourceId: string): string {
  const summary = manifest.summary;
  return [
    `Subscription source "${sourceId}" produced no valid outbound entries.`,
    `parsed=${summary.parsed}`,
    `skipped=${summary.skipped}`,
    `filtered=${summary.filtered}`,
    `filteredByCountry=${summary.filteredByCountry}`,
    `filteredByLabelRegex=${summary.filteredByLabelRegex}`,
    `unsupportedScheme=${summary.unsupportedScheme}`,
    `unsupportedParam=${summary.unsupportedParam}`,
    `unsupportedValue=${summary.unsupportedValue}`,
    `unsupportedCombo=${summary.unsupportedCombo}`,
    `invalidUri=${summary.invalidUri}`,
    `missingRequiredField=${summary.missingRequiredField}`,
  ].join(' ');
}

export function validateManifestOrThrow(manifest: OutboundManifest, sourceId: string): OutboundManifest {
  if (manifest.summary.parsed === 0 || manifest.entries.length === 0) {
    throw new ManifestValidationError(buildNoValidEntriesMessage(manifest, sourceId));
  }
  return manifest;
}

function compareSubscriptions(left: SubscriptionConfig, right: SubscriptionConfig): number {
  return left.id.localeCompare(right.id) || left.input.localeCompare(right.input);
}

export async function loadSubscriptions(sources: SubscriptionConfig[]): Promise<LoadSubscriptionsResult> {
  const loaded: LoadedSubscription[] = [];
  const failed: FailedSubscriptionLoad[] = [];

  for (const source of [...sources].filter((item) => item.enabled).sort(compareSubscriptions)) {
    try {
      const input = await loadInputSource(source.input, {
        format: source.format,
        fetchTimeoutMs: source.fetchTimeoutMs,
      });

      loaded.push({
        id: source.id,
        input: source.input,
        source: input.source,
        content: input.content,
        encoding: input.encoding,
        filters: source.filters,
        targets: source.targets,
      });
    } catch (error) {
      failed.push({
        id: source.id,
        input: source.input,
        source: source.input,
        error: error instanceof Error ? error.message : String(error),
        targets: source.targets,
      });
    }
  }

  return { loaded, failed };
}

export function generateManifestFromSubscription(
  subscription: LoadedSubscription,
): { manifest: OutboundManifest; parseLog: ReturnType<typeof buildParseLog>; sourceId: string } {
  const sourceId = subscription.id || 'subscription';
  const manifest = validateManifestOrThrow(
    applyManifestFilters(buildManifest(subscription.content.trim(), sourceId), subscription.filters),
    sourceId,
  );
  return {
    manifest,
    parseLog: buildParseLog(manifest),
    sourceId,
  };
}

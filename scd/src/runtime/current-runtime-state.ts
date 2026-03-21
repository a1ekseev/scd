import { Buffer } from 'node:buffer';

import { XrayHandlerClient } from '../api/xray-handler-client.ts';
import {
  decodeGeneratedInbound,
  decodeGeneratedOutbound,
  decodeGeneratedRoutingRule,
  type RawInboundConfig,
  type RawOutboundConfig,
  type RawRoutingRule,
} from '../api/protobuf.ts';
import { loadInputSource } from '../input/load-input.ts';
import { buildManifest } from '../manifest.ts';
import { applyManifestFilters } from './filter-manifest.ts';
import { validateManifestOrThrow } from './generate-manifest-from-source.ts';
import { buildTargetTopology } from '../topology/build-tunnel-topology.ts';
import type { SyncMemoryState } from './run-state.ts';
import type {
  CurrentRuntimeStateSnapshot,
  ExpectedInboundSnapshot,
  ExpectedOutboundSnapshot,
  ExpectedRoutingRuleSnapshot,
  LoadedConfig,
  OutboundManifest,
  ParsedRuntimeOutbound,
  ResourceConfig,
  RuntimeInboundSnapshot,
  RuntimeOutboundSnapshot,
  RuntimeRoutingRuleSnapshot,
  RuntimeStateDiff,
  RuntimeStateOutboundDiff,
  SubscriptionConfig,
  SubscriptionTargetConfig,
} from '../types.ts';

interface CurrentRuntimeStateDependencies {
  createClient?: (target: SubscriptionTargetConfig) => Pick<XrayHandlerClient, 'listOutbounds' | 'listInbounds' | 'listRules'>;
  loadLocalInputFn?: typeof loadInputSource;
  memoryState?: SyncMemoryState;
}

interface RuntimeStateOptions {
  includeRaw: boolean;
  includeSecrets: boolean;
}

const DEFAULT_RUNTIME_STATE_OPTIONS: RuntimeStateOptions = {
  includeRaw: false,
  includeSecrets: false,
};

function now(): string {
  return new Date().toISOString();
}

function encodeBase64(value: Uint8Array): string {
  return Buffer.from(value).toString('base64');
}

function compareStrings(left: string, right: string): number {
  return left.localeCompare(right);
}

function sortValues(values: string[]): string[] {
  return [...values].sort(compareStrings);
}

function createEmptyDiff(): RuntimeStateDiff {
  return {
    matched: [],
    missing: [],
    unexpected: [],
  };
}

function createEmptyOutboundDiff(): RuntimeStateOutboundDiff {
  return {
    matched: [],
    matchedFallback: [],
    missing: [],
    unexpected: [],
  };
}

function isRemoteOrStdinInput(value: string): boolean {
  return /^https?:\/\//i.test(value) || value === '-';
}

function redactSubscriptionInput(input: string, includeSecrets: boolean): string {
  if (includeSecrets || !/^https?:\/\//i.test(input)) {
    return input;
  }

  try {
    const url = new URL(input);
    return `${url.origin}/<redacted>`;
  } catch {
    return '<redacted>';
  }
}

function redactParsedOutbound(
  parsed: ReturnType<typeof decodeGeneratedOutbound>,
  includeSecrets: boolean,
): ReturnType<typeof decodeGeneratedOutbound> {
  if (!parsed || includeSecrets) {
    return parsed;
  }

  const { uuid: _uuid, publicKey: _publicKey, shortId: _shortId, ...rest } = parsed;
  return rest satisfies ParsedRuntimeOutbound;
}

function buildExpectedTopology(manifest: OutboundManifest, target: SubscriptionTargetConfig): {
  outbounds: ExpectedOutboundSnapshot[];
  inbounds: ExpectedInboundSnapshot[];
  routingRules: ExpectedRoutingRuleSnapshot[];
} {
  const topology = buildTargetTopology(manifest, target);

  return {
    outbounds: topology.tunnels.map((tunnel) => ({
      baseTunnelId: tunnel.baseTunnelId,
      displayName: tunnel.displayName,
      countryIso2: tunnel.countryIso2,
      initialTag: tunnel.outboundTagInitial,
      fallbackTag: tunnel.outboundWithoutPrefix.tag,
    })),
    inbounds: topology.tunnels.map((tunnel) => ({
      baseTunnelId: tunnel.baseTunnelId,
      displayName: tunnel.displayName,
      countryIso2: tunnel.countryIso2,
      tag: tunnel.inboundTag,
      endpoint: `${tunnel.listen}:${tunnel.port}`,
    })),
    routingRules: topology.tunnels.map((tunnel) => ({
      baseTunnelId: tunnel.baseTunnelId,
      displayName: tunnel.displayName,
      countryIso2: tunnel.countryIso2,
      ruleTag: tunnel.routeTag,
      inboundTag: tunnel.inboundTag,
      outboundInitialTag: tunnel.outboundTagInitial,
      outboundFallbackTag: tunnel.outboundWithoutPrefix.tag,
    })),
  };
}

function buildExpectedStateFromManifest(
  manifest: OutboundManifest,
  target: SubscriptionTargetConfig,
  resources: ResourceConfig,
  source: string,
): CurrentRuntimeStateSnapshot['expected'] {
  const topology = buildExpectedTopology(manifest, target);

  return {
    source,
    outbounds: resources.outbounds.enabled ? topology.outbounds : [],
    inbounds: resources.inbounds.enabled ? topology.inbounds : [],
    routingRules: resources.routing.enabled ? topology.routingRules : [],
  };
}

async function buildExpectedState(
  subscription: SubscriptionConfig,
  target: SubscriptionTargetConfig,
  resources: ResourceConfig,
  loadLocalInputFn: typeof loadInputSource,
  options: RuntimeStateOptions,
): Promise<CurrentRuntimeStateSnapshot['expected']> {
  const source = redactSubscriptionInput(subscription.input, options.includeSecrets);

  if (isRemoteOrStdinInput(subscription.input)) {
    return {
      source,
      outbounds: [],
      inbounds: [],
      routingRules: [],
      error: 'Expected topology is unavailable for remote or stdin inputs; runtime-state does not refetch subscription sources.',
    };
  }

  try {
    const input = await loadLocalInputFn(subscription.input, {
      format: subscription.format,
      fetchTimeoutMs: subscription.fetchTimeoutMs,
    });
    const manifest = validateManifestOrThrow(
      applyManifestFilters(buildManifest(input.content.trim(), subscription.id || 'subscription'), subscription.filters),
      subscription.id || 'subscription',
    );

    return buildExpectedStateFromManifest(manifest, target, resources, input.source);
  } catch (error) {
    return {
      source,
      outbounds: [],
      inbounds: [],
      routingRules: [],
      error: error instanceof Error ? error.message : String(error),
    };
  }
}

function buildRuntimeOutbounds(
  runtime: RawOutboundConfig[],
  target: SubscriptionTargetConfig,
  expected: ExpectedOutboundSnapshot[],
  options: RuntimeStateOptions,
): { items: RuntimeOutboundSnapshot[]; diff: RuntimeStateOutboundDiff } {
  const initialTags = new Set(expected.map((item) => item.initialTag));
  const fallbackTags = new Set(expected.map((item) => item.fallbackTag));
  const runtimeTags = new Set(runtime.map((item) => item.tag));
  const diff = createEmptyOutboundDiff();

  const items = runtime
    .map((item) => {
      let classification: RuntimeOutboundSnapshot['classification'] = 'unmanaged';
      if (target.fixedOutbounds.includes(item.tag)) {
        classification = 'fixed';
      } else if (initialTags.has(item.tag)) {
        classification = 'managed-initial';
      } else if (fallbackTags.has(item.tag)) {
        classification = 'managed-fallback';
      }

      if (classification === 'managed-initial') {
        diff.matched.push(item.tag);
      } else if (classification === 'managed-fallback') {
        diff.matchedFallback.push(item.tag);
      } else if (classification === 'unmanaged') {
        diff.unexpected.push(item.tag);
      }

      const parsed = redactParsedOutbound(decodeGeneratedOutbound(item.raw), options.includeSecrets);

      return {
        tag: item.tag,
        classification,
        parsed,
        rawBase64: options.includeRaw ? encodeBase64(item.raw) : undefined,
      } satisfies RuntimeOutboundSnapshot;
    })
    .sort((left, right) => left.tag.localeCompare(right.tag));

  for (const expectedItem of expected) {
    if (!runtimeTags.has(expectedItem.initialTag) && !runtimeTags.has(expectedItem.fallbackTag)) {
      diff.missing.push(expectedItem.baseTunnelId);
    }
  }

  diff.matched = sortValues(diff.matched);
  diff.matchedFallback = sortValues(diff.matchedFallback);
  diff.missing = sortValues(diff.missing);
  diff.unexpected = sortValues(diff.unexpected);

  return { items, diff };
}

function buildRuntimeInbounds(
  runtime: RawInboundConfig[],
  target: SubscriptionTargetConfig,
  expected: ExpectedInboundSnapshot[],
  options: RuntimeStateOptions,
): { items: RuntimeInboundSnapshot[]; diff: RuntimeStateDiff } {
  const expectedTags = new Set(expected.map((item) => item.tag));
  const diff = createEmptyDiff();

  const items = runtime
    .map((item) => {
      let classification: RuntimeInboundSnapshot['classification'] = 'unmanaged';
      if (target.fixedInbounds.includes(item.tag)) {
        classification = 'fixed';
      } else if (expectedTags.has(item.tag)) {
        classification = 'managed';
      }

      if (classification === 'managed') {
        diff.matched.push(item.tag);
      } else if (classification === 'unmanaged') {
        diff.unexpected.push(item.tag);
      }

      return {
        tag: item.tag,
        classification,
        parsed: decodeGeneratedInbound(item.raw),
        rawBase64: options.includeRaw ? encodeBase64(item.raw) : undefined,
      } satisfies RuntimeInboundSnapshot;
    })
    .sort((left, right) => left.tag.localeCompare(right.tag));

  const runtimeTags = new Set(runtime.map((item) => item.tag));
  for (const expectedItem of expected) {
    if (!runtimeTags.has(expectedItem.tag)) {
      diff.missing.push(expectedItem.tag);
    }
  }

  diff.matched = sortValues(diff.matched);
  diff.missing = sortValues(diff.missing);
  diff.unexpected = sortValues(diff.unexpected);

  return { items, diff };
}

function buildRuntimeRoutingRules(
  runtime: RawRoutingRule[],
  target: SubscriptionTargetConfig,
  expected: ExpectedRoutingRuleSnapshot[],
  options: RuntimeStateOptions,
): { items: RuntimeRoutingRuleSnapshot[]; diff: RuntimeStateDiff } {
  const expectedTags = new Set(expected.map((item) => item.ruleTag));
  const diff = createEmptyDiff();

  const items = runtime
    .map((item) => {
      let classification: RuntimeRoutingRuleSnapshot['classification'] = 'unmanaged';
      if (target.fixedRouting.includes(item.ruleTag)) {
        classification = 'fixed';
      } else if (expectedTags.has(item.ruleTag)) {
        classification = 'managed';
      }

      if (classification === 'managed') {
        diff.matched.push(item.ruleTag);
      } else if (classification === 'unmanaged') {
        diff.unexpected.push(item.ruleTag);
      }

      return {
        ruleTag: item.ruleTag,
        classification,
        parsed: decodeGeneratedRoutingRule(item.raw),
        rawBase64: options.includeRaw ? encodeBase64(item.raw) : undefined,
      } satisfies RuntimeRoutingRuleSnapshot;
    })
    .sort((left, right) => left.ruleTag.localeCompare(right.ruleTag));

  const runtimeTags = new Set(runtime.map((item) => item.ruleTag));
  for (const expectedItem of expected) {
    if (!runtimeTags.has(expectedItem.ruleTag)) {
      diff.missing.push(expectedItem.ruleTag);
    }
  }

  diff.matched = sortValues(diff.matched);
  diff.missing = sortValues(diff.missing);
  diff.unexpected = sortValues(diff.unexpected);

  return { items, diff };
}

function findSubscriptionAndTarget(
  loadedConfig: LoadedConfig,
  subscriptionId: string,
  targetAddress: string,
): { subscription: SubscriptionConfig; target: SubscriptionTargetConfig } | undefined {
  const subscription = loadedConfig.config.subscriptions.find((item) => item.id === subscriptionId);
  if (!subscription) {
    return undefined;
  }

  const target = subscription.targets.find((item) => item.address === targetAddress);
  if (!target) {
    return undefined;
  }

  return { subscription, target };
}

export async function buildCurrentRuntimeStateSnapshot(
  loadedConfig: LoadedConfig,
  subscriptionId: string,
  targetAddress: string,
  dependencies: CurrentRuntimeStateDependencies = {},
  options: Partial<RuntimeStateOptions> = {},
): Promise<CurrentRuntimeStateSnapshot | undefined> {
  const configured = findSubscriptionAndTarget(loadedConfig, subscriptionId, targetAddress);
  if (!configured) {
    return undefined;
  }

  const runtimeStateOptions = { ...DEFAULT_RUNTIME_STATE_OPTIONS, ...options };
  const loadLocalInputFn = dependencies.loadLocalInputFn ?? loadInputSource;
  const createClient =
    dependencies.createClient ??
    ((runtimeTarget: SubscriptionTargetConfig) => new XrayHandlerClient(runtimeTarget.address, { timeoutMs: runtimeTarget.timeoutMs }));

  const { subscription, target } = configured;
  const expected = await buildExpectedState(subscription, target, loadedConfig.config.resources, loadLocalInputFn, runtimeStateOptions);
  const client = createClient(target);
  const [outbounds, inbounds, routingRules] = await Promise.all([
    client.listOutbounds(),
    client.listInbounds(),
    client.listRules(),
  ]);

  const runtimeOutbounds = buildRuntimeOutbounds(outbounds, target, expected.outbounds, runtimeStateOptions);
  const runtimeInbounds = buildRuntimeInbounds(inbounds, target, expected.inbounds, runtimeStateOptions);
  const runtimeRoutingRules = buildRuntimeRoutingRules(routingRules, target, expected.routingRules, runtimeStateOptions);
  const targetKey = `${subscriptionId}::${targetAddress}`;
  const balancerMonitor = dependencies.memoryState?.targets[targetKey]?.balancerMonitor;

  return {
    subscriptionId,
    targetAddress,
    capturedAt: now(),
    config: {
      subscription: {
        ...subscription,
        input: redactSubscriptionInput(subscription.input, runtimeStateOptions.includeSecrets),
      },
      target,
      resources: loadedConfig.config.resources,
    },
    serviceState: {
      balancerMonitor,
    },
    expected,
    runtime: {
      outbounds: runtimeOutbounds.items,
      inbounds: runtimeInbounds.items,
      routingRules: runtimeRoutingRules.items,
    },
    diff: {
      outbounds: runtimeOutbounds.diff,
      inbounds: runtimeInbounds.diff,
      routingRules: runtimeRoutingRules.diff,
    },
  };
}

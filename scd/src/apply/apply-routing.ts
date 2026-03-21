import type { ApplyReport, TargetTopology } from '../types.ts';
import { buildRoutingGrpc } from '../builders/build-routing-grpc.ts';
import { XrayHandlerClient } from '../api/xray-handler-client.ts';

export interface RoutingApiClient {
  listRules(): Promise<Array<{ tag: string; ruleTag: string }>>;
  removeRule(ruleTag: string): Promise<void>;
  addRule(rawRule: Uint8Array): Promise<void>;
}

export interface ApplyRoutingOptions {
  subscriptionId?: string;
  targetAddress?: string;
  fixedRouting?: string[];
}

function buildBaseReport(
  sourceId: string,
  subscriptionId: string,
  targetAddress: string,
  startedAt: number,
): ApplyReport {
  return {
    kind: 'routing',
    sourceId,
    subscriptionId,
    targetAddress,
    appliedAt: new Date().toISOString(),
    durationMs: Date.now() - startedAt,
    added: 0,
    replaced: 0,
    removed: 0,
    failed: 0,
    deletedIds: [],
    appliedIds: [],
    items: [],
    skipped: [],
  };
}

export async function applyRouting(
  topology: TargetTopology,
  apiAddressOrClient: string | RoutingApiClient,
  sourceId: string,
  options: ApplyRoutingOptions = {},
): Promise<ApplyReport> {
  const startedAt = Date.now();
  const targetAddress = options.targetAddress ?? (typeof apiAddressOrClient === 'string' ? apiAddressOrClient : 'client');
  const subscriptionId = options.subscriptionId ?? sourceId;
  const report = buildBaseReport(sourceId, subscriptionId, targetAddress, startedAt);
  const client = typeof apiAddressOrClient === 'string' ? new XrayHandlerClient(apiAddressOrClient) : apiAddressOrClient;
  const fixedRuleTags = new Set(options.fixedRouting ?? []);

  for (const tunnel of topology.tunnels) {
    if (fixedRuleTags.has(tunnel.routeTag)) {
      report.failed = 1;
      report.items.push({
        id: tunnel.routeTag,
        status: 'failed',
        message: `Managed routing rule "${tunnel.routeTag}" conflicts with fixedRouting on target "${targetAddress}".`,
      });
      report.durationMs = Date.now() - startedAt;
      return report;
    }
  }

  const existing = await client.listRules();
  const existingRuleTags = new Set(existing.map((item) => item.ruleTag));

  try {
    for (const rule of existing) {
      if (fixedRuleTags.has(rule.ruleTag)) {
        continue;
      }

      await client.removeRule(rule.ruleTag);
      report.removed += 1;
      report.deletedIds.push(rule.ruleTag);
      report.items.push({
        id: rule.ruleTag,
        status: 'removed',
      });
    }

    for (const tunnel of topology.tunnels) {
      const encoded = buildRoutingGrpc(tunnel);
      await client.addRule(encoded.raw);
      report.appliedIds.push(encoded.ruleTag);
      if (existingRuleTags.has(encoded.ruleTag)) {
        report.replaced += 1;
      } else {
        report.added += 1;
      }
      report.items.push({
        id: encoded.ruleTag,
        status: existingRuleTags.has(encoded.ruleTag) ? 'replaced' : 'added',
      });
    }
  } catch (error) {
    report.failed = 1;
    report.items.push({
      id: topology.tunnels[0]?.routeTag ?? 'routing',
      status: 'failed',
      message: error instanceof Error ? error.message : String(error),
    });
  }

  report.durationMs = Date.now() - startedAt;
  report.appliedAt = new Date().toISOString();
  return report;
}

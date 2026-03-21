import type { ApplyReport, TargetTopology } from '../types.ts';
import { buildInboundGrpc } from '../builders/build-inbound-grpc.ts';
import { XrayHandlerClient } from '../api/xray-handler-client.ts';

export interface InboundApiClient {
  listInbounds(): Promise<Array<{ tag: string; raw: Uint8Array }>>;
  removeInbound(tag: string): Promise<void>;
  addInbound(rawInbound: Uint8Array): Promise<void>;
}

export interface ApplyInboundsOptions {
  subscriptionId?: string;
  targetAddress?: string;
  fixedInbounds?: string[];
}

function buildBaseReport(
  sourceId: string,
  subscriptionId: string,
  targetAddress: string,
  startedAt: number,
): ApplyReport {
  return {
    kind: 'inbound',
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

export async function applyInbounds(
  topology: TargetTopology,
  apiAddressOrClient: string | InboundApiClient,
  sourceId: string,
  options: ApplyInboundsOptions = {},
): Promise<ApplyReport> {
  const startedAt = Date.now();
  const targetAddress = options.targetAddress ?? (typeof apiAddressOrClient === 'string' ? apiAddressOrClient : 'client');
  const subscriptionId = options.subscriptionId ?? sourceId;
  const report = buildBaseReport(sourceId, subscriptionId, targetAddress, startedAt);
  const client = typeof apiAddressOrClient === 'string' ? new XrayHandlerClient(apiAddressOrClient) : apiAddressOrClient;
  const fixedTags = new Set(options.fixedInbounds ?? []);

  for (const tunnel of topology.tunnels) {
    if (fixedTags.has(tunnel.inboundTag)) {
      report.failed = 1;
      report.items.push({
        id: tunnel.inboundTag,
        status: 'failed',
        message: `Managed inbound tag "${tunnel.inboundTag}" conflicts with fixedInbounds on target "${targetAddress}".`,
      });
      report.durationMs = Date.now() - startedAt;
      return report;
    }
  }

  const existing = await client.listInbounds();
  const existingTags = new Set(existing.map((item) => item.tag));

  try {
    for (const item of existing) {
      if (fixedTags.has(item.tag)) {
        continue;
      }

      await client.removeInbound(item.tag);
      report.removed += 1;
      report.deletedIds.push(item.tag);
      report.items.push({
        id: item.tag,
        status: 'removed',
      });
    }

    for (const tunnel of topology.tunnels) {
      const encoded = buildInboundGrpc(tunnel);
      await client.addInbound(encoded.raw);
      report.appliedIds.push(encoded.tag);
      if (existingTags.has(encoded.tag)) {
        report.replaced += 1;
      } else {
        report.added += 1;
      }
      report.items.push({
        id: encoded.tag,
        status: existingTags.has(encoded.tag) ? 'replaced' : 'added',
      });
    }
  } catch (error) {
    report.failed = 1;
    report.items.push({
      id: topology.tunnels[0]?.inboundTag ?? 'inbound',
      status: 'failed',
      message: error instanceof Error ? error.message : String(error),
    });
  }

  report.durationMs = Date.now() - startedAt;
  report.appliedAt = new Date().toISOString();
  return report;
}

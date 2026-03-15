import type { ApplyReport, OutboundManifest } from '../types.ts';
import { buildOutboundGrpc } from '../builders/build-outbound-grpc.ts';
import { XrayHandlerClient } from '../api/xray-handler-client.ts';
import type { RawOutboundConfig } from '../api/protobuf.ts';

export interface OutboundApiClient {
  listOutbounds(): Promise<RawOutboundConfig[]>;
  removeOutbound(tag: string): Promise<void>;
  addOutbound(rawOutbound: Uint8Array): Promise<void>;
}

export interface ApplyOutboundsOptions {
  subscriptionId?: string;
  targetAddress?: string;
  fixedOutbounds?: string[];
}

function buildBaseReport(
  sourceId: string,
  subscriptionId: string,
  targetAddress: string,
  startedAt: number,
  manifest: OutboundManifest,
): ApplyReport {
  return {
    kind: 'outbound',
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
    skipped: manifest.skipped,
  };
}

export async function applyOutbounds(
  manifest: OutboundManifest,
  apiAddressOrClient: string | OutboundApiClient,
  sourceId = manifest.sourceFile || 'manifest',
  options: ApplyOutboundsOptions = {},
): Promise<ApplyReport> {
  const startedAt = Date.now();
  const targetAddress = options.targetAddress ?? (typeof apiAddressOrClient === 'string' ? apiAddressOrClient : 'client');
  const subscriptionId = options.subscriptionId ?? sourceId;
  const report = buildBaseReport(sourceId, subscriptionId, targetAddress, startedAt, manifest);
  const client =
    typeof apiAddressOrClient === 'string'
      ? new XrayHandlerClient(apiAddressOrClient)
      : apiAddressOrClient;
  const fixedTags = new Set(options.fixedOutbounds ?? []);

  for (const entry of manifest.entries) {
    if (fixedTags.has(entry.tag)) {
      report.failed = 1;
      report.items.push({
        id: entry.tag,
        status: 'failed',
        message: `Managed outbound tag "${entry.tag}" conflicts with fixedOutbounds on target "${targetAddress}".`,
      });
      report.durationMs = Date.now() - startedAt;
      return report;
    }
  }

  const existing = await client.listOutbounds();
  const existingByTag = new Map(existing.map((item) => [item.tag, item.raw]));
  const rollbackBackups = new Map<string, Uint8Array>();
  const removedTags = new Set<string>();
  const addedDuringApply: string[] = [];
  let currentId = '';

  const removeWithBackup = async (tag: string) => {
    if (removedTags.has(tag) || fixedTags.has(tag)) {
      return;
    }

    const backup = existingByTag.get(tag);
    if (!backup) {
      return;
    }

    await client.removeOutbound(tag);
    rollbackBackups.set(tag, backup);
    removedTags.add(tag);
    report.removed += 1;
    report.deletedIds.push(tag);
    report.items.push({
      id: tag,
      status: 'removed',
    });
  };

  try {
    for (const existingOutbound of existing) {
      await removeWithBackup(existingOutbound.tag);
    }

    for (const entry of manifest.entries) {
      currentId = entry.tag;
      const encoded = buildOutboundGrpc(entry.normalized);

      await client.addOutbound(encoded.raw);
      addedDuringApply.push(entry.tag);
      report.appliedIds.push(entry.tag);

      const status = removedTags.has(entry.tag) ? 'replaced' : 'added';
      if (status === 'replaced') {
        report.replaced += 1;
      } else {
        report.added += 1;
      }

      report.items.push({
        id: entry.tag,
        status,
      });
    }
  } catch (error) {
    for (const tag of [...addedDuringApply].reverse()) {
      try {
        await client.removeOutbound(tag);
      } catch {
        // Best-effort rollback.
      }
    }

    for (const [tag, backup] of rollbackBackups) {
      try {
        await client.addOutbound(backup);
      } catch {
        // Best-effort rollback.
      }
    }

    report.failed = 1;
    report.items.push({
      id: currentId || addedDuringApply.at(-1) || manifest.entries[0]?.tag || 'unknown',
      status: 'failed',
      message: error instanceof Error ? error.message : String(error),
    });
  }

  report.durationMs = Date.now() - startedAt;
  report.appliedAt = new Date().toISOString();
  return report;
}

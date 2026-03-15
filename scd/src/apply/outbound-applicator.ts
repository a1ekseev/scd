import { createHash } from 'node:crypto';

import type { ManifestEntry, OutboundManifest } from '../types.ts';
import { XrayHandlerClient } from '../api/xray-handler-client.ts';
import { generateManifestFromSubscription } from '../runtime/generate-manifest-from-source.ts';
import type { ResourceApplicator, ResourcePlan } from './resource-applicator.ts';
import { applyOutbounds } from './apply-outbounds.ts';
import type { OutboundApiClient } from './apply-outbounds.ts';

export interface OutboundResourcePlan extends ResourcePlan {
  kind: 'outbound';
  manifest: OutboundManifest;
}

function buildManifestHash(manifest: OutboundManifest): string {
  return createHash('sha256')
    .update(JSON.stringify(manifest.entries.map((entry) => entry.jsonOutbound)))
    .digest('hex');
}

function compareManifestEntries(left: ManifestEntry, right: ManifestEntry): number {
  return left.tag.localeCompare(right.tag) || left.line - right.line || left.normalized.raw.localeCompare(right.normalized.raw);
}

function sortManifest(manifest: OutboundManifest): OutboundManifest {
  return {
    ...manifest,
    entries: [...manifest.entries].sort(compareManifestEntries),
  };
}

function withEffectiveTag(tag: string, prefix?: string): string {
  if (!prefix) {
    return tag;
  }
  return `${prefix}${tag}`;
}

function buildTargetManifest(manifest: OutboundManifest, prefix?: string): OutboundManifest {
  if (!prefix) {
    return manifest;
  }

  return {
    ...manifest,
    entries: manifest.entries.map((entry) => {
      const tag = withEffectiveTag(entry.tag, prefix);
      return {
        ...entry,
        tag,
        normalized: {
          ...entry.normalized,
          tag,
        },
        jsonOutbound: {
          ...entry.jsonOutbound,
          tag,
        },
      };
    }),
  };
}

export function createOutboundApplicator(
  createClient: (target: { address: string; timeoutMs: number }) => OutboundApiClient = (target) =>
    new XrayHandlerClient(target.address, {
      timeoutMs: target.timeoutMs,
    }),
): ResourceApplicator<OutboundResourcePlan> {
  return {
    kind: 'outbound',
    isEnabled(config) {
      return config.resources.outbounds.enabled;
    },
    buildPlan(subscription) {
      const { manifest, sourceId } = generateManifestFromSubscription(subscription);
      const sortedManifest = sortManifest(manifest);
      return {
        kind: 'outbound',
        sourceId,
        skipped: sortedManifest.skipped,
        manifest: sortedManifest,
        manifestHash: buildManifestHash(sortedManifest),
        managedIds: sortedManifest.entries.map((entry) => entry.tag),
      };
    },
    preparePlanForTarget(plan, target) {
      const targetManifest = sortManifest(buildTargetManifest(plan.manifest, target.observatorySubjectSelectorPrefix));
      return {
        ...plan,
        manifest: targetManifest,
        manifestHash: buildManifestHash(targetManifest),
        managedIds: targetManifest.entries.map((entry) => entry.tag),
      };
    },
    async applyPlan(plan, context) {
      const client = createClient(context.target);

      return applyOutbounds(plan.manifest, client, plan.sourceId, {
        subscriptionId: context.subscriptionId,
        targetAddress: context.target.address,
        fixedOutbounds: context.target.fixedOutbounds,
      });
    },
  };
}

export const outboundApplicator: ResourceApplicator<OutboundResourcePlan> = createOutboundApplicator();

import { createHash } from 'node:crypto';

import { XrayHandlerClient } from '../api/xray-handler-client.ts';
import { generateManifestFromSubscription } from '../runtime/generate-manifest-from-source.ts';
import { buildTargetTopology } from '../topology/build-tunnel-topology.ts';
import type { OutboundManifest, TargetTopology } from '../types.ts';
import { applyInbounds, type InboundApiClient } from './apply-inbounds.ts';
import type { ResourceApplicator, ResourcePlan } from './resource-applicator.ts';

export interface InboundResourcePlan extends ResourcePlan {
  kind: 'inbound';
  manifest: OutboundManifest;
  topology: TargetTopology;
}

function buildManifestHash(topology: TargetTopology): string {
  return createHash('sha256')
    .update(
      JSON.stringify(
        topology.tunnels.map((tunnel) => ({
          inboundTag: tunnel.inboundTag,
          listen: tunnel.listen,
          port: tunnel.port,
        })),
      ),
    )
    .digest('hex');
}

export function createInboundApplicator(
  createClient: (target: { address: string; timeoutMs: number }) => InboundApiClient = (target) =>
    new XrayHandlerClient(target.address, {
      timeoutMs: target.timeoutMs,
    }),
): ResourceApplicator<InboundResourcePlan> {
  return {
    kind: 'inbound',
    isEnabled(config) {
      return config.resources.inbounds.enabled;
    },
    buildPlan(subscription) {
      const { manifest, sourceId } = generateManifestFromSubscription(subscription);
      return {
        kind: 'inbound',
        sourceId,
        skipped: manifest.skipped,
        manifest,
        topology: { tunnels: [] },
        manifestHash: buildManifestHash({ tunnels: [] }),
        managedIds: [],
      };
    },
    preparePlanForTarget(plan, target) {
      const topology = buildTargetTopology(plan.manifest, target);
      return {
        ...plan,
        topology,
        manifestHash: buildManifestHash(topology),
        managedIds: topology.tunnels.map((tunnel) => tunnel.inboundTag),
      };
    },
    async applyPlan(plan, context) {
      const client = createClient(context.target);
      return applyInbounds(plan.topology, client, plan.sourceId, {
        subscriptionId: context.subscriptionId,
        targetAddress: context.target.address,
        fixedInbounds: context.target.fixedInbounds,
      });
    },
  };
}

export const inboundApplicator: ResourceApplicator<InboundResourcePlan> = createInboundApplicator();

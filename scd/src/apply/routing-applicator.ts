import { createHash } from 'node:crypto';

import { XrayHandlerClient } from '../api/xray-handler-client.ts';
import { generateManifestFromSubscription } from '../runtime/generate-manifest-from-source.ts';
import { buildTargetTopology } from '../topology/build-tunnel-topology.ts';
import type { OutboundManifest, TargetTopology } from '../types.ts';
import { applyRouting, type RoutingApiClient } from './apply-routing.ts';
import type { ResourceApplicator, ResourcePlan } from './resource-applicator.ts';

export interface RoutingResourcePlan extends ResourcePlan {
  kind: 'routing';
  manifest: OutboundManifest;
  topology: TargetTopology;
}

function buildManifestHash(topology: TargetTopology): string {
  return createHash('sha256')
    .update(
      JSON.stringify(
        topology.tunnels.map((tunnel) => ({
          routeTag: tunnel.routeTag,
          inboundTag: tunnel.inboundTag,
          outboundTag: tunnel.outboundTagCurrent,
        })),
      ),
    )
    .digest('hex');
}

export function createRoutingApplicator(
  createClient: (target: { address: string; timeoutMs: number }) => RoutingApiClient = (target) =>
    new XrayHandlerClient(target.address, {
      timeoutMs: target.timeoutMs,
    }),
): ResourceApplicator<RoutingResourcePlan> {
  return {
    kind: 'routing',
    isEnabled(config) {
      return config.resources.routing.enabled;
    },
    buildPlan(subscription) {
      const { manifest, sourceId } = generateManifestFromSubscription(subscription);
      return {
        kind: 'routing',
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
        managedIds: topology.tunnels.map((tunnel) => tunnel.routeTag),
      };
    },
    async applyPlan(plan, context) {
      const client = createClient(context.target);
      return applyRouting(plan.topology, client, plan.sourceId, {
        subscriptionId: context.subscriptionId,
        targetAddress: context.target.address,
        fixedRouting: context.target.fixedRouting,
      });
    },
  };
}

export const routingApplicator: ResourceApplicator<RoutingResourcePlan> = createRoutingApplicator();

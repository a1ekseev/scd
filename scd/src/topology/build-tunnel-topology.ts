import type {
  ManifestEntry,
  OutboundManifest,
  PreparedTunnelOutbound,
  SubscriptionTargetConfig,
  TargetTopology,
  TunnelMapping,
} from '../types.ts';
import { overrideOutboundForTarget } from '../targets/override-outbound-for-target.ts';

function compareEntries(left: ManifestEntry, right: ManifestEntry): number {
  return left.tag.localeCompare(right.tag) || left.line - right.line || left.normalized.raw.localeCompare(right.normalized.raw);
}

function withEffectiveTag(tag: string, prefix?: string): string {
  if (!prefix) {
    return tag;
  }

  return `${prefix}${tag}`;
}

function createPreparedOutbound(
  entry: ManifestEntry,
  target: SubscriptionTargetConfig,
  tag: string,
): PreparedTunnelOutbound {
  const effectiveEntry = overrideOutboundForTarget(entry, target, tag);

  return {
    tag,
    normalized: effectiveEntry.normalized,
    jsonOutbound: effectiveEntry.jsonOutbound,
  };
}

export function buildTargetTopology(
  manifest: OutboundManifest,
  target: SubscriptionTargetConfig,
): TargetTopology {
  if (!target.inboundSocks) {
    return {
      tunnels: [],
    };
  }

  const entries = [...manifest.entries].sort(compareEntries);
  const portCount = target.inboundSocks.portRange.end - target.inboundSocks.portRange.start + 1;
  if (entries.length > portCount) {
    throw new Error(
      `Target "${target.address}" inboundSocks.portRange can allocate only ${portCount} ports, but ${entries.length} tunnels were generated.`,
    );
  }

  const tunnels: TunnelMapping[] = entries.map((entry, index) => {
    const baseTunnelId = entry.tag;
    const outboundTagInitial = withEffectiveTag(entry.tag, target.observatorySubjectSelectorPrefix);
    const outboundInitial = createPreparedOutbound(entry, target, outboundTagInitial);
    const outboundWithoutPrefix = createPreparedOutbound(entry, target, entry.tag);

    return {
      baseTunnelId,
      displayName: entry.label,
      countryIso2: entry.country?.iso2,
      baseOutboundTag: entry.tag,
      outboundTagInitial,
      outboundTagCurrent: outboundTagInitial,
      inboundTag: `in-${baseTunnelId}`,
      routeTag: `route-${baseTunnelId}`,
      listen: target.inboundSocks!.listen,
      port: target.inboundSocks!.portRange.start + index,
      outboundInitial,
      outboundWithoutPrefix,
    };
  });

  return { tunnels };
}

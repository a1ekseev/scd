import { buildOutboundJson } from '../builders/build-outbound-json.ts';
import type { ManifestEntry, NormalizedOutbound, SubscriptionTargetConfig } from '../types.ts';

const VISION_FLOW = 'xtls-rprx-vision';
const VISION_UDP443_FLOW = 'xtls-rprx-vision-udp443';

function getEffectiveFlow(
  normalized: NormalizedOutbound,
  target: Pick<SubscriptionTargetConfig, 'visionUdp443Override'>,
): string | undefined {
  if (
    target.visionUdp443Override &&
    normalized.protocol === 'vless' &&
    normalized.security === 'reality' &&
    normalized.flow === VISION_FLOW
  ) {
    return VISION_UDP443_FLOW;
  }

  return normalized.flow;
}

export function overrideOutboundForTarget(
  entry: ManifestEntry,
  target: Pick<SubscriptionTargetConfig, 'visionUdp443Override'>,
  tag: string = entry.tag,
): ManifestEntry {
  const normalized: NormalizedOutbound = {
    ...entry.normalized,
    tag,
    flow: getEffectiveFlow(entry.normalized, target),
  };

  return {
    ...entry,
    tag,
    normalized,
    jsonOutbound: buildOutboundJson(normalized),
  };
}

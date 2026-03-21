import type { TunnelMapping } from '../types.ts';
import {
  concatBytes,
  encodeBytesField,
  encodeMessageField,
  encodeRepeatedStringField,
  encodeStringField,
} from '../api/protobuf.ts';

function encodeTypedMessage(type: string, value: Uint8Array): Uint8Array {
  return concatBytes([
    encodeStringField(1, type),
    encodeBytesField(2, value),
  ]);
}

function encodeRoutingRule(tunnel: TunnelMapping): Uint8Array {
  return concatBytes([
    encodeStringField(1, tunnel.outboundTagCurrent),
    encodeRepeatedStringField(8, [tunnel.inboundTag]),
    encodeStringField(19, tunnel.routeTag),
  ]);
}

function encodeRoutingConfig(tunnel: TunnelMapping): Uint8Array {
  return encodeMessageField(2, encodeRoutingRule(tunnel));
}

export interface GrpcRoutingRuleConfig {
  ruleTag: string;
  raw: Uint8Array;
}

export function buildRoutingGrpc(tunnel: TunnelMapping): GrpcRoutingRuleConfig {
  return {
    ruleTag: tunnel.routeTag,
    raw: encodeTypedMessage('xray.app.router.Config', encodeRoutingConfig(tunnel)),
  };
}

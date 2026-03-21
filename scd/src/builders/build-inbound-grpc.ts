import { isIP } from 'node:net';

import type { TunnelMapping } from '../types.ts';
import {
  concatBytes,
  encodeBoolField,
  encodeBytesField,
  encodeMessageField,
  encodeStringField,
  encodeUInt32Field,
} from '../api/protobuf.ts';

function encodeTypedMessage(type: string, value: Uint8Array): Uint8Array {
  return concatBytes([
    encodeStringField(1, type),
    encodeBytesField(2, value),
  ]);
}

function encodeIpOrDomain(value: string): Uint8Array {
  const version = isIP(value);
  if (version === 4) {
    return encodeBytesField(1, Uint8Array.from(value.split('.').map((octet) => Number(octet))));
  }

  return encodeStringField(2, value);
}

function encodePortRange(port: number): Uint8Array {
  return concatBytes([
    encodeUInt32Field(1, port),
    encodeUInt32Field(2, port),
  ]);
}

function encodePortList(port: number): Uint8Array {
  return encodeMessageField(1, encodePortRange(port));
}

function encodeReceiverConfig(tunnel: TunnelMapping): Uint8Array {
  return concatBytes([
    encodeMessageField(1, encodePortList(tunnel.port)),
    encodeMessageField(2, encodeIpOrDomain(tunnel.listen)),
  ]);
}

function encodeSocksServerConfig(): Uint8Array {
  return concatBytes([
    encodeBoolField(4, true),
  ]);
}

export interface GrpcInboundHandlerConfig {
  tag: string;
  raw: Uint8Array;
}

export function buildInboundGrpc(tunnel: TunnelMapping): GrpcInboundHandlerConfig {
  const raw = concatBytes([
    encodeStringField(1, tunnel.inboundTag),
    encodeMessageField(
      2,
      encodeTypedMessage('xray.app.proxyman.ReceiverConfig', encodeReceiverConfig(tunnel)),
    ),
    encodeMessageField(
      3,
      encodeTypedMessage('xray.proxy.socks.ServerConfig', encodeSocksServerConfig()),
    ),
  ]);

  return {
    tag: tunnel.inboundTag,
    raw,
  };
}

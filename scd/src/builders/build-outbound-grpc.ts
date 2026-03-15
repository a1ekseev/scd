import { Buffer } from 'node:buffer';

import type { NormalizedOutbound } from '../types.ts';
import {
  concatBytes,
  encodeBoolField,
  encodeBytesField,
  encodeMessageField,
  encodeRepeatedStringField,
  encodeStringField,
  encodeUInt32Field,
} from '../api/protobuf.ts';

function base64ToBytes(value: string | undefined): Uint8Array | undefined {
  if (!value) {
    return undefined;
  }
  return Uint8Array.from(Buffer.from(value, 'base64'));
}

function hexToBytes(value: string | undefined): Uint8Array | undefined {
  if (!value) {
    return undefined;
  }
  return Uint8Array.from(Buffer.from(value, 'hex'));
}

function encodeTypedMessage(type: string, value: Uint8Array): Uint8Array {
  return concatBytes([
    encodeStringField(1, type),
    encodeBytesField(2, value),
  ]);
}

function encodeIpOrDomain(domain: string): Uint8Array {
  return encodeStringField(2, domain);
}

function encodeVlessAccount(entry: NormalizedOutbound): Uint8Array {
  return concatBytes([
    encodeStringField(1, entry.uuid),
    encodeStringField(2, entry.flow),
    encodeStringField(3, entry.encryption),
  ]);
}

function encodeUser(entry: NormalizedOutbound): Uint8Array {
  return concatBytes([
    encodeUInt32Field(1, 0),
    encodeStringField(2, entry.tag),
    encodeMessageField(3, encodeTypedMessage('xray.proxy.vless.Account', encodeVlessAccount(entry))),
  ]);
}

function encodeServerEndpoint(entry: NormalizedOutbound): Uint8Array {
  return concatBytes([
    encodeMessageField(1, encodeIpOrDomain(entry.address)),
    encodeUInt32Field(2, entry.port),
    encodeMessageField(3, encodeUser(entry)),
  ]);
}

function encodeVlessOutboundConfig(entry: NormalizedOutbound): Uint8Array {
  return encodeMessageField(1, encodeServerEndpoint(entry));
}

function encodeTlsConfig(entry: NormalizedOutbound): Uint8Array {
  return concatBytes([
    encodeBoolField(1, entry.allowInsecure),
    encodeStringField(3, entry.sni),
    encodeRepeatedStringField(4, entry.alpn),
    encodeStringField(10, entry.fingerprint),
  ]);
}

function encodeRealityConfig(entry: NormalizedOutbound): Uint8Array {
  return concatBytes([
    encodeStringField(21, entry.fingerprint),
    encodeStringField(22, entry.sni),
    encodeBytesField(23, base64ToBytes(entry.publicKey)),
    encodeBytesField(24, hexToBytes(entry.shortId)),
  ]);
}

function encodeWsHeaderEntry(key: string, value: string): Uint8Array {
  return concatBytes([
    encodeStringField(1, key),
    encodeStringField(2, value),
  ]);
}

function encodeWsConfig(entry: NormalizedOutbound): Uint8Array {
  const headers = entry.host ? encodeMessageField(3, encodeWsHeaderEntry('Host', entry.host)) : new Uint8Array(0);
  return concatBytes([
    encodeStringField(1, entry.host),
    encodeStringField(2, entry.path ?? '/'),
    headers,
  ]);
}

function encodeTransportConfig(entry: NormalizedOutbound): Uint8Array[] {
  if (entry.network === 'ws') {
    return [
      concatBytes([
        encodeStringField(1, 'websocket'),
        encodeMessageField(
          2,
          encodeTypedMessage('xray.transport.internet.websocket.Config', encodeWsConfig(entry)),
        ),
      ]),
    ];
  }
  return [];
}

function mapGrpcProtocolName(entry: NormalizedOutbound): string {
  if (entry.network === 'ws') {
    return 'websocket';
  }
  return entry.network;
}

function encodeStreamConfig(entry: NormalizedOutbound): Uint8Array {
  const securityType =
    entry.security === 'tls'
      ? 'xray.transport.internet.tls.Config'
      : 'xray.transport.internet.reality.Config';
  const securitySettings =
    entry.security === 'tls'
      ? [encodeTypedMessage('xray.transport.internet.tls.Config', encodeTlsConfig(entry))]
      : [encodeTypedMessage('xray.transport.internet.reality.Config', encodeRealityConfig(entry))];

  return concatBytes([
    concatBytes(encodeTransportConfig(entry).map((item) => encodeMessageField(2, item))),
    encodeStringField(3, securityType),
    concatBytes(securitySettings.map((item) => encodeMessageField(4, item))),
    encodeStringField(5, mapGrpcProtocolName(entry)),
  ]);
}

function encodeSenderConfig(entry: NormalizedOutbound): Uint8Array {
  return encodeMessageField(2, encodeStreamConfig(entry));
}

export interface GrpcOutboundHandlerConfig {
  tag: string;
  raw: Uint8Array;
}

export function buildOutboundGrpc(entry: NormalizedOutbound): GrpcOutboundHandlerConfig {
  const raw = concatBytes([
    encodeStringField(1, entry.tag),
    encodeMessageField(
      2,
      encodeTypedMessage('xray.app.proxyman.SenderConfig', encodeSenderConfig(entry)),
    ),
    encodeMessageField(
      3,
      encodeTypedMessage('xray.proxy.vless.outbound.Config', encodeVlessOutboundConfig(entry)),
    ),
  ]);

  return {
    tag: entry.tag,
    raw,
  };
}

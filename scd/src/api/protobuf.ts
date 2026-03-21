import { Buffer } from 'node:buffer';

function concatBytes(parts: Uint8Array[]): Uint8Array {
  const size = parts.reduce((sum, part) => sum + part.length, 0);
  const out = new Uint8Array(size);
  let offset = 0;
  for (const part of parts) {
    out.set(part, offset);
    offset += part.length;
  }
  return out;
}

function encodeVarint(value: number | bigint): Uint8Array {
  let current = BigInt(value);
  const bytes: number[] = [];
  while (current >= 0x80n) {
    bytes.push(Number((current & 0x7fn) | 0x80n));
    current >>= 7n;
  }
  bytes.push(Number(current));
  return Uint8Array.from(bytes);
}

function encodeTag(fieldNumber: number, wireType: number): Uint8Array {
  return encodeVarint((fieldNumber << 3) | wireType);
}

export function encodeStringField(fieldNumber: number, value: string | undefined): Uint8Array {
  if (!value) {
    return new Uint8Array(0);
  }
  const bytes = new TextEncoder().encode(value);
  return concatBytes([encodeTag(fieldNumber, 2), encodeVarint(bytes.length), bytes]);
}

export function encodeBoolField(fieldNumber: number, value: boolean | undefined): Uint8Array {
  if (value === undefined) {
    return new Uint8Array(0);
  }
  return concatBytes([encodeTag(fieldNumber, 0), encodeVarint(value ? 1 : 0)]);
}

export function encodeUInt32Field(fieldNumber: number, value: number | undefined): Uint8Array {
  if (value === undefined || value === 0) {
    return new Uint8Array(0);
  }
  return concatBytes([encodeTag(fieldNumber, 0), encodeVarint(value)]);
}

export function encodeBytesField(fieldNumber: number, value: Uint8Array | undefined): Uint8Array {
  if (!value || value.length === 0) {
    return new Uint8Array(0);
  }
  return concatBytes([encodeTag(fieldNumber, 2), encodeVarint(value.length), value]);
}

export function encodeMessageField(fieldNumber: number, message: Uint8Array | undefined): Uint8Array {
  if (!message || message.length === 0) {
    return new Uint8Array(0);
  }
  return concatBytes([encodeTag(fieldNumber, 2), encodeVarint(message.length), message]);
}

export function encodeRepeatedStringField(fieldNumber: number, values: string[]): Uint8Array {
  return concatBytes(values.map((value) => encodeStringField(fieldNumber, value)));
}

export function encodeRepeatedBytesField(fieldNumber: number, values: Uint8Array[]): Uint8Array {
  return concatBytes(values.map((value) => encodeBytesField(fieldNumber, value)));
}

function decodeVarint(input: Uint8Array, start: number): { value: number; next: number } {
  let shift = 0;
  let result = 0n;
  let offset = start;
  while (offset < input.length) {
    const byte = input[offset];
    result |= BigInt(byte & 0x7f) << BigInt(shift);
    offset += 1;
    if ((byte & 0x80) === 0) {
      return { value: Number(result), next: offset };
    }
    shift += 7;
  }
  throw new Error('Unexpected EOF while decoding varint.');
}

function skipField(input: Uint8Array, offset: number, wireType: number): number {
  if (wireType === 0) {
    return decodeVarint(input, offset).next;
  }
  if (wireType === 2) {
    const length = decodeVarint(input, offset);
    return length.next + length.value;
  }
  if (wireType === 5) {
    return offset + 4;
  }
  if (wireType === 1) {
    return offset + 8;
  }
  throw new Error(`Unsupported wire type: ${wireType}`);
}

function decodeLengthDelimitedField(input: Uint8Array, offset: number): { value: Uint8Array; next: number } {
  const length = decodeVarint(input, offset);
  const start = length.next;
  const end = start + length.value;
  return {
    value: input.slice(start, end),
    next: end,
  };
}

function decodeBytesFields(raw: Uint8Array, field: number): Uint8Array[] {
  const values: Uint8Array[] = [];
  let offset = 0;
  while (offset < raw.length) {
    const header = decodeVarint(raw, offset);
    offset = header.next;
    const fieldNumber = header.value >>> 3;
    const wireType = header.value & 0x7;
    if (fieldNumber === field && wireType === 2) {
      const decoded = decodeLengthDelimitedField(raw, offset);
      values.push(decoded.value);
      offset = decoded.next;
      continue;
    }
    offset = skipField(raw, offset, wireType);
  }
  return values;
}

function decodeFirstBytesField(raw: Uint8Array, field: number): Uint8Array | undefined {
  return decodeBytesFields(raw, field)[0];
}

function decodeUInt32Fields(raw: Uint8Array, field: number): number[] {
  const values: number[] = [];
  let offset = 0;
  while (offset < raw.length) {
    const header = decodeVarint(raw, offset);
    offset = header.next;
    const fieldNumber = header.value >>> 3;
    const wireType = header.value & 0x7;
    if (fieldNumber === field && wireType === 0) {
      const decoded = decodeVarint(raw, offset);
      values.push(decoded.value);
      offset = decoded.next;
      continue;
    }
    offset = skipField(raw, offset, wireType);
  }
  return values;
}

function decodeFirstUInt32Field(raw: Uint8Array, field: number): number | undefined {
  return decodeUInt32Fields(raw, field)[0];
}

function decodeBoolField(raw: Uint8Array, field: number): boolean | undefined {
  const value = decodeFirstUInt32Field(raw, field);
  return value === undefined ? undefined : value !== 0;
}

function decodeRepeatedStringField(raw: Uint8Array, field: number): string[] {
  return decodeBytesFields(raw, field).map((item) => new TextDecoder().decode(item));
}

function decodeTypedMessage(raw: Uint8Array): { type: string; value: Uint8Array } | undefined {
  const type = decodeStringField(raw, 1);
  const value = decodeFirstBytesField(raw, 2);
  if (!type || !value) {
    return undefined;
  }
  return { type, value };
}

function decodeIpOrDomain(raw: Uint8Array): string | undefined {
  const ipv4 = decodeFirstBytesField(raw, 1);
  if (ipv4 && ipv4.length === 4) {
    return Array.from(ipv4).join('.');
  }

  const domain = decodeStringField(raw, 2);
  return domain || undefined;
}

export interface RawOutboundConfig {
  tag: string;
  raw: Uint8Array;
}

export interface RawInboundConfig {
  tag: string;
  raw: Uint8Array;
}

export interface RawRoutingRule {
  tag: string;
  ruleTag: string;
  inboundTags: string[];
  raw: Uint8Array;
}

function decodeTagFromHandlerConfig(raw: Uint8Array): string {
  let offset = 0;
  while (offset < raw.length) {
    const header = decodeVarint(raw, offset);
    offset = header.next;
    const fieldNumber = header.value >>> 3;
    const wireType = header.value & 0x7;
    if (fieldNumber === 1 && wireType === 2) {
      const length = decodeVarint(raw, offset);
      const start = length.next;
      const end = start + length.value;
      return new TextDecoder().decode(raw.subarray(start, end));
    }
    offset = skipField(raw, offset, wireType);
  }
  return '';
}

export function decodeListOutboundsResponse(bytes: Uint8Array): RawOutboundConfig[] {
  const outbounds: RawOutboundConfig[] = [];
  let offset = 0;
  while (offset < bytes.length) {
    const header = decodeVarint(bytes, offset);
    offset = header.next;
    const fieldNumber = header.value >>> 3;
    const wireType = header.value & 0x7;
    if (fieldNumber === 1 && wireType === 2) {
      const length = decodeVarint(bytes, offset);
      const start = length.next;
      const end = start + length.value;
      const raw = bytes.slice(start, end);
      outbounds.push({ tag: decodeTagFromHandlerConfig(raw), raw });
      offset = end;
      continue;
    }
    offset = skipField(bytes, offset, wireType);
  }
  return outbounds;
}

export function decodeListInboundsResponse(bytes: Uint8Array): RawInboundConfig[] {
  const inbounds: RawInboundConfig[] = [];
  let offset = 0;
  while (offset < bytes.length) {
    const header = decodeVarint(bytes, offset);
    offset = header.next;
    const fieldNumber = header.value >>> 3;
    const wireType = header.value & 0x7;
    if (fieldNumber === 1 && wireType === 2) {
      const length = decodeVarint(bytes, offset);
      const start = length.next;
      const end = start + length.value;
      const raw = bytes.slice(start, end);
      inbounds.push({ tag: decodeTagFromHandlerConfig(raw), raw });
      offset = end;
      continue;
    }
    offset = skipField(bytes, offset, wireType);
  }
  return inbounds;
}

function decodeStringField(raw: Uint8Array, field: number): string {
  let offset = 0;
  while (offset < raw.length) {
    const header = decodeVarint(raw, offset);
    offset = header.next;
    const fieldNumber = header.value >>> 3;
    const wireType = header.value & 0x7;
    if (fieldNumber === field && wireType === 2) {
      const length = decodeVarint(raw, offset);
      const start = length.next;
      const end = start + length.value;
      return new TextDecoder().decode(raw.subarray(start, end));
    }
    offset = skipField(raw, offset, wireType);
  }
  return '';
}

export function decodeListRulesResponse(bytes: Uint8Array): RawRoutingRule[] {
  const rules: RawRoutingRule[] = [];
  let offset = 0;
  while (offset < bytes.length) {
    const header = decodeVarint(bytes, offset);
    offset = header.next;
    const fieldNumber = header.value >>> 3;
    const wireType = header.value & 0x7;
    if (fieldNumber === 1 && wireType === 2) {
      const length = decodeVarint(bytes, offset);
      const start = length.next;
      const end = start + length.value;
      const raw = bytes.slice(start, end);
      rules.push({
        tag: decodeStringField(raw, 1),
        ruleTag: decodeStringField(raw, 2),
        inboundTags: decodeRepeatedStringField(raw, 8),
        raw,
      });
      offset = end;
      continue;
    }
    offset = skipField(bytes, offset, wireType);
  }
  return rules;
}

export function decodeGeneratedOutbound(raw: Uint8Array): import('../types.ts').ParsedRuntimeOutbound | undefined {
  const senderMessage = decodeTypedMessage(decodeFirstBytesField(raw, 2) ?? new Uint8Array(0));
  const outboundMessage = decodeTypedMessage(decodeFirstBytesField(raw, 3) ?? new Uint8Array(0));
  if (senderMessage?.type !== 'xray.app.proxyman.SenderConfig') {
    return undefined;
  }
  if (outboundMessage?.type !== 'xray.proxy.vless.outbound.Config') {
    return undefined;
  }

  const streamConfig = decodeFirstBytesField(senderMessage.value, 2);
  const serverEndpoint = decodeFirstBytesField(outboundMessage.value, 1);
  if (!streamConfig || !serverEndpoint) {
    return undefined;
  }

  const address = decodeIpOrDomain(decodeFirstBytesField(serverEndpoint, 1) ?? new Uint8Array(0));
  const port = decodeFirstUInt32Field(serverEndpoint, 2);
  const user = decodeFirstBytesField(serverEndpoint, 3);
  const accountMessage = user
    ? decodeTypedMessage(decodeFirstBytesField(user, 3) ?? new Uint8Array(0))
    : undefined;

  const parsed: import('../types.ts').ParsedRuntimeOutbound = {
    protocol: 'vless',
    address,
    port,
    uuid: accountMessage?.type === 'xray.proxy.vless.Account' ? decodeStringField(accountMessage.value, 1) : undefined,
    flow: accountMessage?.type === 'xray.proxy.vless.Account' ? decodeStringField(accountMessage.value, 2) || undefined : undefined,
    encryption: accountMessage?.type === 'xray.proxy.vless.Account' ? decodeStringField(accountMessage.value, 3) || undefined : undefined,
    network: decodeStringField(streamConfig, 5) || undefined,
    alpn: [],
  };

  const transportConfigs = decodeBytesFields(streamConfig, 2);
  for (const transportConfig of transportConfigs) {
    const protocol = decodeStringField(transportConfig, 1);
    const message = decodeTypedMessage(decodeFirstBytesField(transportConfig, 2) ?? new Uint8Array(0));
    if (protocol === 'websocket' && message?.type === 'xray.transport.internet.websocket.Config') {
      parsed.host = decodeStringField(message.value, 1) || undefined;
      parsed.path = decodeStringField(message.value, 2) || undefined;
    }
  }

  const securityType = decodeStringField(streamConfig, 3);
  const securityMessages = decodeBytesFields(streamConfig, 4).map((item) => decodeTypedMessage(item)).filter(Boolean);
  if (securityType === 'xray.transport.internet.tls.Config') {
    parsed.security = 'tls';
    const message = securityMessages.find((item) => item!.type === 'xray.transport.internet.tls.Config');
    if (message) {
      parsed.sni = decodeStringField(message.value, 3) || undefined;
      parsed.alpn = decodeRepeatedStringField(message.value, 4);
      parsed.fingerprint = decodeStringField(message.value, 10) || undefined;
    }
  } else if (securityType === 'xray.transport.internet.reality.Config') {
    parsed.security = 'reality';
    const message = securityMessages.find((item) => item!.type === 'xray.transport.internet.reality.Config');
    if (message) {
      parsed.fingerprint = decodeStringField(message.value, 21) || undefined;
      parsed.sni = decodeStringField(message.value, 22) || undefined;
      const publicKey = decodeFirstBytesField(message.value, 23);
      const shortId = decodeFirstBytesField(message.value, 24);
      parsed.publicKey = publicKey ? Buffer.from(publicKey).toString('base64') : undefined;
      parsed.shortId = shortId ? Buffer.from(shortId).toString('hex') : undefined;
    }
  }

  return parsed;
}

export function decodeGeneratedInbound(raw: Uint8Array): import('../types.ts').ParsedRuntimeInbound | undefined {
  const receiverMessage = decodeTypedMessage(decodeFirstBytesField(raw, 2) ?? new Uint8Array(0));
  const proxyMessage = decodeTypedMessage(decodeFirstBytesField(raw, 3) ?? new Uint8Array(0));
  if (receiverMessage?.type !== 'xray.app.proxyman.ReceiverConfig') {
    return undefined;
  }
  if (proxyMessage?.type !== 'xray.proxy.socks.ServerConfig') {
    return undefined;
  }

  const portList = decodeFirstBytesField(receiverMessage.value, 1);
  const portRange = portList ? decodeFirstBytesField(portList, 1) : undefined;
  const listen = decodeIpOrDomain(decodeFirstBytesField(receiverMessage.value, 2) ?? new Uint8Array(0));

  return {
    protocol: 'socks',
    listen,
    portStart: portRange ? decodeFirstUInt32Field(portRange, 1) : undefined,
    portEnd: portRange ? decodeFirstUInt32Field(portRange, 2) : undefined,
    udp: decodeBoolField(proxyMessage.value, 4),
  };
}

export function decodeGeneratedRoutingRule(raw: Uint8Array): import('../types.ts').ParsedRuntimeRoutingRule {
  return {
    ruleTag: decodeStringField(raw, 19) || decodeStringField(raw, 2),
    outboundTag: decodeStringField(raw, 1),
    inboundTags: decodeRepeatedStringField(raw, 8),
  };
}

export function encodeGrpcFrame(message: Uint8Array): Uint8Array {
  const frame = new Uint8Array(5 + message.length);
  frame[0] = 0;
  const view = new DataView(frame.buffer);
  view.setUint32(1, message.length, false);
  frame.set(message, 5);
  return frame;
}

export function decodeGrpcFrames(payload: Uint8Array): Uint8Array[] {
  const frames: Uint8Array[] = [];
  let offset = 0;
  while (offset + 5 <= payload.length) {
    const compressed = payload[offset];
    const length = new DataView(payload.buffer, payload.byteOffset + offset + 1, 4).getUint32(0, false);
    offset += 5;
    const end = offset + length;
    if (compressed !== 0) {
      throw new Error('Compressed gRPC frames are not supported.');
    }
    frames.push(payload.slice(offset, end));
    offset = end;
  }
  return frames;
}

export { concatBytes };

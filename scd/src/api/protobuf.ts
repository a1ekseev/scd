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

export interface RawOutboundConfig {
  tag: string;
  raw: Uint8Array;
}

function decodeTagFromOutbound(raw: Uint8Array): string {
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
      outbounds.push({ tag: decodeTagFromOutbound(raw), raw });
      offset = end;
      continue;
    }
    offset = skipField(bytes, offset, wireType);
  }
  return outbounds;
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

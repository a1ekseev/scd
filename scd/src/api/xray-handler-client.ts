import { Buffer } from 'node:buffer';
import { connect } from 'node:http2';

import { ApiRequestError } from '../errors.ts';
import {
  decodeGrpcFrames,
  decodeListInboundsResponse,
  decodeListOutboundsResponse,
  decodeListRulesResponse,
  encodeBoolField,
  encodeGrpcFrame,
  encodeMessageField,
  encodeStringField,
  type RawInboundConfig,
  type RawOutboundConfig,
  type RawRoutingRule,
} from './protobuf.ts';

interface XrayHandlerClientOptions {
  timeoutMs?: number;
}

async function unaryCall(authority: string, path: string, message: Uint8Array, timeoutMs = 5000): Promise<Uint8Array> {
  const url = new URL(authority.includes('://') ? authority : `http://${authority}`);
  const client = connect(url);
  const stream = client.request({
    ':method': 'POST',
    ':path': path,
    'content-type': 'application/grpc',
    te: 'trailers',
  });

  const chunks: Uint8Array[] = [];
  let grpcStatus = '0';
  let grpcMessage = '';

  return await new Promise((resolve, reject) => {
    let settled = false;
    let timeout: NodeJS.Timeout | undefined;

    const cleanup = () => {
      if (timeout) {
        clearTimeout(timeout);
      }
      if (!stream.closed) {
        stream.close();
      }
      client.close();
    };

    const normalizeConnectionError = (error: Error): Error => {
      const code = 'code' in error && typeof error.code === 'string' ? error.code : '';
      if (code === 'ECONNREFUSED' || code === 'EPERM') {
        return new ApiRequestError(
          `Unable to connect to Xray API at ${url.host}. Start the Xray service first, for example with "docker compose up -d".`,
          { cause: error },
        );
      }
      if (error.message.includes('ECONNREFUSED') || error.message.includes('connect EPERM')) {
        return new ApiRequestError(
          `Unable to connect to Xray API at ${url.host}. Start the Xray service first, for example with "docker compose up -d".`,
          { cause: error },
        );
      }
      return new ApiRequestError(error.message, { cause: error });
    };

    const fail = (error: Error) => {
      if (settled) {
        return;
      }
      settled = true;
      cleanup();
      reject(normalizeConnectionError(error));
    };

    timeout = setTimeout(() => {
      fail(new ApiRequestError(`Request to Xray API at ${url.host} timed out after ${timeoutMs}ms.`));
    }, timeoutMs);

    stream.on('response', (headers) => {
      if (headers['grpc-status']) {
        grpcStatus = String(headers['grpc-status']);
      }
      if (headers['grpc-message']) {
        grpcMessage = String(headers['grpc-message']);
      }
    });
    stream.on('trailers', (headers) => {
      if (headers['grpc-status']) {
        grpcStatus = String(headers['grpc-status']);
      }
      if (headers['grpc-message']) {
        grpcMessage = decodeURIComponent(String(headers['grpc-message']));
      }
    });
    stream.on('data', (chunk: string | Buffer) => {
      chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
    });
    client.on('error', (error) => {
      fail(error);
    });
    stream.on('error', (error) => {
      fail(error);
    });
    stream.on('end', () => {
      if (settled) {
        return;
      }
      if (grpcStatus !== '0') {
        fail(new Error(grpcMessage || `gRPC call failed with status ${grpcStatus}`));
        return;
      }
      const payload = Buffer.concat(chunks.map((chunk) => Buffer.from(chunk)));
      const frames = decodeGrpcFrames(new Uint8Array(payload));
      settled = true;
      cleanup();
      resolve(frames[0] ?? new Uint8Array(0));
    });
    stream.end(Buffer.from(encodeGrpcFrame(message)));
  });
}

export class XrayHandlerClient {
  private readonly authority: string;
  private readonly timeoutMs: number;

  constructor(authority: string, options: XrayHandlerClientOptions = {}) {
    this.authority = authority;
    this.timeoutMs = options.timeoutMs ?? 5000;
  }

  async listOutbounds(): Promise<RawOutboundConfig[]> {
    const payload = await unaryCall(
      this.authority,
      '/xray.app.proxyman.command.HandlerService/ListOutbounds',
      new Uint8Array(0),
      this.timeoutMs,
    );
    return decodeListOutboundsResponse(payload);
  }

  async listInbounds(): Promise<RawInboundConfig[]> {
    const payload = await unaryCall(
      this.authority,
      '/xray.app.proxyman.command.HandlerService/ListInbounds',
      new Uint8Array(0),
      this.timeoutMs,
    );
    return decodeListInboundsResponse(payload);
  }

  async removeOutbound(tag: string): Promise<void> {
    await unaryCall(
      this.authority,
      '/xray.app.proxyman.command.HandlerService/RemoveOutbound',
      encodeStringField(1, tag),
      this.timeoutMs,
    );
  }

  async addOutbound(rawOutbound: Uint8Array): Promise<void> {
    await unaryCall(
      this.authority,
      '/xray.app.proxyman.command.HandlerService/AddOutbound',
      encodeMessageField(1, rawOutbound),
      this.timeoutMs,
    );
  }

  async removeInbound(tag: string): Promise<void> {
    await unaryCall(
      this.authority,
      '/xray.app.proxyman.command.HandlerService/RemoveInbound',
      encodeStringField(1, tag),
      this.timeoutMs,
    );
  }

  async addInbound(rawInbound: Uint8Array): Promise<void> {
    await unaryCall(
      this.authority,
      '/xray.app.proxyman.command.HandlerService/AddInbound',
      encodeMessageField(1, rawInbound),
      this.timeoutMs,
    );
  }

  async listRules(): Promise<RawRoutingRule[]> {
    const payload = await unaryCall(
      this.authority,
      '/xray.app.router.command.RoutingService/ListRule',
      new Uint8Array(0),
      this.timeoutMs,
    );
    return decodeListRulesResponse(payload);
  }

  async removeRule(ruleTag: string): Promise<void> {
    await unaryCall(
      this.authority,
      '/xray.app.router.command.RoutingService/RemoveRule',
      encodeStringField(1, ruleTag),
      this.timeoutMs,
    );
  }

  async addRule(rawRule: Uint8Array): Promise<void> {
    await unaryCall(
      this.authority,
      '/xray.app.router.command.RoutingService/AddRule',
      new Uint8Array([
        ...encodeMessageField(1, rawRule),
        ...encodeBoolField(2, true),
      ]),
      this.timeoutMs,
    );
  }
}

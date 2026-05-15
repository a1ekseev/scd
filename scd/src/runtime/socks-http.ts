import { Buffer } from 'node:buffer';
import { isIP } from 'node:net';
import * as net from 'node:net';
import { performance } from 'node:perf_hooks';
import * as tls from 'node:tls';

import type { MonitorHttpMethod } from '../types.ts';

interface SocksHttpRequestOptions {
  proxyHost: string;
  proxyPort: number;
  url: string;
  method: MonitorHttpMethod;
  timeoutMs: number;
}

interface SocksHttpDependencies {
  createConnection?: (options: net.NetConnectOpts) => net.Socket;
  tlsConnect?: (options: tls.ConnectionOptions) => tls.TLSSocket;
  nowFn?: () => number;
}

export interface SocksHttpResponse {
  statusCode: number;
  latencyMs: number;
  bodyBytes: number;
}

class BufferedReader {
  private buffer = Buffer.alloc(0);
  private readonly pending: Array<{ size: number; resolve: (buffer: Buffer) => void; reject: (error: Error) => void }> = [];
  private ended = false;
  private error?: Error;
  private readonly onData: (chunk: Buffer) => void;
  private readonly onEnd: () => void;
  private readonly onError: (error: Error) => void;
  private readonly onTimeout: () => void;
  private readonly socket: net.Socket;

  constructor(socket: net.Socket, timeoutMs: number) {
    this.socket = socket;
    this.onData = (chunk: Buffer) => {
      this.buffer = Buffer.concat([this.buffer, chunk]);
      this.flush();
    };
    this.onEnd = () => {
      this.ended = true;
      this.flush();
    };
    this.onError = (error) => {
      this.error = error;
      this.flush();
    };
    this.onTimeout = () => {
      this.error = new Error(`SOCKS5 handshake timed out after ${timeoutMs}ms.`);
      socket.destroy();
      this.flush();
    };

    socket.on('data', this.onData);
    socket.on('end', this.onEnd);
    socket.on('error', this.onError);
    socket.on('timeout', this.onTimeout);
  }

  async readExactly(size: number): Promise<Buffer> {
    if (this.error) {
      throw this.error;
    }
    if (this.buffer.length >= size) {
      const chunk = this.buffer.subarray(0, size);
      this.buffer = this.buffer.subarray(size);
      return chunk;
    }
    if (this.ended) {
      throw new Error(`Unexpected EOF while reading ${size} bytes from SOCKS proxy.`);
    }

    return await new Promise<Buffer>((resolve, reject) => {
      this.pending.push({ size, resolve, reject });
    });
  }

  dispose(): void {
    this.socket.off('data', this.onData);
    this.socket.off('end', this.onEnd);
    this.socket.off('error', this.onError);
    this.socket.off('timeout', this.onTimeout);
  }

  private flush(): void {
    while (this.pending.length > 0) {
      const current = this.pending[0];
      if (this.error) {
        current.reject(this.error);
        this.pending.shift();
        continue;
      }
      if (this.buffer.length >= current.size) {
        const chunk = this.buffer.subarray(0, current.size);
        this.buffer = this.buffer.subarray(current.size);
        current.resolve(chunk);
        this.pending.shift();
        continue;
      }
      if (this.ended) {
        current.reject(new Error('Unexpected EOF from SOCKS proxy.'));
        this.pending.shift();
        continue;
      }
      break;
    }
  }
}

function parseProxyTarget(urlValue: string): URL {
  return new URL(urlValue);
}

function buildSocksConnectRequest(hostname: string, port: number): Buffer {
  const version = isIP(hostname);
  if (version === 4) {
    return Buffer.from([
      0x05,
      0x01,
      0x00,
      0x01,
      ...hostname.split('.').map((octet) => Number(octet)),
      (port >> 8) & 0xff,
      port & 0xff,
    ]);
  }

  const hostBytes = Buffer.from(hostname, 'utf8');
  return Buffer.concat([
    Buffer.from([0x05, 0x01, 0x00, 0x03, hostBytes.length]),
    hostBytes,
    Buffer.from([(port >> 8) & 0xff, port & 0xff]),
  ]);
}

async function waitForEventWithTimeout(
  stream: net.Socket | tls.TLSSocket,
  eventName: string,
  timeoutMs: number,
  timeoutMessage: string,
): Promise<void> {
  stream.setTimeout(timeoutMs);

  await new Promise<void>((resolve, reject) => {
    let settled = false;

    const cleanup = () => {
      stream.off(eventName, onEvent);
      stream.off('error', onError);
      stream.off('timeout', onTimeout);
    };

    const settle = (callback: () => void) => {
      if (settled) {
        return;
      }
      settled = true;
      cleanup();
      callback();
    };

    const onEvent = () => {
      settle(resolve);
    };

    const onError = (error: Error) => {
      settle(() => reject(error));
    };

    const onTimeout = () => {
      const error = new Error(timeoutMessage);
      settle(() => {
        stream.destroy();
        reject(error);
      });
    };

    stream.once(eventName, onEvent);
    stream.once('error', onError);
    stream.once('timeout', onTimeout);
  });
}

async function connectViaSocks(
  proxyHost: string,
  proxyPort: number,
  hostname: string,
  port: number,
  timeoutMs: number,
  createConnection: NonNullable<SocksHttpDependencies['createConnection']>,
): Promise<net.Socket> {
  const socket = createConnection({
    host: proxyHost,
    port: proxyPort,
  });
  await waitForEventWithTimeout(
    socket,
    'connect',
    timeoutMs,
    `SOCKS5 proxy connection timed out after ${timeoutMs}ms.`,
  );

  const reader = new BufferedReader(socket, timeoutMs);
  try {
    socket.write(Buffer.from([0x05, 0x01, 0x00]));
    const greeting = await reader.readExactly(2);
    if (greeting[0] !== 0x05 || greeting[1] !== 0x00) {
      socket.destroy();
      throw new Error('SOCKS5 proxy does not accept no-auth authentication.');
    }

    socket.write(buildSocksConnectRequest(hostname, port));
    const replyHead = await reader.readExactly(4);
    if (replyHead[0] !== 0x05 || replyHead[1] !== 0x00) {
      socket.destroy();
      throw new Error(`SOCKS5 connect failed with code ${replyHead[1]}.`);
    }

    if (replyHead[3] === 0x01) {
      await reader.readExactly(4 + 2);
    } else if (replyHead[3] === 0x03) {
      const domainLength = await reader.readExactly(1);
      await reader.readExactly(domainLength[0] + 2);
    } else if (replyHead[3] === 0x04) {
      await reader.readExactly(16 + 2);
    }
  } finally {
    reader.dispose();
  }

  return socket;
}

export async function collectResponseHead(stream: net.Socket | tls.TLSSocket, timeoutMs: number): Promise<Buffer> {
  const chunks: Buffer[] = [];
  let totalLength = 0;
  stream.setTimeout(timeoutMs);

  return await new Promise<Buffer>((resolve, reject) => {
    let settled = false;

    const cleanup = () => {
      stream.off('data', onData);
      stream.off('end', onEnd);
      stream.off('error', onError);
      stream.off('timeout', onTimeout);
    };

    const settle = (callback: () => void) => {
      if (settled) {
        return;
      }
      settled = true;
      cleanup();
      callback();
    };

    const onData = (chunk: Buffer) => {
      chunks.push(chunk);
      totalLength += chunk.length;
      const buffered = Buffer.concat(chunks, totalLength);
      if (buffered.indexOf('\r\n\r\n') >= 0) {
        settle(() => resolve(buffered));
      }
    };

    const onEnd = () => {
      settle(() => resolve(Buffer.concat(chunks, totalLength)));
    };

    const onError = (error: Error) => {
      settle(() => reject(error));
    };

    const onTimeout = () => {
      settle(() => {
        stream.destroy();
        reject(new Error(`SOCKS HTTP request timed out after ${timeoutMs}ms.`));
      });
    };

    stream.on('data', onData);
    stream.on('end', onEnd);
    stream.on('error', onError);
    stream.on('timeout', onTimeout);
  });
}

function parseHttpResponse(response: Buffer): { statusCode: number; bodyBytes: number } {
  const separator = response.indexOf('\r\n\r\n');
  if (separator < 0) {
    throw new Error('Invalid HTTP response from monitored endpoint.');
  }

  const headers = response.subarray(0, separator).toString('utf8');
  const [statusLine] = headers.split('\r\n');
  const match = statusLine?.match(/^HTTP\/1\.[01]\s+(\d{3})/);
  if (!match) {
    throw new Error(`Unable to parse HTTP status line: ${statusLine ?? '<empty>'}`);
  }

  return {
    statusCode: Number(match[1]),
    bodyBytes: response.length - separator - 4,
  };
}

export async function requestViaSocks(
  options: SocksHttpRequestOptions,
  dependencies: SocksHttpDependencies = {},
): Promise<SocksHttpResponse> {
  const url = parseProxyTarget(options.url);
  const port = url.port ? Number(url.port) : url.protocol === 'https:' ? 443 : 80;
  const path = `${url.pathname || '/'}${url.search}`;
  const nowFn = dependencies.nowFn ?? (() => performance.now());
  const createConnection = dependencies.createConnection ?? net.createConnection;
  const tlsConnect = dependencies.tlsConnect ?? tls.connect;
  const rawSocket = await connectViaSocks(
    options.proxyHost,
    options.proxyPort,
    url.hostname,
    port,
    options.timeoutMs,
    createConnection,
  );

  let stream: net.Socket | tls.TLSSocket = rawSocket;
  if (url.protocol === 'https:') {
    stream = tlsConnect({
      socket: rawSocket,
      servername: url.hostname,
    });
    await waitForEventWithTimeout(
      stream,
      'secureConnect',
      options.timeoutMs,
      `SOCKS HTTPS TLS handshake timed out after ${options.timeoutMs}ms.`,
    );
  }

  const body = options.method === 'POST' ? '' : undefined;
  const request = [
    `${options.method} ${path} HTTP/1.1`,
    `Host: ${url.host}`,
    'Connection: close',
    ...(body !== undefined ? [`Content-Length: ${Buffer.byteLength(body)}`] : []),
    '',
    body ?? '',
  ].join('\r\n');

  const responsePromise = collectResponseHead(stream, options.timeoutMs);
  const requestStartedAt = nowFn();
  let response: Buffer;
  try {
    stream.write(request);
    response = await responsePromise;
  } finally {
    stream.destroy();
  }

  const parsed = parseHttpResponse(response);
  return {
    statusCode: parsed.statusCode,
    bodyBytes: parsed.bodyBytes,
    latencyMs: Math.round(nowFn() - requestStartedAt),
  };
}

import { Buffer } from 'node:buffer';
import { once } from 'node:events';
import { isIP } from 'node:net';
import * as net from 'node:net';
import * as tls from 'node:tls';

import type { MonitorHttpMethod } from '../types.ts';

interface SocksHttpRequestOptions {
  proxyHost: string;
  proxyPort: number;
  url: string;
  method: MonitorHttpMethod;
  timeoutMs: number;
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

  constructor(socket: net.Socket) {
    socket.on('data', (chunk: Buffer) => {
      this.buffer = Buffer.concat([this.buffer, chunk]);
      this.flush();
    });
    socket.on('end', () => {
      this.ended = true;
      this.flush();
    });
    socket.on('error', (error) => {
      this.error = error;
      this.flush();
    });
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

async function connectViaSocks(
  proxyHost: string,
  proxyPort: number,
  hostname: string,
  port: number,
  timeoutMs: number,
): Promise<net.Socket> {
  const socket = net.createConnection({
    host: proxyHost,
    port: proxyPort,
  });
  socket.setTimeout(timeoutMs);
  await once(socket, 'connect');

  const reader = new BufferedReader(socket);
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

  return socket;
}

async function collectResponse(stream: net.Socket | tls.TLSSocket, timeoutMs: number): Promise<Buffer> {
  const chunks: Buffer[] = [];
  stream.setTimeout(timeoutMs);

  return await new Promise<Buffer>((resolve, reject) => {
    stream.on('data', (chunk: Buffer) => {
      chunks.push(chunk);
    });
    stream.on('end', () => {
      resolve(Buffer.concat(chunks));
    });
    stream.on('error', (error) => {
      reject(error);
    });
    stream.on('timeout', () => {
      reject(new Error(`SOCKS HTTP request timed out after ${timeoutMs}ms.`));
    });
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

export async function requestViaSocks(options: SocksHttpRequestOptions): Promise<SocksHttpResponse> {
  const url = parseProxyTarget(options.url);
  const port = url.port ? Number(url.port) : url.protocol === 'https:' ? 443 : 80;
  const path = `${url.pathname || '/'}${url.search}`;
  const startedAt = Date.now();
  const rawSocket = await connectViaSocks(options.proxyHost, options.proxyPort, url.hostname, port, options.timeoutMs);

  let stream: net.Socket | tls.TLSSocket = rawSocket;
  if (url.protocol === 'https:') {
    stream = tls.connect({
      socket: rawSocket,
      servername: url.hostname,
    });
    stream.setTimeout(options.timeoutMs);
    await once(stream, 'secureConnect');
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

  stream.write(request);
  const response = await collectResponse(stream, options.timeoutMs);
  stream.destroy();

  const parsed = parseHttpResponse(response);
  return {
    statusCode: parsed.statusCode,
    bodyBytes: parsed.bodyBytes,
    latencyMs: Date.now() - startedAt,
  };
}

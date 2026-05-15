import assert from 'node:assert/strict';
import { EventEmitter } from 'node:events';
import { PassThrough } from 'node:stream';
import test from 'node:test';

import { collectResponseHead, requestViaSocks } from '../src/runtime/socks-http.ts';

class TestResponseStream extends PassThrough {
  destroyedByTimeout = false;

  setTimeout(_timeoutMs: number): this {
    return this;
  }

  destroy(error?: Error): this {
    this.destroyedByTimeout = true;
    return super.destroy(error);
  }
}

class FakeSocket extends EventEmitter {
  writes: Buffer[] = [];
  destroyedByRequest = false;
  autoTimeoutOnSet = false;
  onWrite?: (chunk: Buffer) => void;

  setTimeout(_timeoutMs: number): this {
    if (this.autoTimeoutOnSet) {
      setImmediate(() => {
        this.emit('timeout');
      });
    }
    return this;
  }

  write(chunk: string | Uint8Array): boolean {
    const buffer = Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk);
    this.writes.push(buffer);
    this.onWrite?.(buffer);
    return true;
  }

  destroy(): this {
    this.destroyedByRequest = true;
    this.emit('close');
    return this;
  }
}

function createSuccessfulSocksSocket(response: string): FakeSocket {
  const socket = new FakeSocket();
  let writeCount = 0;
  socket.onWrite = () => {
    writeCount += 1;
    if (writeCount === 1) {
      socket.emit('data', Buffer.from([0x05, 0x00]));
      return;
    }
    if (writeCount === 2) {
      socket.emit('data', Buffer.from([0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 0]));
      return;
    }
    socket.emit('data', Buffer.from(response));
  };
  return socket;
}

test('collectResponseHead resolves when response headers arrive, not when connection closes', async () => {
  const stream = new TestResponseStream();
  let responseEnded = false;
  const closeTimer = setTimeout(() => {
    responseEnded = true;
    stream.end();
  }, 350);

  try {
    const startedAt = Date.now();
    const responsePromise = collectResponseHead(stream as never, 1000);
    stream.write(Buffer.from('HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n'));
    const response = await responsePromise;
    const elapsedMs = Date.now() - startedAt;

    assert.equal(responseEnded, false);
    assert.match(response.toString('utf8'), /^HTTP\/1\.1 204/);
    assert.ok(elapsedMs < 250, `expected response head to resolve before delayed close, got ${elapsedMs}ms`);
  } finally {
    clearTimeout(closeTimer);
    stream.destroy();
  }
});

test('collectResponseHead rejects, destroys stream and removes listeners on timeout', async () => {
  const stream = new TestResponseStream();
  const responsePromise = collectResponseHead(stream as never, 1000);

  stream.emit('timeout');

  await assert.rejects(responsePromise, /SOCKS HTTP request timed out after 1000ms/);
  assert.equal(stream.destroyedByTimeout, true);
  assert.equal(stream.listenerCount('data'), 0);
  assert.equal(stream.listenerCount('end'), 0);
  assert.equal(stream.listenerCount('error'), 0);
  assert.equal(stream.listenerCount('timeout'), 0);
});

test('requestViaSocks attaches response listener before writing request', async () => {
  const socket = createSuccessfulSocksSocket('HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n');
  process.nextTick(() => {
    socket.emit('connect');
  });
  let now = 100;

  const response = await requestViaSocks(
    {
      proxyHost: '127.0.0.1',
      proxyPort: 1080,
      url: 'http://example.test/health',
      method: 'GET',
      timeoutMs: 1000,
    },
    {
      createConnection: () => socket as never,
      nowFn: () => {
        now += 23;
        return now;
      },
    },
  );

  assert.equal(response.statusCode, 204);
  assert.equal(response.latencyMs, 23);
  assert.equal(socket.destroyedByRequest, true);
  assert.match(socket.writes[2]?.toString('utf8') ?? '', /^GET \/health HTTP\/1\.1/);
  assert.equal(socket.listenerCount('data'), 0);
  assert.equal(socket.listenerCount('end'), 0);
  assert.equal(socket.listenerCount('error'), 0);
  assert.equal(socket.listenerCount('timeout'), 0);
});

test('requestViaSocks latency excludes SOCKS handshake setup time', async () => {
  const socket = new FakeSocket();
  let writeCount = 0;
  let currentTime = 100;

  socket.onWrite = () => {
    writeCount += 1;
    if (writeCount === 1) {
      currentTime += 500;
      socket.emit('data', Buffer.from([0x05, 0x00]));
      return;
    }
    if (writeCount === 2) {
      currentTime += 500;
      socket.emit('data', Buffer.from([0x05, 0x00, 0x00, 0x01, 127, 0, 0, 1, 0, 0]));
      return;
    }

    currentTime += 7;
    socket.emit('data', Buffer.from('HTTP/1.1 204 No Content\r\nConnection: close\r\n\r\n'));
  };
  process.nextTick(() => {
    socket.emit('connect');
  });

  const response = await requestViaSocks(
    {
      proxyHost: '127.0.0.1',
      proxyPort: 1080,
      url: 'http://example.test/health',
      method: 'GET',
      timeoutMs: 1000,
    },
    {
      createConnection: () => socket as never,
      nowFn: () => currentTime,
    },
  );

  assert.equal(response.statusCode, 204);
  assert.equal(response.latencyMs, 7);
});

test('requestViaSocks rejects and destroys socket on TCP connect timeout', async () => {
  const socket = new FakeSocket();
  socket.autoTimeoutOnSet = true;
  const responsePromise = requestViaSocks(
    {
      proxyHost: '127.0.0.1',
      proxyPort: 1080,
      url: 'http://example.test/health',
      method: 'GET',
      timeoutMs: 1000,
    },
    {
      createConnection: () => socket as never,
    },
  );

  await assert.rejects(responsePromise, /SOCKS5 proxy connection timed out after 1000ms/);
  assert.equal(socket.destroyedByRequest, true);
});

test('requestViaSocks rejects and destroys socket on SOCKS handshake timeout', async () => {
  const socket = new FakeSocket();
  socket.onWrite = () => {
    setImmediate(() => {
      socket.emit('timeout');
    });
  };
  process.nextTick(() => {
    socket.emit('connect');
  });
  const responsePromise = requestViaSocks(
    {
      proxyHost: '127.0.0.1',
      proxyPort: 1080,
      url: 'http://example.test/health',
      method: 'GET',
      timeoutMs: 1000,
    },
    {
      createConnection: () => socket as never,
    },
  );

  await assert.rejects(responsePromise, /SOCKS5 handshake timed out after 1000ms/);
  assert.equal(socket.destroyedByRequest, true);
  assert.equal(socket.listenerCount('data'), 0);
  assert.equal(socket.listenerCount('end'), 0);
  assert.equal(socket.listenerCount('error'), 0);
  assert.equal(socket.listenerCount('timeout'), 0);
});

test('requestViaSocks rejects and destroys TLS stream on secureConnect timeout', async () => {
  const rawSocket = createSuccessfulSocksSocket('');
  const tlsSocket = new FakeSocket();
  tlsSocket.autoTimeoutOnSet = true;
  process.nextTick(() => {
    rawSocket.emit('connect');
  });
  const responsePromise = requestViaSocks(
    {
      proxyHost: '127.0.0.1',
      proxyPort: 1080,
      url: 'https://example.test/health',
      method: 'GET',
      timeoutMs: 1000,
    },
    {
      createConnection: () => rawSocket as never,
      tlsConnect: () => tlsSocket as never,
    },
  );

  await assert.rejects(responsePromise, /SOCKS HTTPS TLS handshake timed out after 1000ms/);
  assert.equal(tlsSocket.destroyedByRequest, true);
});

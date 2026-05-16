import { Writable } from "node:stream";
import test from "node:test";
import assert from "node:assert/strict";

import { buildResponse, fillDeterministicPayload, writeRepeatedPayload } from "../src/runtime/server.ts";
import type { AppConfig } from "../src/types.ts";

const config: AppConfig = {
  server: { listen: "127.0.0.1:8080" },
  load: { path: "/load", maxSizeKb: 64 },
  logging: { level: "silent", format: "json" }
};

test("/healthz returns ok", () => {
  const response = buildResponse("/healthz", config);
  assert.equal(response.statusCode, 200);
  assert.equal(response.errorBody, "ok");
  assert.equal(response.headers["content-length"], "2");
});

test("/load/:sizeKb returns 200 and exact body size", () => {
  const response = buildResponse("/load/32", config);
  assert.equal(response.statusCode, 200);
  assert.equal(response.bodyBytes, 32768);
  assert.equal(response.headers["content-type"], "application/octet-stream");
  assert.equal(response.headers["content-length"], "32768");
  assert.equal(response.headers["x-gtl-status-code"], "200");
  assert.equal(response.headers["x-gtl-requested-size-kb"], "32");
  assert.equal(response.headers["x-gtl-requested-size-bytes"], "32768");
  assert.equal(response.headers["x-gtl-actual-size-bytes"], "32768");
});

test("custom load.path routes load requests", () => {
  const response = buildResponse("/download/32", {
    ...config,
    load: { path: "/download", maxSizeKb: 64 }
  });
  assert.equal(response.statusCode, 200);
  assert.equal(response.bodyBytes, 32768);
});

test("old /load path is not an alias when load.path differs", () => {
  const response = buildResponse("/load/32", {
    ...config,
    load: { path: "/download", maxSizeKb: 64 }
  });
  assert.equal(response.statusCode, 404);
});

test("/load/0 returns an empty 200 response", () => {
  const response = buildResponse("/load/0", config);
  assert.equal(response.statusCode, 200);
  assert.equal(response.bodyBytes, 0);
  assert.equal(response.headers["content-length"], "0");
});

test("invalid load route under configured path returns 400", () => {
  const response = buildResponse("/download/200_32", {
    ...config,
    load: { path: "/download", maxSizeKb: 64 }
  });
  assert.equal(response.statusCode, 400);
  assert.match(response.errorBody ?? "", /expected \/download\/:sizeKb/);
});

test("size above maxSizeKb returns 400", () => {
  const response = buildResponse("/load/65", config);
  assert.equal(response.statusCode, 400);
  assert.match(response.errorBody ?? "", /0 to 64/);
});

test("unknown route returns 404", () => {
  const response = buildResponse("/unknown", config);
  assert.equal(response.statusCode, 404);
});

test("writeRepeatedPayload streams exact deterministic non-trivial bytes", async () => {
  const writable = new CaptureResponse();
  await writeRepeatedPayload(writable, 96 * 1024 + 7);
  assert.equal(writable.bytes.length, 96 * 1024 + 7);
  assert.notEqual(writable.bytes.toString("utf8", 0, 8), "aaaaaaaa");
  assert.ok(new Set(writable.bytes.subarray(0, 256)).size > 128);
  assert.equal(writable.ended, true);
});

test("fillDeterministicPayload depends on absolute offset", () => {
  const first = Buffer.alloc(64);
  const second = Buffer.alloc(64);
  fillDeterministicPayload(first, 0);
  fillDeterministicPayload(second, 64);

  assert.notDeepEqual(first, second);
});

class CaptureResponse extends Writable {
  public chunks: Buffer[] = [];
  public ended = false;

  get bytes(): Buffer {
    return Buffer.concat(this.chunks);
  }

  override _write(chunk: Buffer, _encoding: BufferEncoding, callback: (error?: Error | null) => void): void {
    this.chunks.push(Buffer.from(chunk));
    callback();
  }

  override end(callback?: () => void): this;
  override end(chunk: unknown, callback?: () => void): this;
  override end(chunk: unknown, encoding?: BufferEncoding, callback?: () => void): this;
  override end(
    chunkOrCallback?: unknown,
    encodingOrCallback?: BufferEncoding | (() => void),
    maybeCallback?: () => void
  ): this {
    this.ended = true;
    return super.end(chunkOrCallback as never, encodingOrCallback as never, maybeCallback);
  }
}

import { once } from "node:events";
import { createServer, type IncomingMessage, type Server, type ServerResponse } from "node:http";

import { parseListenAddress } from "../config/load-config.ts";
import type { AppConfig } from "../types.ts";

const CHUNK_SIZE_BYTES = 64 * 1024;

export interface BuiltResponse {
  statusCode: number;
  headers: Record<string, string>;
  bodyBytes: number;
  errorBody?: string;
}

export function buildResponse(path: string, config: AppConfig): BuiltResponse {
  if (path === "/healthz") {
    return {
      statusCode: 200,
      headers: {
        "content-type": "text/plain; charset=utf-8",
        "content-length": "2"
      },
      bodyBytes: 2,
      errorBody: "ok"
    };
  }

  const routePrefix = `${config.load.path}/`;
  if (!path.startsWith(routePrefix)) {
    return textResponse(404, "not found\n");
  }

  const sizePart = path.slice(routePrefix.length);
  if (!/^\d+$/.test(sizePart)) {
    return textResponse(400, `invalid load route, expected ${config.load.path}/:sizeKb\n`);
  }

  const sizeKb = Number(sizePart);
  if (!Number.isSafeInteger(sizeKb) || sizeKb < 0 || sizeKb > config.load.maxSizeKb) {
    return textResponse(400, `sizeKb must be an integer from 0 to ${config.load.maxSizeKb}\n`);
  }

  const sizeBytes = sizeKb * 1024;
  if (!Number.isSafeInteger(sizeBytes)) {
    return textResponse(400, "requested size is too large\n");
  }

  return {
    statusCode: 200,
    headers: {
      "content-type": "application/octet-stream",
      "content-length": String(sizeBytes),
      "x-gtl-status-code": "200",
      "x-gtl-requested-size-kb": String(sizeKb),
      "x-gtl-requested-size-bytes": String(sizeBytes),
      "x-gtl-actual-size-bytes": String(sizeBytes)
    },
    bodyBytes: sizeBytes
  };
}

export async function writeResponse(response: ServerResponse, built: BuiltResponse): Promise<void> {
  response.writeHead(built.statusCode, built.headers);

  if (built.errorBody !== undefined) {
    response.end(built.errorBody);
    return;
  }

  await writeRepeatedPayload(response, built.bodyBytes);
}

export async function writeRepeatedPayload(response: NodeJS.WritableStream, totalBytes: number): Promise<void> {
  const chunk = Buffer.alloc(Math.min(CHUNK_SIZE_BYTES, Math.max(totalBytes, 1)), "a");
  let remaining = totalBytes;

  while (remaining > 0) {
    const current = remaining >= chunk.length ? chunk : chunk.subarray(0, remaining);
    if (!response.write(current)) {
      await once(response, "drain");
    }
    remaining -= current.length;
  }

  response.end();
}

export function startServer(config: AppConfig): Server {
  const server = createServer((request: IncomingMessage, response: ServerResponse) => {
    void handleRequest(request, response, config);
  });
  const { host, port } = parseListenAddress(config.server.listen);
  server.listen(port, host);
  return server;
}

async function handleRequest(request: IncomingMessage, response: ServerResponse, config: AppConfig): Promise<void> {
  if (request.method !== "GET") {
    await writeResponse(response, textResponse(405, "method not allowed\n"));
    return;
  }

  const url = new URL(request.url ?? "/", "http://127.0.0.1");
  await writeResponse(response, buildResponse(url.pathname, config));
}

function textResponse(statusCode: number, body: string): BuiltResponse {
  return {
    statusCode,
    headers: {
      "content-type": "text/plain; charset=utf-8",
      "content-length": String(Buffer.byteLength(body))
    },
    bodyBytes: Buffer.byteLength(body),
    errorBody: body
  };
}

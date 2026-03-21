import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';

import type { AppState, LoadedConfig } from '../types.ts';
import type { Logger } from '../logging/create-logger.ts';

export interface ResponsePayload {
  statusCode: number;
  contentType: string;
  body: string;
}

function text(statusCode: number, body: string): ResponsePayload {
  return {
    statusCode,
    contentType: 'text/plain; charset=utf-8',
    body,
  };
}

export function handleServerRequest(urlPath: string, state: AppState): ResponsePayload {
  if (urlPath === '/healthz') {
    return text(200, 'ok\n');
  }

  const match = /^\/s\/([^/?#]+)$/.exec(urlPath);
  if (!match) {
    return text(404, 'not found\n');
  }

  const id = decodeURIComponent(match[1] ?? '');
  const output = state.outputs[id];
  if (!output) {
    return text(404, 'unknown output id\n');
  }

  if (!output.lastGoodBase64) {
    return text(502, 'output is not available yet\n');
  }

  return text(200, output.lastGoodBase64);
}

export async function startServer(
  listen: string,
  _loadedConfig: LoadedConfig,
  state: AppState,
  logger: Logger,
): Promise<{ close: () => Promise<void> }> {
  const separator = listen.lastIndexOf(':');
  const host = listen.slice(0, separator);
  const port = Number(listen.slice(separator + 1));

  const server = createServer((request: IncomingMessage, response: ServerResponse) => {
    const url = new URL(request.url ?? '/', 'http://localhost');
    const payload = handleServerRequest(url.pathname, state);
    response.writeHead(payload.statusCode, {
      'content-type': payload.contentType,
    });
    response.end(payload.body);
  });

  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(port, host, () => {
      server.off('error', reject);
      resolve();
    });
  });

  logger.info(
    {
      event: 'server_started',
      listen,
    },
    'Subscription server started.',
  );

  return {
    close: async () => {
      await new Promise<void>((resolve, reject) => {
        server.close((error) => {
          if (error) {
            reject(error);
            return;
          }
          resolve();
        });
      });
    },
  };
}

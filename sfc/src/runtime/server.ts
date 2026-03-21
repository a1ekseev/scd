import { createServer, type IncomingMessage, type ServerResponse } from 'node:http';
import { Buffer } from 'node:buffer';

import type { AppState, LoadedConfig } from '../types.ts';
import type { Logger } from '../logging/create-logger.ts';
import { buildOutputPath } from './output-path.ts';

export interface ResponsePayload {
  statusCode: number;
  contentType: string;
  body: string;
  headers?: Record<string, string>;
}

function text(statusCode: number, body: string): ResponsePayload {
  return {
    statusCode,
    contentType: 'text/plain; charset=utf-8',
    body,
  };
}

function matchesUserAgent(userAgent: string | undefined, allowedPrefixes: string[] | undefined): boolean {
  if (!userAgent || !allowedPrefixes || allowedPrefixes.length === 0) {
    return false;
  }

  return allowedPrefixes.some((prefix) => userAgent.startsWith(prefix));
}

function encodeProfileTitle(value: string): string {
  return `base64:${Buffer.from(value, 'utf8').toString('base64')}`;
}

export function handleServerRequest(
  urlPath: string,
  state: AppState,
  requestHeaders: Record<string, string | string[] | undefined> = {},
): ResponsePayload {
  if (urlPath === '/healthz') {
    return text(200, 'ok\n');
  }

  const output = state.outputsByPath[urlPath];
  if (!output) {
    return text(404, 'not found\n');
  }

  if (!output.lastGoodBase64) {
    return text(502, 'output is not available yet\n');
  }

  const response = text(200, output.lastGoodBase64);
  const userAgentHeader = requestHeaders['user-agent'];
  const userAgent = Array.isArray(userAgentHeader) ? userAgentHeader[0] : userAgentHeader;

  if (!matchesUserAgent(userAgent, output.userAgent)) {
    return response;
  }

  const headers: Record<string, string> = {};
  if (output.profileTitle) {
    headers['profile-title'] = encodeProfileTitle(output.profileTitle);
  }
  if (output.profileUpdateInterval !== undefined) {
    headers['profile-update-interval'] = String(output.profileUpdateInterval);
  }
  if (Object.keys(headers).length > 0) {
    response.headers = headers;
  }

  return response;
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
    const payload = handleServerRequest(url.pathname, state, request.headers);
    response.writeHead(payload.statusCode, {
      'content-type': payload.contentType,
      ...(payload.headers ?? {}),
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

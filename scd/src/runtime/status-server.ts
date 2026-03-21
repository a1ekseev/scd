import { createServer, type Server } from 'node:http';
import { URL } from 'node:url';

import type { Logger } from '../logging/create-logger.ts';
import type { GroupedStatusSubscription, LoadedConfig, StatusSnapshotTunnel } from '../types.ts';
import { ApiRequestError } from '../errors.ts';
import { buildCurrentRuntimeStateSnapshot } from './current-runtime-state.ts';
import { buildStatusSnapshot, type SyncMemoryState } from './run-state.ts';

interface StatusServerDependencies {
  buildCurrentRuntimeStateSnapshotFn?: typeof buildCurrentRuntimeStateSnapshot;
}

interface StatusServerResponse {
  statusCode: number;
  contentType: string;
  body: string;
}

function parseListenAddress(value: string): { host: string; port: number } {
  const separator = value.lastIndexOf(':');
  if (separator <= 0 || separator === value.length - 1) {
    throw new Error('statusServer.listen must use "host:port" format.');
  }

  return {
    host: value.slice(0, separator),
    port: Number(value.slice(separator + 1)),
  };
}

function escapeHtml(value: string): string {
  return value
    .replaceAll('&', '&amp;')
    .replaceAll('<', '&lt;')
    .replaceAll('>', '&gt;')
    .replaceAll('"', '&quot;');
}

function formatBitsPerSecond(value?: number): string {
  if (!value) {
    return '-';
  }

  if (value >= 1_000_000_000) {
    return `${(value / 1_000_000_000).toFixed(2)} Gbps`;
  }
  if (value >= 1_000_000) {
    return `${(value / 1_000_000).toFixed(2)} Mbps`;
  }
  if (value >= 1_000) {
    return `${(value / 1_000).toFixed(2)} Kbps`;
  }
  return `${value} bps`;
}

function renderHtml(snapshot: StatusSnapshotTunnel[]): string {
  const grouped = groupStatusSnapshot(snapshot);
  const sections = grouped
    .map((subscription) => {
      const targets = subscription.targets
        .map((target) => {
          const rows = target.tunnels
            .map((item) => {
              return `<tr>
<td>${escapeHtml(item.displayName)}</td>
<td>${escapeHtml(item.countryIso2 ?? '-')}</td>
<td>${escapeHtml(item.endpoint)}</td>
<td>${escapeHtml(item.state)}</td>
<td>${escapeHtml(item.lastHttpStatus ? String(item.lastHttpStatus) : '-')}</td>
<td>${escapeHtml(item.lastLatencyMs ? `${item.lastLatencyMs} ms` : '-')}</td>
<td>${escapeHtml(formatBitsPerSecond(item.lastBitsPerSecond))}</td>
</tr>`;
            })
            .join('\n');

          const runtimeStateHref = `/api/runtime-state?subscriptionId=${encodeURIComponent(subscription.subscriptionId)}&targetAddress=${encodeURIComponent(target.targetAddress)}`;

          return `<section class="target-card">
  <div class="target-head">
    <div>
      <h3>${escapeHtml(target.targetAddress)}</h3>
      <p>${target.tunnels.length} tunnel(s)</p>
      <p>Balancer: ${escapeHtml(target.balancerMonitor?.state ?? 'idle')} | HTTP ${escapeHtml(target.balancerMonitor?.lastStatusCode ? String(target.balancerMonitor.lastStatusCode) : '-')} | Latency ${escapeHtml(target.balancerMonitor?.lastLatencyMs ? `${target.balancerMonitor.lastLatencyMs} ms` : '-')} | Success GET ${escapeHtml(target.balancerMonitor?.successGetLastStatusCode ? String(target.balancerMonitor.successGetLastStatusCode) : '-')}</p>
    </div>
    <a class="action-link" href="${runtimeStateHref}">Current runtime state (JSON)</a>
  </div>
  <table>
    <thead>
      <tr>
        <th>Name</th>
        <th>Country</th>
        <th>SOCKS Endpoint</th>
        <th>State</th>
        <th>HTTP</th>
        <th>Latency</th>
        <th>Speed</th>
      </tr>
    </thead>
    <tbody>
      ${rows || '<tr><td colspan="7">No tunnel data.</td></tr>'}
    </tbody>
  </table>
</section>`;
        })
        .join('\n');

      return `<section class="subscription-card">
  <div class="subscription-head">
    <h2>${escapeHtml(subscription.subscriptionId)}</h2>
  </div>
  ${targets}
</section>`;
    })
    .join('\n');

  return `<!doctype html>
<html lang="en">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>scd status</title>
  <style>
    :root { color-scheme: light; }
    body { font-family: ui-sans-serif, system-ui, sans-serif; margin: 24px; background: #f5f3ef; color: #1c1712; }
    h1 { margin: 0 0 12px; font-size: 28px; }
    h2 { margin: 0; font-size: 18px; }
    h3 { margin: 0; font-size: 16px; }
    p { margin: 4px 0 0; color: #6c5d4a; font-size: 13px; }
    .subscription-card { margin: 0 0 20px; padding: 12px; border: 1px solid #d9d1c4; background: #fffaf0; }
    .target-card { margin: 12px 0 0; padding: 12px; border: 1px solid #e0d7c8; background: #fffdf8; }
    .subscription-head, .target-head { display: flex; justify-content: space-between; align-items: center; gap: 12px; margin: 0 0 12px; }
    .action-link { color: #6d3f0e; text-decoration: none; font-weight: 600; }
    .action-link:hover { text-decoration: underline; }
    table { width: 100%; border-collapse: collapse; background: #fffdf8; border: 1px solid #d9d1c4; }
    th, td { padding: 10px 12px; border-bottom: 1px solid #e8e0d5; text-align: left; font-size: 14px; }
    th { background: #efe6d7; }
    tr:nth-child(even) { background: #fbf7ef; }
  </style>
</head>
<body>
  <h1>scd status</h1>
  ${sections || '<section class="subscription-card"><div class="subscription-head"><h2>No subscriptions</h2></div></section>'}
</body>
</html>`;
}

export function groupStatusSnapshot(snapshot: StatusSnapshotTunnel[]): GroupedStatusSubscription[] {
  const subscriptions = new Map<string, Map<string, StatusSnapshotTunnel[]>>();

  for (const item of snapshot) {
    const targets = subscriptions.get(item.subscriptionId) ?? new Map<string, StatusSnapshotTunnel[]>();
    subscriptions.set(item.subscriptionId, targets);
    const tunnels = targets.get(item.targetAddress) ?? [];
    targets.set(item.targetAddress, tunnels);
    tunnels.push(item);
  }

  return [...subscriptions.entries()]
    .sort(([left], [right]) => left.localeCompare(right))
    .map(([subscriptionId, targets]) => ({
      subscriptionId,
      targets: [...targets.entries()]
        .sort(([left], [right]) => left.localeCompare(right))
        .map(([targetAddress, tunnels]) => ({
          subscriptionId,
          targetAddress,
          tunnels: [...tunnels].sort((left, right) => left.displayName.localeCompare(right.displayName)),
          balancerMonitor: tunnels[0]
            ? {
                state: tunnels[0].balancerMonitorState ?? 'idle',
                lastStatusCode: tunnels[0].balancerMonitorLastStatusCode,
                lastLatencyMs: tunnels[0].balancerMonitorLastLatencyMs,
                successGetLastStatusCode: tunnels[0].balancerMonitorSuccessGetLastStatusCode,
              }
            : undefined,
        })),
    }));
}

export interface RunningStatusServer {
  close(): Promise<void>;
}

export async function handleStatusServerRequest(
  requestUrl: string | undefined,
  loadedConfig: LoadedConfig,
  memoryState: SyncMemoryState,
  dependencies: StatusServerDependencies = {},
): Promise<StatusServerResponse> {
  const snapshot = buildStatusSnapshot(memoryState);
  const buildCurrentRuntimeStateSnapshotFn =
    dependencies.buildCurrentRuntimeStateSnapshotFn ?? buildCurrentRuntimeStateSnapshot;

  if (requestUrl === '/api/status') {
    return {
      statusCode: 200,
      contentType: 'application/json; charset=utf-8',
      body: JSON.stringify({ tunnels: snapshot }, null, 2),
    };
  }

  if (requestUrl?.startsWith('/api/runtime-state')) {
    if (!loadedConfig.config.statusServer.runtimeState.enabled) {
      return {
        statusCode: 404,
        contentType: 'application/json; charset=utf-8',
        body: JSON.stringify({ error: 'Runtime state endpoint is disabled.' }, null, 2),
      };
    }

    try {
      const url = new URL(requestUrl, 'http://status.local');
      const subscriptionId = url.searchParams.get('subscriptionId')?.trim();
      const targetAddress = url.searchParams.get('targetAddress')?.trim();

      if (!subscriptionId || !targetAddress) {
        return {
          statusCode: 400,
          contentType: 'application/json; charset=utf-8',
          body: JSON.stringify({ error: 'subscriptionId and targetAddress query parameters are required.' }, null, 2),
        };
      }

      const runtimeState = await buildCurrentRuntimeStateSnapshotFn(
        loadedConfig,
        subscriptionId,
        targetAddress,
        { memoryState },
        {
          includeRaw: loadedConfig.config.statusServer.runtimeState.includeRaw,
          includeSecrets: loadedConfig.config.statusServer.runtimeState.includeSecrets,
        },
      );

      if (!runtimeState) {
        return {
          statusCode: 404,
          contentType: 'application/json; charset=utf-8',
          body: JSON.stringify({ error: 'Target not found in loaded config.' }, null, 2),
        };
      }

      return {
        statusCode: 200,
        contentType: 'application/json; charset=utf-8',
        body: JSON.stringify(runtimeState, null, 2),
      };
    } catch (error) {
      return {
        statusCode: error instanceof ApiRequestError ? 502 : 500,
        contentType: 'application/json; charset=utf-8',
        body: JSON.stringify(
          { error: error instanceof Error ? error.message : String(error) },
          null,
          2,
        ),
      };
    }
  }

  return {
    statusCode: 200,
    contentType: 'text/html; charset=utf-8',
    body: renderHtml(snapshot),
  };
}

export async function startStatusServer(
  listen: string,
  loadedConfig: LoadedConfig,
  memoryState: SyncMemoryState,
  logger: Logger,
  dependencies: StatusServerDependencies = {},
): Promise<RunningStatusServer> {
  const address = parseListenAddress(listen);
  const server = createServer(async (request, response) => {
    const result = await handleStatusServerRequest(request.url, loadedConfig, memoryState, dependencies);
    response.writeHead(result.statusCode, { 'content-type': result.contentType });
    response.end(result.body);
  });

  await new Promise<void>((resolve, reject) => {
    server.once('error', reject);
    server.listen(address.port, address.host, () => {
      server.off('error', reject);
      resolve();
    });
  });

  logger.info({ event: 'status_server_started', listen }, 'Status server started.');

  return {
    async close() {
      await new Promise<void>((resolve, reject) => {
        (server as Server).close((error) => {
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

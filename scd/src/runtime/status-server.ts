import { createServer, type Server } from 'node:http';
import { URL } from 'node:url';

import type { Logger } from '../logging/create-logger.ts';
import type { GroupedStatusSubscription, LoadedConfig, StatusSnapshotTunnel } from '../types.ts';
import { ApiRequestError } from '../errors.ts';
import { buildCurrentRuntimeStateSnapshot } from './current-runtime-state.ts';
import { buildStatusSnapshot, buildTargetStateKey, type SyncMemoryState } from './run-state.ts';

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

function stateClass(value: string | undefined): string {
  if (value === 'healthy' || value === 'degraded' || value === 'repairing') {
    return value;
  }
  return 'idle';
}

function formatLatency(value?: number): string {
  return value === undefined ? '-' : `${value} ms`;
}

function formatLastCheck(latency?: number, error?: string): string {
  if (latency !== undefined) {
    return formatLatency(latency);
  }
  return error ? `Error: ${error}` : '-';
}

function formatBalancerStatus(value?: boolean): string {
  if (value === true) {
    return 'active';
  }
  if (value === false) {
    return 'removed';
  }
  return 'n/a';
}

function formatRemotePushStatus(state?: string, reportedStatus?: 'up' | 'down'): string {
  const delivery = state === 'ok'
    ? 'delivered'
    : state === 'failed'
      ? 'failed'
      : state === 'pending'
        ? 'pending'
        : 'idle';

  return reportedStatus ? `${delivery} ${reportedStatus}` : delivery;
}

function metricClass(label: string, value: string): string {
  if (label === 'State') {
    return ` metric-state-${stateClass(value)}`;
  }
  if (label === 'Balancer') {
    if (value === 'active') {
      return ' metric-balancer-active';
    }
    if (value === 'removed') {
      return ' metric-balancer-removed';
    }
    return ' metric-balancer-na';
  }
  if (label === 'Remote Push') {
    if (value.includes('delivered up')) {
      return ' metric-remote-up';
    }
    if (value.includes('delivered down') || value.includes('failed')) {
      return ' metric-remote-down';
    }
    if (value.includes('pending')) {
      return ' metric-remote-pending';
    }
    return ' metric-remote-idle';
  }
  return '';
}

function compareLatencyThenName(left: StatusSnapshotTunnel, right: StatusSnapshotTunnel): number {
  const leftLatency = left.state === 'idle' || left.lastLatencyMs === undefined ? Number.POSITIVE_INFINITY : left.lastLatencyMs;
  const rightLatency = right.state === 'idle' || right.lastLatencyMs === undefined ? Number.POSITIVE_INFINITY : right.lastLatencyMs;
  return leftLatency - rightLatency || left.displayName.localeCompare(right.displayName);
}

function renderMetric(label: string, value: string): string {
  return `<span class="metric${metricClass(label, value)}"><span class="metric-label">${escapeHtml(label)}</span><span class="metric-value">${escapeHtml(value)}</span></span>`;
}

function renderTunnelCard(item: StatusSnapshotTunnel): string {
  const state = stateClass(item.state);
  return `<article class="node-card state-${state}">
  <div class="node-orb" aria-hidden="true"></div>
  <div class="node-main">
    <div class="node-title">${escapeHtml(item.displayName)}</div>
    <div class="node-meta">${escapeHtml(item.countryIso2 ?? '-')} · ${escapeHtml(item.endpoint)}</div>
    <div class="node-metrics">
      ${renderMetric('State', item.state)}
      ${renderMetric('Last Check', formatLastCheck(item.lastLatencyMs, item.lastError))}
      ${renderMetric('Balancer', formatBalancerStatus(item.balanced))}
    </div>
  </div>
</article>`;
}

function buildConfiguredStatusGroups(
  snapshot: StatusSnapshotTunnel[],
  loadedConfig: LoadedConfig,
  memoryState: SyncMemoryState,
): GroupedStatusSubscription[] {
  const grouped = groupStatusSnapshot(snapshot);
  const subscriptionMap = new Map<string, GroupedStatusSubscription>(
    grouped.map((subscription) => [
      subscription.subscriptionId,
      {
        subscriptionId: subscription.subscriptionId,
        targets: subscription.targets.map((target) => ({ ...target })),
      },
    ]),
  );

  for (const subscription of loadedConfig.config.subscriptions.filter((item) => item.enabled)) {
    const configuredSubscription = subscriptionMap.get(subscription.id) ?? {
      subscriptionId: subscription.id,
      targets: [],
    };
    subscriptionMap.set(subscription.id, configuredSubscription);

    const targetState = memoryState.targets[buildTargetStateKey(subscription.id, subscription.target.address)];
    const existingTarget = configuredSubscription.targets.find(
      (target) => target.targetAddress === subscription.target.address,
    );
    const balancerMonitor = targetState?.balancerMonitor
      ? {
          state: targetState.balancerMonitor.state,
          lastStatusCode: targetState.balancerMonitor.lastStatusCode,
          lastLatencyMs: targetState.balancerMonitor.lastLatencyMs,
          lastError: targetState.balancerMonitor.lastError,
          lastCheckedAt: targetState.balancerMonitor.lastCheckedAt,
          lastSuccessAt: targetState.balancerMonitor.lastSuccessAt,
          lastFailureAt: targetState.balancerMonitor.lastFailureAt,
          lastSuccessStatusCode: targetState.balancerMonitor.lastSuccessStatusCode,
          lastSuccessLatencyMs: targetState.balancerMonitor.lastSuccessLatencyMs,
          remotePingState: targetState.balancerMonitor.remotePingState,
          remotePingLastRunAt: targetState.balancerMonitor.remotePingLastRunAt,
          remotePingLastSuccessAt: targetState.balancerMonitor.remotePingLastSuccessAt,
          remotePingLastFailureAt: targetState.balancerMonitor.remotePingLastFailureAt,
          remotePingLastStatusCode: targetState.balancerMonitor.remotePingLastStatusCode,
          remotePingLastError: targetState.balancerMonitor.remotePingLastError,
          remotePingLastReportedStatus: targetState.balancerMonitor.remotePingLastReportedStatus,
          remotePingLastReportedMsg: targetState.balancerMonitor.remotePingLastReportedMsg,
          remotePingLastReportedPingMs: targetState.balancerMonitor.remotePingLastReportedPingMs,
        }
      : existingTarget?.balancerMonitor ?? { state: 'idle' as const };

    if (existingTarget) {
      existingTarget.balancerMonitor = balancerMonitor;
      continue;
    }

    configuredSubscription.targets.push({
      subscriptionId: subscription.id,
      targetAddress: subscription.target.address,
      tunnels: [],
      balancerMonitor,
    });
  }

  return [...subscriptionMap.values()]
    .sort((left, right) => left.subscriptionId.localeCompare(right.subscriptionId))
    .map((subscription) => ({
      ...subscription,
      targets: [...subscription.targets].sort((left, right) => left.targetAddress.localeCompare(right.targetAddress)),
    }));
}

function renderHtml(
  snapshot: StatusSnapshotTunnel[],
  loadedConfig: LoadedConfig,
  memoryState: SyncMemoryState,
): string {
  const grouped = buildConfiguredStatusGroups(snapshot, loadedConfig, memoryState);
  const sections = grouped
    .map((subscription) => {
      const targets = subscription.targets
        .map((target) => {
          const balancerState = target.balancerMonitor?.state ?? 'idle';
          const balancerStateClass = stateClass(balancerState);
          const tunnelCards = target.tunnels.map(renderTunnelCard).join('\n');

          return `<section class="target-card">
  <div class="target-head">
    <div>
      <h3>${escapeHtml(target.targetAddress)}</h3>
      <p>${target.tunnels.length} tunnel(s)</p>
    </div>
  </div>
  <article class="balancer-card state-${balancerStateClass}">
    <div class="node-orb" aria-hidden="true"></div>
    <div class="node-main">
      <div class="node-kicker">Balancer</div>
      <div class="node-title">External SOCKS check</div>
      <div class="node-metrics">
        ${renderMetric('State', balancerState)}
        ${renderMetric('Last Check', formatLastCheck(target.balancerMonitor?.lastLatencyMs, target.balancerMonitor?.lastError))}
        ${renderMetric('Remote Push', formatRemotePushStatus(
          target.balancerMonitor?.remotePingState,
          target.balancerMonitor?.remotePingLastReportedStatus,
        ))}
      </div>
    </div>
  </article>
  <div class="nodes-grid">
    ${tunnelCards || '<p class="empty-state">No tunnel data.</p>'}
  </div>
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
  <title>Subscription Control Daemon Status</title>
  <style>
    :root {
      color-scheme: light;
      --bg: #f2efe7;
      --panel: #fffaf0;
      --panel-strong: #fff4df;
      --ink: #1f1a14;
      --muted: #766b5c;
      --line: #dfd3c0;
      --healthy: #16864d;
      --degraded: #c53931;
      --repairing: #c27a12;
      --idle: #8b8580;
    }
    body {
      margin: 0;
      min-height: 100vh;
      font-family: ui-sans-serif, system-ui, sans-serif;
      color: var(--ink);
      background:
        radial-gradient(circle at top left, rgba(194, 122, 18, .16), transparent 32rem),
        linear-gradient(135deg, #f8f5ed, var(--bg));
    }
    main { max-width: 1440px; margin: 0 auto; padding: 28px; }
    h1 { margin: 0 0 20px; font-size: clamp(28px, 4vw, 48px); letter-spacing: -.04em; }
    h2 { margin: 0; font-size: 22px; letter-spacing: -.02em; }
    h3 { margin: 0; font-size: 18px; }
    p { margin: 4px 0 0; color: var(--muted); font-size: 13px; }
    .subscription-card {
      margin: 0 0 24px;
      padding: 16px;
      border: 1px solid var(--line);
      border-radius: 22px;
      background: rgba(255, 250, 240, .78);
      box-shadow: 0 18px 50px rgba(72, 55, 32, .08);
      backdrop-filter: blur(14px);
    }
    .target-card {
      margin: 14px 0 0;
      padding: 16px;
      border: 1px solid var(--line);
      border-radius: 18px;
      background: rgba(255, 253, 248, .82);
    }
    .subscription-head, .target-head { display: flex; justify-content: space-between; align-items: center; gap: 12px; margin: 0 0 12px; }
    .balancer-card, .node-card {
      position: relative;
      display: flex;
      gap: 13px;
      align-items: flex-start;
      min-width: 0;
      border: 1px solid var(--line);
      border-radius: 18px;
      background: linear-gradient(180deg, #fffdf8, #fbf4e7);
      box-shadow: 0 10px 24px rgba(72, 55, 32, .06);
    }
    .balancer-card { margin: 0 0 14px; padding: 16px; background: linear-gradient(135deg, var(--panel-strong), #fffdf8); }
    .nodes-grid { display: grid; grid-template-columns: repeat(auto-fill, minmax(260px, 1fr)); gap: 12px; }
    .node-card { padding: 14px; }
    .node-orb {
      flex: 0 0 auto;
      width: 18px;
      height: 18px;
      margin-top: 2px;
      border-radius: 999px;
      background: var(--idle);
      box-shadow: 0 0 0 6px rgba(139, 133, 128, .14);
    }
    .state-healthy .node-orb { background: var(--healthy); box-shadow: 0 0 0 6px rgba(22, 134, 77, .14); }
    .state-degraded .node-orb { background: var(--degraded); box-shadow: 0 0 0 6px rgba(197, 57, 49, .14); }
    .state-repairing .node-orb { background: var(--repairing); box-shadow: 0 0 0 6px rgba(194, 122, 18, .18); }
    .node-main { min-width: 0; width: 100%; }
    .node-kicker { margin: 0 0 4px; color: var(--muted); font-size: 12px; font-weight: 700; text-transform: uppercase; letter-spacing: .08em; }
    .node-title { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; font-weight: 750; font-size: 15px; }
    .node-meta { overflow: hidden; text-overflow: ellipsis; white-space: nowrap; margin-top: 4px; color: var(--muted); font-size: 12px; }
    .node-metrics { display: flex; flex-wrap: wrap; gap: 8px; margin-top: 12px; }
    .metric { display: inline-flex; align-items: baseline; gap: 5px; padding: 5px 8px; border-radius: 999px; background: rgba(239, 230, 215, .8); }
    .metric-label { color: var(--muted); font-size: 11px; }
    .metric-value { font-size: 12px; font-weight: 750; }
    .metric-state-healthy, .metric-balancer-active, .metric-remote-up { background: rgba(22, 134, 77, .14); color: #0f5f36; }
    .metric-state-degraded, .metric-balancer-removed, .metric-remote-down { background: rgba(197, 57, 49, .14); color: #8f241f; }
    .metric-state-repairing, .metric-remote-pending { background: rgba(194, 122, 18, .16); color: #8a5307; }
    .metric-state-idle, .metric-balancer-na, .metric-remote-idle { background: rgba(139, 133, 128, .14); color: #5f5a55; }
    .metric-state-healthy .metric-label, .metric-balancer-active .metric-label, .metric-remote-up .metric-label,
    .metric-state-degraded .metric-label, .metric-balancer-removed .metric-label, .metric-remote-down .metric-label,
    .metric-state-repairing .metric-label, .metric-remote-pending .metric-label,
    .metric-state-idle .metric-label, .metric-balancer-na .metric-label, .metric-remote-idle .metric-label { color: currentColor; opacity: .74; }
    .empty-state { padding: 18px; border: 1px dashed var(--line); border-radius: 16px; }
    @media (max-width: 720px) {
      main { padding: 18px; }
      .nodes-grid { grid-template-columns: 1fr; }
      .subscription-card, .target-card { border-radius: 16px; }
    }
  </style>
</head>
<body>
  <main>
    <h1>Subscription Control Daemon Status</h1>
    ${sections || '<section class="subscription-card"><div class="subscription-head"><h2>No subscriptions</h2></div></section>'}
  </main>
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
          tunnels: [...tunnels].sort(compareLatencyThenName),
          balancerMonitor: tunnels[0]
            ? {
                state: tunnels[0].balancerMonitorState ?? 'idle',
                lastStatusCode: tunnels[0].balancerMonitorLastStatusCode,
                lastLatencyMs: tunnels[0].balancerMonitorLastLatencyMs,
                lastError: tunnels[0].balancerMonitorLastError,
                lastCheckedAt: tunnels[0].balancerMonitorLastCheckedAt,
                lastSuccessAt: tunnels[0].balancerMonitorLastSuccessAt,
                lastFailureAt: tunnels[0].balancerMonitorLastFailureAt,
                lastSuccessStatusCode: tunnels[0].balancerMonitorLastSuccessStatusCode,
                lastSuccessLatencyMs: tunnels[0].balancerMonitorLastSuccessLatencyMs,
                remotePingState: tunnels[0].balancerMonitorRemotePingState,
                remotePingLastRunAt: tunnels[0].balancerMonitorRemotePingLastRunAt,
                remotePingLastSuccessAt: tunnels[0].balancerMonitorRemotePingLastSuccessAt,
                remotePingLastFailureAt: tunnels[0].balancerMonitorRemotePingLastFailureAt,
                remotePingLastStatusCode: tunnels[0].balancerMonitorRemotePingLastStatusCode,
                remotePingLastError: tunnels[0].balancerMonitorRemotePingLastError,
                remotePingLastReportedStatus: tunnels[0].balancerMonitorRemotePingLastReportedStatus,
                remotePingLastReportedMsg: tunnels[0].balancerMonitorRemotePingLastReportedMsg,
                remotePingLastReportedPingMs: tunnels[0].balancerMonitorRemotePingLastReportedPingMs,
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
    body: renderHtml(snapshot, loadedConfig, memoryState),
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

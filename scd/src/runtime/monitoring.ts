import { XrayHandlerClient } from '../api/xray-handler-client.ts';
import { buildRoutingGrpc } from '../builders/build-routing-grpc.ts';
import { buildOutboundGrpc } from '../builders/build-outbound-grpc.ts';
import type { Logger } from '../logging/create-logger.ts';
import type { SubscriptionTargetConfig, TunnelRuntimeState } from '../types.ts';
import {
  buildTargetStateKey,
  getOrCreateTargetState,
  setTargetBalancerMonitorState,
  setTunnelMonitorState,
  withTargetMutationLock,
  type TargetApplyState,
  type SyncMemoryState,
} from './run-state.ts';
import { requestViaSocks } from './socks-http.ts';

type MutationApiClient = Pick<XrayHandlerClient, 'addOutbound' | 'removeOutbound' | 'addRule' | 'removeRule'>;

interface MonitoringDependencies {
  requestViaSocksFn?: typeof requestViaSocks;
  directRemotePingFn?: typeof pushRemotePingDirect;
  repairTunnelFn?: typeof repairTunnel;
  rejoinTunnelFn?: typeof rejoinTunnelToBalancer;
  createMutationClient?: (target: SubscriptionTargetConfig) => MutationApiClient;
}

interface RemotePingPayload {
  status: 'up' | 'down';
  msg: string;
  pingMs?: number;
}

function now(): string {
  return new Date().toISOString();
}

function createDefaultMutationClient(target: SubscriptionTargetConfig): MutationApiClient {
  return new XrayHandlerClient(target.address, {
    timeoutMs: target.timeoutMs,
  });
}

function resolveProxyHost(listen: string, targetAddress: string): string {
  if (listen === '0.0.0.0' || listen === '::') {
    return targetAddress.split(':')[0] ?? listen;
  }

  return listen;
}

function hasMonitorRequest(target: SubscriptionTargetConfig): target is SubscriptionTargetConfig & { monitor: { enabled: true; request: NonNullable<SubscriptionTargetConfig['monitor']['request']> } } {
  return Boolean(target.monitor.enabled && target.monitor.request);
}

function hasBalancerMonitorRequest(target: SubscriptionTargetConfig): target is SubscriptionTargetConfig & {
  balancerMonitor: {
    enabled: true;
    socks5: NonNullable<SubscriptionTargetConfig['balancerMonitor']['socks5']>;
    request: NonNullable<SubscriptionTargetConfig['balancerMonitor']['request']>;
  };
} {
  return Boolean(target.balancerMonitor.enabled && target.balancerMonitor.socks5 && target.balancerMonitor.request);
}

function hasRemotePing(target: SubscriptionTargetConfig): target is SubscriptionTargetConfig & {
  balancerMonitor: SubscriptionTargetConfig['balancerMonitor'] & {
    remotePing: {
      enabled: true;
      url: string;
      timeoutMs: number;
      viaSocks: boolean;
    };
    socks5: NonNullable<SubscriptionTargetConfig['balancerMonitor']['socks5']>;
  };
} {
  return Boolean(target.balancerMonitor.remotePing?.enabled && target.balancerMonitor.remotePing.url && target.balancerMonitor.socks5);
}

export function buildRemotePingUrl(baseUrl: string, payload: RemotePingPayload): string {
  const url = new URL(baseUrl);
  url.searchParams.set('status', payload.status);
  url.searchParams.set('msg', payload.msg);
  url.searchParams.delete('ping');
  if (payload.pingMs !== undefined) {
    url.searchParams.set('ping', String(payload.pingMs));
  }
  return url.toString();
}

function getRemoteHost(urlValue: string): string {
  try {
    return new URL(urlValue).host;
  } catch {
    return '<invalid>';
  }
}

export async function pushRemotePingDirect(config: {
  url: string;
  timeoutMs: number;
}): Promise<{ statusCode: number }> {
  const controller = new AbortController();
  const timeout = setTimeout(() => {
    controller.abort();
  }, config.timeoutMs);

  try {
    const response = await fetch(config.url, {
      method: 'GET',
      signal: controller.signal,
    });
    const statusCode = response.status;
    await response.body?.cancel().catch(() => undefined);
    return { statusCode };
  } catch (error) {
    if (error instanceof Error && error.name === 'AbortError') {
      throw new Error(`Remote ping timed out after ${config.timeoutMs}ms.`);
    }
    throw error;
  } finally {
    clearTimeout(timeout);
  }
}

async function executeRequestViaSocks(
  requestViaSocksFn: typeof requestViaSocks,
  config: {
    proxyHost: string;
    proxyPort: number;
    url: string;
    method: 'GET' | 'HEAD' | 'POST';
    timeoutMs: number;
  },
): Promise<Awaited<ReturnType<typeof requestViaSocksFn>>> {
  return await requestViaSocksFn(config);
}

async function runWithConcurrencyLimit<T>(
  items: T[],
  maxParallel: number,
  worker: (item: T) => Promise<void>,
): Promise<void> {
  if (items.length === 0) {
    return;
  }

  const concurrency = Math.max(1, Math.min(maxParallel, items.length));
  let index = 0;

  async function runWorker(): Promise<void> {
    while (true) {
      const current = index;
      index += 1;
      if (current >= items.length) {
        return;
      }
      await worker(items[current]!);
    }
  }

  const results = await Promise.allSettled(Array.from({ length: concurrency }, () => runWorker()));
  const rejected = results.filter((result): result is PromiseRejectedResult => result.status === 'rejected');
  if (rejected.length > 0) {
    throw new AggregateError(rejected.map((result) => result.reason), 'Concurrent task execution failed.');
  }
}

async function repairTunnel(
  subscriptionId: string,
  target: SubscriptionTargetConfig,
  memoryState: SyncMemoryState,
  runtimeState: TunnelRuntimeState,
  logger: Logger,
  createMutationClient: NonNullable<MonitoringDependencies['createMutationClient']>,
): Promise<void> {
  const targetKey = buildTargetStateKey(subscriptionId, target.address);

  await withTargetMutationLock(memoryState, targetKey, async () => {
    const targetState = getOrCreateTargetState(memoryState, targetKey);
    const current = targetState.tunnels[runtimeState.tunnel.baseTunnelId];
    if (!current) {
      return;
    }

    setTunnelMonitorState(targetState, current.tunnel.baseTunnelId, (monitor) => ({
      ...monitor,
      state: 'repairing',
    }));

    const client = createMutationClient(target);

    try {
      await client.removeRule(current.tunnel.routeTag).catch(() => undefined);
      await client.removeOutbound(current.tunnel.outboundTagCurrent).catch(() => undefined);

      const repairedTunnel = {
        ...current.tunnel,
        outboundTagCurrent: current.tunnel.outboundWithoutPrefix.tag,
      };

      await client.addOutbound(buildOutboundGrpc(repairedTunnel.outboundWithoutPrefix.normalized).raw);
      await client.addRule(buildRoutingGrpc(repairedTunnel).raw);

      targetState.tunnels[current.tunnel.baseTunnelId] = {
        ...current,
        tunnel: repairedTunnel,
        monitor: {
          ...current.monitor,
          state: 'degraded',
          lastStatusCode: undefined,
          lastLatencyMs: undefined,
          consecutiveFailures: 0,
          lastError: 'Tunnel repaired; awaiting next successful monitor check.',
        },
      };

      logger.warn(
        {
          event: 'tunnel_repaired',
          subscriptionId,
          targetAddress: target.address,
          tunnelId: current.tunnel.baseTunnelId,
          outboundTag: repairedTunnel.outboundTagCurrent,
        },
        'Tunnel repaired after failed monitor probe.',
      );
    } catch (error) {
      setTunnelMonitorState(targetState, current.tunnel.baseTunnelId, (monitor) => ({
        ...monitor,
        state: 'degraded',
        lastFailureAt: now(),
        lastError: error instanceof Error ? error.message : String(error),
      }));

      logger.error(
        {
          event: 'tunnel_repair_failed',
          subscriptionId,
          targetAddress: target.address,
          tunnelId: current.tunnel.baseTunnelId,
          error: error instanceof Error ? error.message : String(error),
        },
        'Tunnel repair failed.',
      );
    }
  });
}

function shouldRejoinTunnel(runtimeState: TunnelRuntimeState): boolean {
  return (
    runtimeState.tunnel.outboundTagCurrent !== runtimeState.tunnel.outboundTagInitial &&
    runtimeState.tunnel.outboundTagInitial !== runtimeState.tunnel.outboundWithoutPrefix.tag
  );
}

async function rejoinTunnelToBalancer(
  subscriptionId: string,
  target: SubscriptionTargetConfig,
  memoryState: SyncMemoryState,
  runtimeState: TunnelRuntimeState,
  logger: Logger,
  createMutationClient: NonNullable<MonitoringDependencies['createMutationClient']>,
): Promise<void> {
  if (!shouldRejoinTunnel(runtimeState)) {
    return;
  }

  const targetKey = buildTargetStateKey(subscriptionId, target.address);

  await withTargetMutationLock(memoryState, targetKey, async () => {
    const targetState = getOrCreateTargetState(memoryState, targetKey);
    const current = targetState.tunnels[runtimeState.tunnel.baseTunnelId];
    if (!current || !shouldRejoinTunnel(current)) {
      return;
    }

    const client = createMutationClient(target);

    try {
      const rejoinedTunnel = {
        ...current.tunnel,
        outboundTagCurrent: current.tunnel.outboundTagInitial,
      };

      await client.addOutbound(buildOutboundGrpc(rejoinedTunnel.outboundInitial.normalized).raw);
      try {
        await client.removeRule(current.tunnel.routeTag).catch(() => undefined);
        await client.addRule(buildRoutingGrpc(rejoinedTunnel).raw);
        await client.removeOutbound(current.tunnel.outboundTagCurrent).catch(() => undefined);
      } catch (error) {
        await client.removeRule(current.tunnel.routeTag).catch(() => undefined);
        await client.addRule(buildRoutingGrpc(current.tunnel).raw).catch(() => undefined);
        await client.removeOutbound(rejoinedTunnel.outboundTagCurrent).catch(() => undefined);
        throw error;
      }

      targetState.tunnels[current.tunnel.baseTunnelId] = {
        ...current,
        tunnel: rejoinedTunnel,
        monitor: {
          ...current.monitor,
          lastError: undefined,
        },
      };

      logger.info(
        {
          event: 'tunnel_rejoined_balancer',
          subscriptionId,
          targetAddress: target.address,
          tunnelId: current.tunnel.baseTunnelId,
          outboundTag: rejoinedTunnel.outboundTagCurrent,
        },
        'Tunnel rejoined Xray balancer after successful monitor probe.',
      );
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      setTunnelMonitorState(targetState, current.tunnel.baseTunnelId, (monitor) => ({
        ...monitor,
        lastError: `Tunnel healthy but balancer rejoin failed: ${message}`,
      }));

      logger.error(
        {
          event: 'tunnel_rejoin_failed',
          subscriptionId,
          targetAddress: target.address,
          tunnelId: current.tunnel.baseTunnelId,
          error: message,
        },
        'Tunnel balancer rejoin failed after successful monitor probe.',
      );
    }
  });
}

export async function runTargetMonitorTick(
  subscriptionId: string,
  target: SubscriptionTargetConfig,
  memoryState: SyncMemoryState,
  logger: Logger,
  dependencies: MonitoringDependencies = {},
): Promise<void> {
  if (!hasMonitorRequest(target)) {
    return;
  }

  const targetKey = buildTargetStateKey(subscriptionId, target.address);
  const targetState = memoryState.targets[targetKey];
  if (!targetState) {
    return;
  }

  const snapshot = Object.values(targetState.tunnels);
  const requestViaSocksFn = dependencies.requestViaSocksFn ?? requestViaSocks;
  const repairTunnelFn = dependencies.repairTunnelFn ?? repairTunnel;
  const rejoinTunnelFn = dependencies.rejoinTunnelFn ?? rejoinTunnelToBalancer;
  const createMutationClient = dependencies.createMutationClient ?? createDefaultMutationClient;

  await runWithConcurrencyLimit(snapshot, target.monitor.maxParallel, async (runtimeState) => {
    let response: Awaited<ReturnType<typeof requestViaSocksFn>>;

    try {
      response = await executeRequestViaSocks(requestViaSocksFn, {
        proxyHost: resolveProxyHost(runtimeState.tunnel.listen, target.address),
        proxyPort: runtimeState.tunnel.port,
        url: target.monitor.request.url,
        method: target.monitor.request.method,
        timeoutMs: target.monitor.request.timeoutMs,
      });
    } catch (error) {
      const failedAt = now();
      setTunnelMonitorState(targetState, runtimeState.tunnel.baseTunnelId, (monitor) => ({
        ...monitor,
        state: 'degraded',
        lastCheckedAt: failedAt,
        lastFailureAt: failedAt,
        lastStatusCode: undefined,
        lastLatencyMs: undefined,
        lastError: error instanceof Error ? error.message : String(error),
        consecutiveFailures: monitor.consecutiveFailures + 1,
      }));

      await repairTunnelFn(subscriptionId, target, memoryState, runtimeState, logger, createMutationClient);
      return;
    }

    if (response.statusCode !== target.monitor.request.expectedStatus) {
      const error = new Error(`Expected HTTP ${target.monitor.request.expectedStatus}, got ${response.statusCode}.`);
      const failedAt = now();
      setTunnelMonitorState(targetState, runtimeState.tunnel.baseTunnelId, (monitor) => ({
        ...monitor,
        state: 'degraded',
        lastCheckedAt: failedAt,
        lastFailureAt: failedAt,
        lastStatusCode: undefined,
        lastLatencyMs: undefined,
        lastError: error.message,
        consecutiveFailures: monitor.consecutiveFailures + 1,
      }));

      await repairTunnelFn(subscriptionId, target, memoryState, runtimeState, logger, createMutationClient);
      return;
    }

    const checkedAt = now();
    setTunnelMonitorState(targetState, runtimeState.tunnel.baseTunnelId, (monitor) => ({
      ...monitor,
      state: 'healthy',
      lastCheckedAt: checkedAt,
      lastSuccessAt: checkedAt,
      lastStatusCode: response.statusCode,
      lastLatencyMs: response.latencyMs,
      lastSuccessStatusCode: response.statusCode,
      lastSuccessLatencyMs: response.latencyMs,
      lastError: undefined,
      consecutiveFailures: 0,
    }));

    try {
      await rejoinTunnelFn(subscriptionId, target, memoryState, runtimeState, logger, createMutationClient);
    } catch (error) {
      const message = error instanceof Error ? error.message : String(error);
      setTunnelMonitorState(targetState, runtimeState.tunnel.baseTunnelId, (monitor) => ({
        ...monitor,
        lastError: `Tunnel healthy but balancer rejoin failed: ${message}`,
      }));
      logger.error(
        {
          event: 'tunnel_rejoin_failed',
          subscriptionId,
          targetAddress: target.address,
          tunnelId: runtimeState.tunnel.baseTunnelId,
          error: message,
        },
        'Tunnel balancer rejoin failed after successful monitor probe.',
      );
    }
  });
}

export async function runTargetBalancerMonitorTick(
  subscriptionId: string,
  target: SubscriptionTargetConfig,
  memoryState: SyncMemoryState,
  logger: Logger,
  dependencies: Pick<MonitoringDependencies, 'requestViaSocksFn' | 'directRemotePingFn'> = {},
): Promise<void> {
  if (!hasBalancerMonitorRequest(target)) {
    return;
  }

  const targetKey = buildTargetStateKey(subscriptionId, target.address);
  const targetState = memoryState.targets[targetKey];
  if (!targetState) {
    return;
  }

  const requestViaSocksFn = dependencies.requestViaSocksFn ?? requestViaSocks;
  const directRemotePingFn = dependencies.directRemotePingFn ?? pushRemotePingDirect;

  let response: Awaited<ReturnType<typeof requestViaSocksFn>>;
  try {
    response = await executeRequestViaSocks(requestViaSocksFn, {
      proxyHost: target.balancerMonitor.socks5.host,
      proxyPort: target.balancerMonitor.socks5.port,
      url: target.balancerMonitor.request.url,
      method: target.balancerMonitor.request.method,
      timeoutMs: target.balancerMonitor.request.timeoutMs,
    });
  } catch (error) {
    const failedAt = now();
    const message = error instanceof Error ? error.message : String(error);
    setTargetBalancerMonitorState(targetState, (current) => ({
      ...current,
      state: 'degraded',
      lastCheckedAt: failedAt,
      lastFailureAt: failedAt,
      lastStatusCode: undefined,
      lastLatencyMs: undefined,
      lastError: message,
      consecutiveFailures: current.consecutiveFailures + 1,
    }));
    scheduleRemotePing(subscriptionId, target, targetState, logger, requestViaSocksFn, directRemotePingFn, {
      status: 'down',
      msg: message,
    });
    return;
  }

  if (response.statusCode !== target.balancerMonitor.request.expectedStatus) {
    const failedAt = now();
    const message = `Expected HTTP ${target.balancerMonitor.request.expectedStatus}, got ${response.statusCode}.`;
    setTargetBalancerMonitorState(targetState, (current) => ({
      ...current,
      state: 'degraded',
      lastCheckedAt: failedAt,
      lastFailureAt: failedAt,
      lastStatusCode: undefined,
      lastLatencyMs: undefined,
      lastError: message,
      consecutiveFailures: current.consecutiveFailures + 1,
    }));
    scheduleRemotePing(subscriptionId, target, targetState, logger, requestViaSocksFn, directRemotePingFn, {
      status: 'down',
      msg: message,
    });
    return;
  }

  const checkedAt = now();
  setTargetBalancerMonitorState(targetState, (current) => ({
    ...current,
    state: 'healthy',
    lastCheckedAt: checkedAt,
    lastSuccessAt: checkedAt,
    lastStatusCode: response.statusCode,
    lastLatencyMs: response.latencyMs,
    lastSuccessStatusCode: response.statusCode,
    lastSuccessLatencyMs: response.latencyMs,
    lastError: undefined,
    consecutiveFailures: 0,
  }));

  scheduleRemotePing(subscriptionId, target, targetState, logger, requestViaSocksFn, directRemotePingFn, {
    status: 'up',
    msg: 'OK',
    pingMs: response.latencyMs,
  });
}

function scheduleRemotePing(
  subscriptionId: string,
  target: SubscriptionTargetConfig,
  targetState: TargetApplyState,
  logger: Logger,
  requestViaSocksFn: typeof requestViaSocks,
  directRemotePingFn: NonNullable<MonitoringDependencies['directRemotePingFn']>,
  payload: RemotePingPayload,
): void {
  if (!hasRemotePing(target)) {
    return;
  }

  if (targetState.balancerMonitor.remotePingState === 'pending') {
    logger.warn(
      {
        event: 'balancer_monitor_remote_ping_skipped_overrun',
        subscriptionId,
        targetAddress: target.address,
      },
      'Balancer monitor remote ping skipped because previous push is still in progress.',
    );
    return;
  }

  const startedAt = now();
  setTargetBalancerMonitorState(targetState, (current) => ({
    ...current,
    remotePingState: 'pending',
    remotePingLastRunAt: startedAt,
    remotePingLastError: undefined,
    remotePingLastReportedStatus: payload.status,
    remotePingLastReportedMsg: payload.msg,
    remotePingLastReportedPingMs: payload.pingMs,
  }));

  logger.info(
    {
      event: 'balancer_monitor_remote_ping_started',
      subscriptionId,
      targetAddress: target.address,
      status: payload.status,
      reportedStatus: payload.status,
      viaSocks: target.balancerMonitor.remotePing.viaSocks,
      remoteHost: getRemoteHost(target.balancerMonitor.remotePing.url),
      ...(target.balancerMonitor.remotePing.viaSocks
        ? {
            proxyHost: target.balancerMonitor.socks5.host,
            proxyPort: target.balancerMonitor.socks5.port,
          }
        : {}),
    },
    'Balancer monitor remote ping started.',
  );

  void (async () => {
    try {
      const pushUrl = buildRemotePingUrl(target.balancerMonitor.remotePing.url, payload);
      const response = target.balancerMonitor.remotePing.viaSocks
        ? await requestViaSocksFn({
            proxyHost: target.balancerMonitor.socks5.host,
            proxyPort: target.balancerMonitor.socks5.port,
            url: pushUrl,
            method: 'GET',
            timeoutMs: target.balancerMonitor.remotePing.timeoutMs,
          })
        : await directRemotePingFn({
            url: pushUrl,
            timeoutMs: target.balancerMonitor.remotePing.timeoutMs,
          });

      if (response.statusCode < 200 || response.statusCode >= 300) {
        const failedAt = now();
        const message = `Remote ping returned HTTP ${response.statusCode}.`;
        setTargetBalancerMonitorState(targetState, (current) => ({
          ...current,
          remotePingState: 'failed',
          remotePingLastFailureAt: failedAt,
          remotePingLastStatusCode: response.statusCode,
          remotePingLastError: message,
        }));

        logger.warn(
          {
            event: 'balancer_monitor_remote_ping_failed',
            subscriptionId,
            targetAddress: target.address,
            statusCode: response.statusCode,
            error: message,
            reportedStatus: payload.status,
            viaSocks: target.balancerMonitor.remotePing.viaSocks,
            remoteHost: getRemoteHost(target.balancerMonitor.remotePing.url),
            ...(target.balancerMonitor.remotePing.viaSocks
              ? {
                  proxyHost: target.balancerMonitor.socks5.host,
                  proxyPort: target.balancerMonitor.socks5.port,
                }
              : {}),
          },
          'Balancer monitor remote ping failed.',
        );
        return;
      }

      const completedAt = now();
      setTargetBalancerMonitorState(targetState, (current) => ({
        ...current,
        remotePingState: 'ok',
        remotePingLastSuccessAt: completedAt,
        remotePingLastStatusCode: response.statusCode,
        remotePingLastError: undefined,
      }));

      logger.info(
        {
          event: 'balancer_monitor_remote_ping_succeeded',
          subscriptionId,
          targetAddress: target.address,
          statusCode: response.statusCode,
          reportedStatus: payload.status,
          viaSocks: target.balancerMonitor.remotePing.viaSocks,
          remoteHost: getRemoteHost(target.balancerMonitor.remotePing.url),
          ...(target.balancerMonitor.remotePing.viaSocks
            ? {
                proxyHost: target.balancerMonitor.socks5.host,
                proxyPort: target.balancerMonitor.socks5.port,
              }
            : {}),
        },
        'Balancer monitor remote ping succeeded.',
      );
    } catch (error) {
      const failedAt = now();
      const message = error instanceof Error ? error.message : String(error);
      setTargetBalancerMonitorState(targetState, (current) => ({
        ...current,
        remotePingState: 'failed',
        remotePingLastFailureAt: failedAt,
        remotePingLastStatusCode: undefined,
        remotePingLastError: message,
      }));

      logger.warn(
        {
          event: 'balancer_monitor_remote_ping_failed',
          subscriptionId,
          targetAddress: target.address,
          error: message,
          reportedStatus: payload.status,
          viaSocks: target.balancerMonitor.remotePing.viaSocks,
          remoteHost: getRemoteHost(target.balancerMonitor.remotePing.url),
          ...(target.balancerMonitor.remotePing.viaSocks
            ? {
                proxyHost: target.balancerMonitor.socks5.host,
                proxyPort: target.balancerMonitor.socks5.port,
              }
            : {}),
        },
        'Balancer monitor remote ping failed.',
      );
    }
  })();
}

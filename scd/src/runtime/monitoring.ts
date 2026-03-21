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
  setTunnelSpeedtestState,
  withTargetMutationLock,
  type SyncMemoryState,
} from './run-state.ts';
import { requestViaSocks } from './socks-http.ts';

interface MonitoringDependencies {
  requestViaSocksFn?: typeof requestViaSocks;
  repairTunnelFn?: typeof repairTunnel;
}

function now(): string {
  return new Date().toISOString();
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

function hasSpeedtestUrls(target: SubscriptionTargetConfig): target is SubscriptionTargetConfig & { speedtest: { enabled: true; urls: string[] } } {
  return Boolean(target.speedtest.enabled && target.speedtest.urls && target.speedtest.urls.length > 0);
}

function hasBalancerMonitorRequest(target: SubscriptionTargetConfig): target is SubscriptionTargetConfig & {
  balancerMonitor: {
    enabled: true;
    socks5: NonNullable<SubscriptionTargetConfig['balancerMonitor']['socks5']>;
    request: NonNullable<SubscriptionTargetConfig['balancerMonitor']['request']>;
    successGet?: SubscriptionTargetConfig['balancerMonitor']['successGet'];
  };
} {
  return Boolean(target.balancerMonitor.enabled && target.balancerMonitor.socks5 && target.balancerMonitor.request);
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

    const client = new XrayHandlerClient(target.address, {
      timeoutMs: target.timeoutMs,
    });

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
          consecutiveFailures: 0,
          lastError: undefined,
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
        lastError: error instanceof Error ? error.message : String(error),
        consecutiveFailures: monitor.consecutiveFailures + 1,
      }));

      await repairTunnelFn(subscriptionId, target, memoryState, runtimeState, logger);
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
        lastError: error.message,
        consecutiveFailures: monitor.consecutiveFailures + 1,
      }));

      await repairTunnelFn(subscriptionId, target, memoryState, runtimeState, logger);
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
      lastError: undefined,
      consecutiveFailures: 0,
    }));
  });
}

export async function runTargetSpeedtestTick(
  subscriptionId: string,
  target: SubscriptionTargetConfig,
  memoryState: SyncMemoryState,
  dependencies: MonitoringDependencies = {},
): Promise<void> {
  if (!hasSpeedtestUrls(target)) {
    return;
  }

  const targetKey = buildTargetStateKey(subscriptionId, target.address);
  const targetState = memoryState.targets[targetKey];
  if (!targetState) {
    return;
  }

  const snapshot = Object.values(targetState.tunnels);
  const requestViaSocksFn = dependencies.requestViaSocksFn ?? requestViaSocks;

  await runWithConcurrencyLimit(snapshot, target.speedtest.maxParallel, async (runtimeState) => {
    let lastError: unknown;
    let completed = false;

    for (const url of target.speedtest.urls) {
      const startedAt = Date.now();

      try {
        const response = await executeRequestViaSocks(requestViaSocksFn, {
          proxyHost: resolveProxyHost(runtimeState.tunnel.listen, target.address),
          proxyPort: runtimeState.tunnel.port,
          url,
          method: target.speedtest.method,
          timeoutMs: target.speedtest.timeoutMs,
        });
        const finishedAt = now();
        const durationMs = Math.max(1, Date.now() - startedAt);
        const bitsPerSecond = Math.round((response.bodyBytes * 8 * 1000) / durationMs);

        setTunnelSpeedtestState(targetState, runtimeState.tunnel.baseTunnelId, (speedtest) => ({
          ...speedtest,
          lastRunAt: finishedAt,
          lastSuccessAt: finishedAt,
          lastBytes: response.bodyBytes,
          lastDurationMs: durationMs,
          lastBitsPerSecond: bitsPerSecond,
          lastError: undefined,
        }));
        completed = true;
        break;
      } catch (error) {
        lastError = error;
      }
    }

    if (!completed) {
      const failedAt = now();
      setTunnelSpeedtestState(targetState, runtimeState.tunnel.baseTunnelId, (speedtest) => ({
        ...speedtest,
        lastRunAt: failedAt,
        lastFailureAt: failedAt,
        lastError: lastError instanceof Error ? lastError.message : String(lastError),
      }));
    }
  });
}

export async function runTargetBalancerMonitorTick(
  subscriptionId: string,
  target: SubscriptionTargetConfig,
  memoryState: SyncMemoryState,
  logger: Logger,
  dependencies: Pick<MonitoringDependencies, 'requestViaSocksFn'> = {},
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
    setTargetBalancerMonitorState(targetState, (current) => ({
      ...current,
      state: 'degraded',
      lastCheckedAt: failedAt,
      lastFailureAt: failedAt,
      lastError: error instanceof Error ? error.message : String(error),
      consecutiveFailures: current.consecutiveFailures + 1,
    }));
    return;
  }

  if (response.statusCode !== target.balancerMonitor.request.expectedStatus) {
    const failedAt = now();
    setTargetBalancerMonitorState(targetState, (current) => ({
      ...current,
      state: 'degraded',
      lastCheckedAt: failedAt,
      lastFailureAt: failedAt,
      lastStatusCode: response.statusCode,
      lastLatencyMs: response.latencyMs,
      lastError: `Expected HTTP ${target.balancerMonitor.request.expectedStatus}, got ${response.statusCode}.`,
      consecutiveFailures: current.consecutiveFailures + 1,
    }));
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
    lastError: undefined,
    consecutiveFailures: 0,
  }));

  if (!target.balancerMonitor.successGet) {
    return;
  }

  try {
    const successResponse = await executeRequestViaSocks(requestViaSocksFn, {
      proxyHost: target.balancerMonitor.socks5.host,
      proxyPort: target.balancerMonitor.socks5.port,
      url: target.balancerMonitor.successGet.url,
      method: 'GET',
      timeoutMs: target.balancerMonitor.successGet.timeoutMs,
    });

    setTargetBalancerMonitorState(targetState, (current) => ({
      ...current,
      successGetLastRunAt: now(),
      successGetLastStatusCode: successResponse.statusCode,
      successGetLastLatencyMs: successResponse.latencyMs,
      successGetLastError:
        successResponse.statusCode === target.balancerMonitor.successGet!.expectedStatus
          ? undefined
          : `Expected HTTP ${target.balancerMonitor.successGet!.expectedStatus}, got ${successResponse.statusCode}.`,
    }));
  } catch (error) {
    setTargetBalancerMonitorState(targetState, (current) => ({
      ...current,
      successGetLastRunAt: now(),
      successGetLastError: error instanceof Error ? error.message : String(error),
    }));
    logger.warn(
      {
        event: 'balancer_monitor_success_get_failed',
        subscriptionId,
        targetAddress: target.address,
        error: error instanceof Error ? error.message : String(error),
      },
      'Balancer monitor successGet failed.',
    );
  }
}

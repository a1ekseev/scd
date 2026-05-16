import assert from 'node:assert/strict';
import test from 'node:test';

import { ApiRequestError } from '../src/errors.ts';
import { buildTargetStateKey, createSyncMemoryState, getOrCreateTargetState } from '../src/runtime/run-state.ts';
import { handleStatusServerRequest } from '../src/runtime/status-server.ts';
import type { LoadedConfig, TargetTopology, TunnelMapping } from '../src/types.ts';

function createLoadedConfig(): LoadedConfig {
  return {
    configPath: '/tmp/config.yml',
    config: {
      subscriptions: [
        {
          id: 'sub-1',
          input: '/tmp/subscription.txt',
          enabled: true,
          format: 'plain',
          fetchTimeoutMs: 5000,
          filters: undefined,
          target: {
            address: '127.0.0.1:8080',
            timeoutMs: 5000,
            fixedOutbounds: [],
            fixedInbounds: [],
            fixedRouting: [],
            visionUdp443Override: false,
            inboundSocks: {
              listen: '127.0.0.1',
              portRange: {
                start: 20000,
                end: 20010,
              },
            },
            monitor: {
              enabled: false,
              maxParallel: 10,
            },
            balancerMonitor: {
              enabled: false,
            },
          },
        },
      ],
      runtime: {
        mode: 'run-once',
      },
      logging: {
        level: 'info',
        format: 'json',
      },
      resources: {
        outbounds: { enabled: true },
        inbounds: { enabled: true },
        routing: { enabled: true },
      },
      statusServer: {
        enabled: true,
        listen: '127.0.0.1:9090',
        runtimeState: {
          enabled: true,
          includeRaw: false,
          includeSecrets: false,
        },
      },
    },
  };
}

function createLoadedConfigWithDisabledSubscription(): LoadedConfig {
  const loadedConfig = createLoadedConfig();
  return {
    ...loadedConfig,
    config: {
      ...loadedConfig.config,
      subscriptions: [
        ...loadedConfig.config.subscriptions,
        {
          ...loadedConfig.config.subscriptions[0]!,
          id: 'sub-disabled',
          enabled: false,
          target: {
            ...loadedConfig.config.subscriptions[0]!.target,
            address: '127.0.0.1:9999',
          },
        },
      ],
    },
  };
}

function createTunnel(overrides: Partial<TunnelMapping> = {}): TunnelMapping {
  return {
    baseTunnelId: 'tunnel-a',
    displayName: 'Alpha',
    countryIso2: 'AT',
    baseOutboundTag: 'out-a',
    outboundTagInitial: 'out-a',
    outboundTagCurrent: 'out-a',
    inboundTag: 'in-a',
    routeTag: 'route-a',
    listen: '127.0.0.1',
    port: 20000,
    outboundInitial: {} as TunnelMapping['outboundInitial'],
    outboundWithoutPrefix: { tag: 'out-a' } as TunnelMapping['outboundWithoutPrefix'],
    ...overrides,
  };
}

test('status server renders HTML dashboard without JSON links and with state cards', async () => {
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('sub-1', '127.0.0.1:8080');
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  const tunnel = createTunnel({
    baseTunnelId: 'alpha',
    displayName: 'Alpha',
    countryIso2: 'AT',
    port: 20000,
  });
  targetState.topology = { tunnels: [tunnel] } as TargetTopology;
  targetState.tunnels = {
    alpha: {
      tunnel,
      monitor: {
        state: 'healthy',
        consecutiveFailures: 0,
        lastStatusCode: 204,
        lastLatencyMs: 42,
        lastSuccessStatusCode: 204,
        lastSuccessLatencyMs: 42,
      },
    },
  };
  targetState.balancerMonitor = {
    state: 'degraded',
    remotePingState: 'failed',
    consecutiveFailures: 1,
    lastError: 'balancer failed',
    lastSuccessStatusCode: 204,
    lastSuccessLatencyMs: 90,
    remotePingLastError: 'push failed',
  };

  const result = await handleStatusServerRequest('/', createLoadedConfig(), memoryState);

  assert.equal(result.statusCode, 200);
  assert.equal(result.contentType, 'text/html; charset=utf-8');
  assert.match(result.body, /<title>Subscription Control Daemon Status<\/title>/);
  assert.match(result.body, /class="balancer-card state-degraded"/);
  assert.match(result.body, /class="node-card state-healthy"/);
  assert.match(result.body, /Last Check/);
  assert.match(result.body, />42 ms</);
  assert.match(result.body, /Remote Ping/);
  assert.match(result.body, />failed</);
  assert.doesNotMatch(result.body, /HTTP 204/);
  assert.doesNotMatch(result.body, /Last success/);
  assert.match(result.body, /Balancer/);
  assert.match(result.body, /class="nodes-grid"/);
  assert.ok(
    result.body.indexOf('class="balancer-card state-degraded"') < result.body.indexOf('class="nodes-grid"'),
    'expected balancer card to render before tunnel grid',
  );
  assert.doesNotMatch(result.body, /Current runtime state \(JSON\)/);
  assert.doesNotMatch(result.body, /\/api\/runtime-state/);
  assert.doesNotMatch(result.body, /<table>/);
});

test('status server renders degraded current error separately from last success and balancer status', async () => {
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('sub-1', '127.0.0.1:8080');
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  const tunnel = createTunnel({
    baseTunnelId: 'alpha',
    displayName: 'Alpha',
    outboundTagInitial: 'x-observe-out-a',
    outboundTagCurrent: 'out-a',
  });
  targetState.topology = { tunnels: [tunnel] } as TargetTopology;
  targetState.tunnels = {
    alpha: {
      tunnel,
      monitor: {
        state: 'degraded',
        consecutiveFailures: 1,
        lastError: 'probe failed',
        lastSuccessStatusCode: 204,
        lastSuccessLatencyMs: 42,
      },
    },
  };

  const result = await handleStatusServerRequest('/status', createLoadedConfig(), memoryState);

  assert.equal(result.statusCode, 200);
  assert.match(result.body, /class="node-card state-degraded"/);
  assert.match(result.body, /Error: probe failed/);
  assert.doesNotMatch(result.body, /HTTP 204/);
  assert.match(result.body, />removed</);
});

test('status server renders configured target and balancer card even without tunnel rows', async () => {
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('sub-1', '127.0.0.1:8080');
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  targetState.balancerMonitor = {
    state: 'healthy',
    remotePingState: 'ok',
    consecutiveFailures: 0,
    lastStatusCode: 204,
    lastLatencyMs: 17,
    remotePingLastStatusCode: 200,
  };

  const result = await handleStatusServerRequest('/status', createLoadedConfig(), memoryState);

  assert.equal(result.statusCode, 200);
  assert.match(result.body, /127\.0\.0\.1:8080/);
  assert.match(result.body, /class="balancer-card state-healthy"/);
  assert.match(result.body, /No tunnel data\./);
  assert.match(result.body, />17 ms</);
  assert.match(result.body, />ok</);
  assert.doesNotMatch(result.body, /HTTP 204/);
});

test('status server does not render disabled subscriptions from config', async () => {
  const result = await handleStatusServerRequest('/status', createLoadedConfigWithDisabledSubscription(), createSyncMemoryState());

  assert.equal(result.statusCode, 200);
  assert.match(result.body, /sub-1/);
  assert.doesNotMatch(result.body, /sub-disabled/);
  assert.doesNotMatch(result.body, /127\.0\.0\.1:9999/);
});

test('status server keeps api status wire format as flat tunnels array', async () => {
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('sub-1', '127.0.0.1:8080');
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  const tunnel = createTunnel({
    baseTunnelId: 'alpha',
    displayName: 'Alpha',
  });
  targetState.topology = { tunnels: [tunnel] } as TargetTopology;
  targetState.tunnels = {
    alpha: {
      tunnel,
      monitor: {
        state: 'idle',
        consecutiveFailures: 0,
      },
    },
  };

  const result = await handleStatusServerRequest('/api/status', createLoadedConfig(), memoryState);
  const payload = JSON.parse(result.body) as { tunnels?: unknown[]; subscriptions?: unknown };

  assert.equal(result.statusCode, 200);
  assert.ok(Array.isArray(payload.tunnels));
  assert.equal(payload.tunnels.length, 1);
  assert.equal(payload.subscriptions, undefined);
});

test('status server api status exposes monitor history and balancer participation fields', async () => {
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('sub-1', '127.0.0.1:8080');
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  const tunnel = createTunnel({
    baseTunnelId: 'alpha',
    displayName: 'Alpha',
    outboundTagInitial: 'x-observe-out-a',
    outboundTagCurrent: 'out-a',
  });
  targetState.topology = { tunnels: [tunnel] } as TargetTopology;
  targetState.tunnels = {
    alpha: {
      tunnel,
      monitor: {
        state: 'degraded',
        consecutiveFailures: 1,
        lastError: 'probe failed',
        lastCheckedAt: '2026-01-01T00:00:00.000Z',
        lastFailureAt: '2026-01-01T00:00:00.000Z',
        lastSuccessAt: '2025-12-31T23:59:00.000Z',
        lastSuccessStatusCode: 204,
        lastSuccessLatencyMs: 42,
      },
    },
  };
  targetState.balancerMonitor.remotePingState = 'failed';
  targetState.balancerMonitor.remotePingLastError = 'push failed';

  const result = await handleStatusServerRequest('/api/status', createLoadedConfig(), memoryState);
  const payload = JSON.parse(result.body) as {
    tunnels: Array<{
      lastHttpStatus?: number;
      lastLatencyMs?: number;
      lastError?: string;
      lastSuccessHttpStatus?: number;
      lastSuccessLatencyMs?: number;
      balanced?: boolean;
      balancerMonitorRemotePingState?: string;
      balancerMonitorRemotePingLastError?: string;
    }>;
  };

  assert.equal(payload.tunnels[0]?.lastHttpStatus, undefined);
  assert.equal(payload.tunnels[0]?.lastLatencyMs, undefined);
  assert.equal(payload.tunnels[0]?.lastError, 'probe failed');
  assert.equal(payload.tunnels[0]?.lastSuccessHttpStatus, 204);
  assert.equal(payload.tunnels[0]?.lastSuccessLatencyMs, 42);
  assert.equal(payload.tunnels[0]?.balanced, false);
  assert.equal(payload.tunnels[0]?.balancerMonitorRemotePingState, 'failed');
  assert.equal(payload.tunnels[0]?.balancerMonitorRemotePingLastError, 'push failed');
});

test('status server returns 400 for runtime-state endpoint without required query params', async () => {
  const result = await handleStatusServerRequest('/api/runtime-state', createLoadedConfig(), createSyncMemoryState());

  assert.equal(result.statusCode, 400);
  const payload = JSON.parse(result.body) as { error: string };
  assert.match(payload.error, /subscriptionId and targetAddress/);
});

test('status server returns 404 when runtime-state target is missing', async () => {
  const result = await handleStatusServerRequest(
    '/api/runtime-state?subscriptionId=sub-1&targetAddress=127.0.0.1%3A9999',
    createLoadedConfig(),
    createSyncMemoryState(),
  );

  assert.equal(result.statusCode, 404);
  const payload = JSON.parse(result.body) as { error: string };
  assert.match(payload.error, /Target not found/);
});

test('status server returns 502 when runtime-state builder hits Xray API error', async () => {
  const result = await handleStatusServerRequest(
    '/api/runtime-state?subscriptionId=sub-1&targetAddress=127.0.0.1%3A8080',
    createLoadedConfig(),
    createSyncMemoryState(),
    {
      buildCurrentRuntimeStateSnapshotFn: async () => {
        throw new ApiRequestError('Xray API unavailable');
      },
    },
  );

  assert.equal(result.statusCode, 502);
  const payload = JSON.parse(result.body) as { error: string };
  assert.match(payload.error, /Xray API unavailable/);
});

test('status server runtime-state endpoint uses redacted builder output', async () => {
  const loadedConfig = createLoadedConfig();
  const result = await handleStatusServerRequest(
    '/api/runtime-state?subscriptionId=sub-1&targetAddress=127.0.0.1%3A8080',
    loadedConfig,
    createSyncMemoryState(),
    {
      buildCurrentRuntimeStateSnapshotFn: async (_loadedConfig, subscriptionId, targetAddress, _dependencies, options) => ({
        subscriptionId,
        targetAddress,
        capturedAt: '2026-03-21T00:00:00.000Z',
        config: {
          subscription: {
            ...loadedConfig.config.subscriptions[0]!,
            input: options?.includeSecrets ? 'https://example.test/token' : 'https://example.test/<redacted>',
          },
          target: loadedConfig.config.subscriptions[0]!.target,
          resources: loadedConfig.config.resources,
        },
        serviceState: {
          balancerMonitor: {
            state: 'healthy',
            remotePingState: 'idle',
            consecutiveFailures: 0,
            lastStatusCode: 204,
          },
        },
        expected: {
          source: '/tmp/subscription.txt',
          outbounds: [],
          inbounds: [],
          routingRules: [],
        },
        runtime: {
          outbounds: [{ tag: 'tag-1', classification: 'unmanaged', parsed: { protocol: 'vless' } }],
          inbounds: [],
          routingRules: [],
        },
        diff: {
          outbounds: { matched: [], matchedFallback: [], missing: [], unexpected: [] },
          inbounds: { matched: [], missing: [], unexpected: [] },
          routingRules: { matched: [], missing: [], unexpected: [] },
        },
      }),
    },
  );

  assert.equal(result.statusCode, 200);
  const payload = JSON.parse(result.body) as { config: { subscription: { input: string } }; runtime: { outbounds: Array<{ rawBase64?: string }> } };
  assert.equal(payload.config.subscription.input, 'https://example.test/<redacted>');
  assert.equal(payload.runtime.outbounds[0]?.rawBase64, undefined);
  assert.equal((payload as { serviceState?: { balancerMonitor?: { state?: string } } }).serviceState?.balancerMonitor?.state, 'healthy');
});

import assert from 'node:assert/strict';
import test from 'node:test';

import { buildManifest } from '../src/manifest.ts';
import { buildRemotePingUrl, pushRemotePingDirect, runTargetBalancerMonitorTick, runTargetMonitorTick } from '../src/runtime/monitoring.ts';
import {
  buildTargetStateKey,
  createSyncMemoryState,
  getOrCreateTargetState,
  replaceTargetTopology,
  withTargetMutationLock,
} from '../src/runtime/run-state.ts';
import { buildTargetTopology } from '../src/topology/build-tunnel-topology.ts';
import type { SubscriptionTargetConfig } from '../src/types.ts';

function createTarget(overrides: Partial<SubscriptionTargetConfig> = {}): SubscriptionTargetConfig {
  return {
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
      enabled: true,
      schedule: '*/2 * * * *',
      maxParallel: 10,
      request: {
        url: 'https://example.test/health',
        method: 'GET',
        expectedStatus: 200,
        timeoutMs: 5000,
      },
    },
    balancerMonitor: {
      enabled: false,
    },
    ...overrides,
  };
}

function wait(ms: number): Promise<void> {
  return new Promise((resolve) => {
    setTimeout(resolve, ms);
  });
}

test('runTargetMonitorTick starts tunnel probes in parallel and serializes repairs via target mutex', async () => {
  const manifest = buildManifest(
    [
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf6@example.com:443?type=tcp&security=tls#🇧🇪 Брюссель, Бельгия, Extra',
    ].join('\n'),
    'inline',
  );
  const target = createTarget();
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('source-1', target.address);
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  replaceTargetTopology(targetState, buildTargetTopology(manifest, target), {
    outbound: 'hash-a',
    inbound: 'hash-b',
    routing: 'hash-c',
  });

  let probeStarted = 0;
  let probeInFlight = 0;
  let probeMaxConcurrent = 0;
  let releaseProbe!: () => void;
  const firstProbeGate = new Promise<void>((resolve) => {
    releaseProbe = resolve;
  });

  let repairInFlight = 0;
  let repairMaxConcurrent = 0;

  const tickPromise = runTargetMonitorTick('source-1', target, memoryState, {
    info() {},
    error() {},
    warn() {},
  } as never, {
    requestViaSocksFn: async () => {
      probeStarted += 1;
      probeInFlight += 1;
      probeMaxConcurrent = Math.max(probeMaxConcurrent, probeInFlight);

      if (probeStarted === 1) {
        await firstProbeGate;
      } else {
        releaseProbe();
      }

      probeInFlight -= 1;
      throw new Error('probe failed');
    },
    repairTunnelFn: async (subscriptionId, runtimeTarget, state, runtimeState) => {
      await withTargetMutationLock(state, buildTargetStateKey(subscriptionId, runtimeTarget.address), async () => {
        repairInFlight += 1;
        repairMaxConcurrent = Math.max(repairMaxConcurrent, repairInFlight);
        await wait(10);
        repairInFlight -= 1;
        const currentTargetState = getOrCreateTargetState(state, buildTargetStateKey(subscriptionId, runtimeTarget.address));
        currentTargetState.tunnels[runtimeState.tunnel.baseTunnelId]!.monitor.state = 'degraded';
      });
    },
  });

  await tickPromise;

  assert.equal(probeStarted, 2);
  assert.equal(probeMaxConcurrent, 2);
  assert.equal(repairMaxConcurrent, 1);
});

test('runTargetMonitorTick respects monitor.maxParallel and does not let one business failure block healthy probes', async () => {
  const manifest = buildManifest(
    [
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf6@example.com:443?type=tcp&security=tls#🇧🇪 Брюссель, Бельгия, Extra',
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf7@example.com:443?type=tcp&security=tls#🇧🇬 София, Болгария, Extra',
    ].join('\n'),
    'inline',
  );
  const target = createTarget({
    monitor: {
      enabled: true,
      schedule: '*/2 * * * *',
      maxParallel: 2,
      request: {
        url: 'https://example.test/health',
        method: 'GET',
        expectedStatus: 200,
        timeoutMs: 5000,
      },
    },
  });
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('source-1', target.address);
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  replaceTargetTopology(targetState, buildTargetTopology(manifest, target), {
    outbound: 'hash-a',
    inbound: 'hash-b',
    routing: 'hash-c',
  });

  let inFlight = 0;
  let maxConcurrent = 0;
  const repaired: string[] = [];

  await runTargetMonitorTick('source-1', target, memoryState, {
    info() {},
    error() {},
    warn() {},
  } as never, {
    requestViaSocksFn: async ({ proxyPort }) => {
      inFlight += 1;
      maxConcurrent = Math.max(maxConcurrent, inFlight);
      await wait(10);
      inFlight -= 1;

      if (proxyPort === 20000) {
        throw new Error('probe failed');
      }

      return {
        statusCode: 200,
        latencyMs: 25,
        bodyBytes: 100,
      };
    },
    repairTunnelFn: async (_subscriptionId, _runtimeTarget, _state, runtimeState) => {
      repaired.push(runtimeState.tunnel.baseTunnelId);
    },
  });

  assert.equal(maxConcurrent, 2);
  assert.equal(repaired.length, 1);
  assert.equal(targetState.tunnels[Object.keys(targetState.tunnels)[0]!]!.monitor.state, 'degraded');
  assert.ok(Object.values(targetState.tunnels).some((item) => item.monitor.state === 'healthy'));
});

test('runTargetMonitorTick clears current check on failure and preserves last successful check', async () => {
  const manifest = buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
    'inline',
  );
  const target = createTarget();
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('source-1', target.address);
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  replaceTargetTopology(targetState, buildTargetTopology(manifest, target), {
    outbound: 'hash-a',
    inbound: 'hash-b',
    routing: 'hash-c',
  });

  await runTargetMonitorTick('source-1', target, memoryState, {
    info() {},
    error() {},
    warn() {},
  } as never, {
    requestViaSocksFn: async () => ({
      statusCode: 200,
      latencyMs: 25,
      bodyBytes: 100,
    }),
  });

  const runtimeState = Object.values(targetState.tunnels)[0]!;
  assert.equal(runtimeState.monitor.state, 'healthy');
  assert.equal(runtimeState.monitor.lastStatusCode, 200);
  assert.equal(runtimeState.monitor.lastLatencyMs, 25);
  assert.equal(runtimeState.monitor.lastSuccessStatusCode, 200);
  assert.equal(runtimeState.monitor.lastSuccessLatencyMs, 25);

  await runTargetMonitorTick('source-1', target, memoryState, {
    info() {},
    error() {},
    warn() {},
  } as never, {
    requestViaSocksFn: async () => {
      throw new Error('probe failed');
    },
    repairTunnelFn: async () => undefined,
  });

  assert.equal(runtimeState.monitor.state, 'degraded');
  assert.equal(runtimeState.monitor.lastStatusCode, undefined);
  assert.equal(runtimeState.monitor.lastLatencyMs, undefined);
  assert.equal(runtimeState.monitor.lastSuccessStatusCode, 200);
  assert.equal(runtimeState.monitor.lastSuccessLatencyMs, 25);
  assert.match(runtimeState.monitor.lastError ?? '', /probe failed/);
});

test('runTargetMonitorTick treats unexpected status as failure without stale current success', async () => {
  const manifest = buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
    'inline',
  );
  const target = createTarget();
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('source-1', target.address);
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  replaceTargetTopology(targetState, buildTargetTopology(manifest, target), {
    outbound: 'hash-a',
    inbound: 'hash-b',
    routing: 'hash-c',
  });

  await runTargetMonitorTick('source-1', target, memoryState, {
    info() {},
    error() {},
    warn() {},
  } as never, {
    requestViaSocksFn: async () => ({
      statusCode: 204,
      latencyMs: 31,
      bodyBytes: 0,
    }),
    repairTunnelFn: async () => undefined,
  });

  const runtimeState = Object.values(targetState.tunnels)[0]!;
  assert.equal(runtimeState.monitor.state, 'degraded');
  assert.equal(runtimeState.monitor.lastStatusCode, undefined);
  assert.equal(runtimeState.monitor.lastLatencyMs, undefined);
  assert.equal(runtimeState.monitor.lastSuccessStatusCode, undefined);
  assert.match(runtimeState.monitor.lastError ?? '', /Expected HTTP 200, got 204/);
});

test('runTargetMonitorTick propagates unexpected exceptions after probe settlement', async () => {
  const manifest = buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
    'inline',
  );
  const target = createTarget();
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('source-1', target.address);
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  replaceTargetTopology(targetState, buildTargetTopology(manifest, target), {
    outbound: 'hash-a',
    inbound: 'hash-b',
    routing: 'hash-c',
  });

  await assert.rejects(
    () =>
      runTargetMonitorTick('source-1', target, memoryState, {
        info() {},
        error() {},
        warn() {},
      } as never, {
        requestViaSocksFn: async () => {
          throw new Error('probe failed');
        },
        repairTunnelFn: async () => {
          throw new Error('unexpected repair exception');
        },
      }),
    /Concurrent task execution failed/,
  );
});

test('runTargetBalancerMonitorTick performs main request via configured external socks', async () => {
  const target = createTarget({
    balancerMonitor: {
      enabled: true,
      schedule: '*/2 * * * *',
      socks5: {
        host: '127.0.0.10',
        port: 1080,
      },
      request: {
        url: 'https://example.test/health',
        method: 'GET',
        expectedStatus: 200,
        timeoutMs: 5000,
      },
    },
  });
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('source-1', target.address);
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  replaceTargetTopology(targetState, buildTargetTopology(buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
    'inline',
  ), target), {
    outbound: 'hash-a',
    inbound: 'hash-b',
    routing: 'hash-c',
  });

  const calls: Array<{ host: string; port: number; url: string; method: string }> = [];

  await runTargetBalancerMonitorTick('source-1', target, memoryState, {
    info() {},
    warn() {},
    error() {},
  } as never, {
    requestViaSocksFn: async ({ proxyHost, proxyPort, url, method }) => {
      calls.push({ host: proxyHost, port: proxyPort, url, method });
      return {
        statusCode: 200,
        latencyMs: 33,
        bodyBytes: 128,
      };
    },
  });

  assert.deepEqual(calls, [
    { host: '127.0.0.10', port: 1080, url: 'https://example.test/health', method: 'GET' },
  ]);
  assert.equal(targetState.balancerMonitor.state, 'healthy');
  assert.equal(targetState.balancerMonitor.lastStatusCode, 200);
  assert.equal(targetState.balancerMonitor.lastSuccessStatusCode, 200);
  assert.equal(targetState.balancerMonitor.lastSuccessLatencyMs, 33);
});

test('runTargetBalancerMonitorTick marks target degraded on failed main check without stale current success', async () => {
  const target = createTarget({
    balancerMonitor: {
      enabled: true,
      schedule: '*/2 * * * *',
      socks5: {
        host: '127.0.0.10',
        port: 1080,
      },
      request: {
        url: 'https://example.test/health',
        method: 'GET',
        expectedStatus: 204,
        timeoutMs: 5000,
      },
    },
  });
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('source-1', target.address);
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  replaceTargetTopology(targetState, buildTargetTopology(buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
    'inline',
  ), target), {
    outbound: 'hash-a',
    inbound: 'hash-b',
    routing: 'hash-c',
  });

  const calls: string[] = [];
  targetState.balancerMonitor = {
    state: 'healthy',
    remotePingState: 'idle',
    consecutiveFailures: 0,
    lastStatusCode: 204,
    lastLatencyMs: 12,
    lastSuccessStatusCode: 204,
    lastSuccessLatencyMs: 12,
  };

  await runTargetBalancerMonitorTick('source-1', target, memoryState, {
    info() {},
    warn() {},
    error() {},
  } as never, {
    requestViaSocksFn: async ({ url }) => {
      calls.push(url);
      return {
        statusCode: 200,
        latencyMs: 40,
        bodyBytes: 10,
      };
    },
  });

  assert.deepEqual(calls, ['https://example.test/health']);
  assert.equal(targetState.balancerMonitor.state, 'degraded');
  assert.equal(targetState.balancerMonitor.lastStatusCode, undefined);
  assert.equal(targetState.balancerMonitor.lastLatencyMs, undefined);
  assert.equal(targetState.balancerMonitor.lastSuccessStatusCode, 204);
  assert.equal(targetState.balancerMonitor.lastSuccessLatencyMs, 12);
  assert.match(targetState.balancerMonitor.lastError ?? '', /Expected HTTP 204, got 200/);
});

test('runTargetBalancerMonitorTick starts remote ping asynchronously after successful primary check', async () => {
  const target = createTarget({
    balancerMonitor: {
      enabled: true,
      schedule: '*/2 * * * *',
      socks5: {
        host: '127.0.0.10',
        port: 1080,
      },
      request: {
        url: 'https://example.test/health',
        method: 'GET',
        expectedStatus: 200,
        timeoutMs: 5000,
      },
      remotePing: {
        enabled: true,
        url: 'https://kuma.example.test/api/push/token?keep=1',
        timeoutMs: 5000,
        viaSocks: false,
      },
    },
  });
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('source-1', target.address);
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  replaceTargetTopology(targetState, buildTargetTopology(buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
    'inline',
  ), target), {
    outbound: 'hash-a',
    inbound: 'hash-b',
    routing: 'hash-c',
  });

  let releaseRemotePing!: () => void;
  const remotePingGate = new Promise<void>((resolve) => {
    releaseRemotePing = resolve;
  });
  const remoteCalls: Array<{ url: string }> = [];
  let socksCallCount = 0;

  await runTargetBalancerMonitorTick('source-1', target, memoryState, {
    info() {},
    warn() {},
    error() {},
  } as never, {
    requestViaSocksFn: async () => {
      socksCallCount += 1;
      return {
        statusCode: 200,
        latencyMs: 33,
        bodyBytes: 128,
      };
    },
    directRemotePingFn: async ({ url }) => {
      remoteCalls.push({ url });
      await remotePingGate;
      return { statusCode: 200 };
    },
  });

  assert.equal(targetState.balancerMonitor.state, 'healthy');
  assert.equal(targetState.balancerMonitor.remotePingState, 'pending');
  assert.deepEqual(remoteCalls, [
    { url: 'https://kuma.example.test/api/push/token?keep=1&status=up&msg=OK&ping=33' },
  ]);
  assert.equal(socksCallCount, 1);

  releaseRemotePing();
  await wait(0);

  assert.equal(targetState.balancerMonitor.remotePingState, 'ok');
  assert.equal(targetState.balancerMonitor.remotePingLastStatusCode, 200);
});

test('runTargetBalancerMonitorTick starts remote ping down after primary failure without changing health state on push failure', async () => {
  const target = createTarget({
    balancerMonitor: {
      enabled: true,
      schedule: '*/2 * * * *',
      socks5: {
        host: '127.0.0.10',
        port: 1080,
      },
      request: {
        url: 'https://example.test/health',
        method: 'GET',
        expectedStatus: 204,
        timeoutMs: 5000,
      },
      remotePing: {
        enabled: true,
        url: 'https://kuma.example.test/api/push/token',
        timeoutMs: 5000,
        viaSocks: false,
      },
    },
  });
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('source-1', target.address);
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  replaceTargetTopology(targetState, buildTargetTopology(buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
    'inline',
  ), target), {
    outbound: 'hash-a',
    inbound: 'hash-b',
    routing: 'hash-c',
  });

  const remoteCalls: Array<{ url: string }> = [];

  await runTargetBalancerMonitorTick('source-1', target, memoryState, {
    info() {},
    warn() {},
    error() {},
  } as never, {
    requestViaSocksFn: async () => ({
      statusCode: 200,
      latencyMs: 40,
      bodyBytes: 10,
    }),
    directRemotePingFn: async ({ url }) => {
      remoteCalls.push({ url });
      return { statusCode: 500 };
    },
  });
  await wait(0);

  assert.equal(targetState.balancerMonitor.state, 'degraded');
  assert.equal(targetState.balancerMonitor.remotePingState, 'failed');
  assert.equal(targetState.balancerMonitor.remotePingLastStatusCode, 500);
  assert.match(targetState.balancerMonitor.remotePingLastError ?? '', /HTTP 500/);
  assert.deepEqual(remoteCalls, [
    { url: 'https://kuma.example.test/api/push/token?status=down&msg=Expected+HTTP+204%2C+got+200.' },
  ]);
});

test('runTargetBalancerMonitorTick skips overlapping remote ping pushes', async () => {
  const target = createTarget({
    balancerMonitor: {
      enabled: true,
      schedule: '*/2 * * * *',
      socks5: {
        host: '127.0.0.10',
        port: 1080,
      },
      request: {
        url: 'https://example.test/health',
        method: 'GET',
        expectedStatus: 200,
        timeoutMs: 5000,
      },
      remotePing: {
        enabled: true,
        url: 'https://kuma.example.test/api/push/token',
        timeoutMs: 5000,
        viaSocks: false,
      },
    },
  });
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('source-1', target.address);
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  replaceTargetTopology(targetState, buildTargetTopology(buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
    'inline',
  ), target), {
    outbound: 'hash-a',
    inbound: 'hash-b',
    routing: 'hash-c',
  });
  targetState.balancerMonitor.remotePingState = 'pending';

  const events: string[] = [];
  let remoteCallCount = 0;

  await runTargetBalancerMonitorTick('source-1', target, memoryState, {
    info() {},
    warn(payload: { event?: string }) {
      if (payload.event) {
        events.push(payload.event);
      }
    },
    error() {},
  } as never, {
    requestViaSocksFn: async () => ({
      statusCode: 200,
      latencyMs: 33,
      bodyBytes: 128,
    }),
    directRemotePingFn: async () => {
      remoteCallCount += 1;
      return { statusCode: 200 };
    },
  });

  assert.equal(remoteCallCount, 0);
  assert.ok(events.includes('balancer_monitor_remote_ping_skipped_overrun'));
});

test('runTargetBalancerMonitorTick sends remote ping through balancer socks when viaSocks is enabled', async () => {
  const target = createTarget({
    balancerMonitor: {
      enabled: true,
      schedule: '*/2 * * * *',
      socks5: {
        host: '127.0.0.10',
        port: 1080,
      },
      request: {
        url: 'https://example.test/health',
        method: 'GET',
        expectedStatus: 200,
        timeoutMs: 5000,
      },
      remotePing: {
        enabled: true,
        url: 'https://kuma.example.test/api/push/token',
        timeoutMs: 3000,
        viaSocks: true,
      },
    },
  });
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('source-1', target.address);
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  replaceTargetTopology(targetState, buildTargetTopology(buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
    'inline',
  ), target), {
    outbound: 'hash-a',
    inbound: 'hash-b',
    routing: 'hash-c',
  });

  const calls: Array<{ proxyHost: string; proxyPort: number; url: string; method: string; timeoutMs: number }> = [];

  await runTargetBalancerMonitorTick('source-1', target, memoryState, {
    info() {},
    warn() {},
    error() {},
  } as never, {
    requestViaSocksFn: async (config) => {
      calls.push(config);
      return calls.length === 1
        ? {
            statusCode: 200,
            latencyMs: 33,
            bodyBytes: 128,
          }
        : {
            statusCode: 502,
            latencyMs: 12,
            bodyBytes: 0,
          };
    },
    directRemotePingFn: async () => {
      throw new Error('direct remote ping must not be called');
    },
  });
  await wait(0);

  assert.equal(calls.length, 2);
  assert.deepEqual(calls[1], {
    proxyHost: '127.0.0.10',
    proxyPort: 1080,
    url: 'https://kuma.example.test/api/push/token?status=up&msg=OK&ping=33',
    method: 'GET',
    timeoutMs: 3000,
  });
  assert.equal(targetState.balancerMonitor.state, 'healthy');
  assert.equal(targetState.balancerMonitor.remotePingState, 'failed');
  assert.equal(targetState.balancerMonitor.remotePingLastStatusCode, 502);
});

test('pushRemotePingDirect records status and cancels response body without downloading it', async () => {
  const originalFetch = globalThis.fetch;
  let cancelCalled = false;
  const calls: Array<{ url: string; method?: string }> = [];

  globalThis.fetch = (async (input: string | URL | Request, init?: RequestInit) => {
    calls.push({ url: String(input), method: init?.method });
    return {
      status: 204,
      body: {
        cancel: async () => {
          cancelCalled = true;
        },
      },
      arrayBuffer: async () => {
        throw new Error('response body must not be downloaded');
      },
    } as unknown as Response;
  }) as typeof fetch;

  try {
    const response = await pushRemotePingDirect({
      url: 'https://kuma.example.test/api/push/token?status=up',
      timeoutMs: 5000,
    });

    assert.equal(response.statusCode, 204);
    assert.equal(cancelCalled, true);
    assert.deepEqual(calls, [
      { url: 'https://kuma.example.test/api/push/token?status=up', method: 'GET' },
    ]);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('buildRemotePingUrl preserves existing query params and overwrites push params', () => {
  assert.equal(
    buildRemotePingUrl('https://kuma.example.test/api/push/token?keep=1&status=down&msg=old&ping=999', {
      status: 'up',
      msg: 'OK',
      pingMs: 42,
    }),
    'https://kuma.example.test/api/push/token?keep=1&status=up&msg=OK&ping=42',
  );
  assert.equal(
    buildRemotePingUrl('https://kuma.example.test/api/push/token?keep=1&ping=999', {
      status: 'down',
      msg: 'failed',
    }),
    'https://kuma.example.test/api/push/token?keep=1&status=down&msg=failed',
  );
});

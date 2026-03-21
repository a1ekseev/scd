import assert from 'node:assert/strict';
import test from 'node:test';

import { buildManifest } from '../src/manifest.ts';
import { runTargetBalancerMonitorTick, runTargetMonitorTick, runTargetSpeedtestTick } from '../src/runtime/monitoring.ts';
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
    speedtest: {
      enabled: true,
      schedule: '*/10 * * * *',
      urls: ['https://example.test/10mb.bin'],
      method: 'GET',
      timeoutMs: 15000,
      maxParallel: 3,
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

test('runTargetSpeedtestTick respects maxParallel and keeps URL fallback sequential per tunnel', async () => {
  const manifest = buildManifest(
    [
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf6@example.com:443?type=tcp&security=tls#🇧🇪 Брюссель, Бельгия, Extra',
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf7@example.com:443?type=tcp&security=tls#🇧🇬 София, Болгария, Extra',
    ].join('\n'),
    'inline',
  );
  const target = createTarget({
    speedtest: {
      enabled: true,
      schedule: '*/10 * * * *',
      urls: ['https://example.test/primary.bin', 'https://example.test/fallback.bin'],
      method: 'GET',
      timeoutMs: 15000,
      maxParallel: 2,
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
  const calls: Array<{ tunnelId: string; url: string }> = [];

  await runTargetSpeedtestTick('source-1', target, memoryState, {
    requestViaSocksFn: async ({ proxyPort, url }) => {
      const tunnelId = `port-${proxyPort}`;
      calls.push({ tunnelId, url });
      inFlight += 1;
      maxConcurrent = Math.max(maxConcurrent, inFlight);
      await wait(10);
      inFlight -= 1;

      if (proxyPort === 20000 && url.includes('primary')) {
        throw new Error('primary failed');
      }

      return {
        statusCode: 200,
        latencyMs: 25,
        bodyBytes: 1_000_000,
      };
    },
  });

  assert.equal(maxConcurrent, 2);
  assert.deepEqual(
    calls.filter((item) => item.tunnelId === 'port-20000').map((item) => item.url),
    ['https://example.test/primary.bin', 'https://example.test/fallback.bin'],
  );
  assert.deepEqual(
    Array.from(new Set(calls.map((item) => item.tunnelId))).sort(),
    ['port-20000', 'port-20001', 'port-20002'],
  );
  assert.ok(calls.some((item) => item.tunnelId === 'port-20001'));
  assert.ok(calls.some((item) => item.tunnelId === 'port-20002'));
});

test('runTargetSpeedtestTick propagates unexpected worker exceptions', async () => {
  const manifest = buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
    'inline',
  );
  const target = createTarget({
    speedtest: {
      enabled: true,
      schedule: '*/10 * * * *',
      urls: ['https://example.test/primary.bin'],
      method: 'GET',
      timeoutMs: 15000,
      maxParallel: 1,
    },
  });
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('source-1', target.address);
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  const topology = buildTargetTopology(manifest, target);
  replaceTargetTopology(targetState, topology, {
    outbound: 'hash-a',
    inbound: 'hash-b',
    routing: 'hash-c',
  });
  const tunnelId = topology.tunnels[0]!.baseTunnelId;
  Object.defineProperty(targetState.tunnels[tunnelId]!, 'speedtest', {
    configurable: true,
    get() {
      throw new Error('unexpected speedtest state failure');
    },
  });

  await assert.rejects(
    () =>
      runTargetSpeedtestTick('source-1', target, memoryState, {
        requestViaSocksFn: async () => {
          return {
            statusCode: 200,
            latencyMs: 10,
            bodyBytes: 100,
          };
        },
      }),
    /Concurrent task execution failed/,
  );
});

test('runTargetBalancerMonitorTick performs main request via configured external socks and runs optional successGet best-effort', async () => {
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
      successGet: {
        url: 'https://example.test/ping.txt',
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
      if (url.endsWith('ping.txt')) {
        throw new Error('success get failed');
      }

      return {
        statusCode: 200,
        latencyMs: 33,
        bodyBytes: 128,
      };
    },
  });

  assert.deepEqual(calls, [
    { host: '127.0.0.10', port: 1080, url: 'https://example.test/health', method: 'GET' },
    { host: '127.0.0.10', port: 1080, url: 'https://example.test/ping.txt', method: 'GET' },
  ]);
  assert.equal(targetState.balancerMonitor.state, 'healthy');
  assert.equal(targetState.balancerMonitor.lastStatusCode, 200);
  assert.equal(targetState.balancerMonitor.successGetLastError, 'success get failed');
});

test('runTargetBalancerMonitorTick marks target degraded on failed main check and skips successGet', async () => {
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
      successGet: {
        url: 'https://example.test/ping.txt',
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

  const calls: string[] = [];
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
  assert.match(targetState.balancerMonitor.lastError ?? '', /Expected HTTP 204, got 200/);
});

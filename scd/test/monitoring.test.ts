import assert from 'node:assert/strict';
import test from 'node:test';

import { buildOutboundGrpc } from '../src/builders/build-outbound-grpc.ts';
import { buildRoutingGrpc } from '../src/builders/build-routing-grpc.ts';
import { buildManifest } from '../src/manifest.ts';
import { buildRemotePingUrl, pushRemotePingDirect, runTargetBalancerMonitorTick, runTargetMonitorTick } from '../src/runtime/monitoring.ts';
import {
  buildTargetStateKey,
  buildStatusSnapshot,
  createSyncMemoryState,
  getOrCreateTargetState,
  replaceTargetTopology,
  withTargetMutationLock,
} from '../src/runtime/run-state.ts';
import { buildTargetTopology } from '../src/topology/build-tunnel-topology.ts';
import type { SubscriptionTargetConfig, TunnelMapping } from '../src/types.ts';

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

function sameBytes(left: Uint8Array, right: Uint8Array): boolean {
  return left.length === right.length && left.every((value, index) => value === right[index]);
}

function createRepairedTunnel(tunnel: TunnelMapping): TunnelMapping {
  return {
    ...tunnel,
    outboundTagCurrent: tunnel.outboundWithoutPrefix.tag,
  };
}

function createRejoinedTunnel(tunnel: TunnelMapping): TunnelMapping {
  return {
    ...tunnel,
    outboundTagCurrent: tunnel.outboundTagInitial,
  };
}

function createRecordingMutationClient(tunnel: TunnelMapping, options: {
  failFirstAddRule?: boolean;
  failEveryRollbackAddRule?: boolean;
} = {}): {
  calls: string[];
  client: {
    addOutbound(raw: Uint8Array): Promise<void>;
    removeOutbound(tag: string): Promise<void>;
    addRule(raw: Uint8Array): Promise<void>;
    removeRule(ruleTag: string): Promise<void>;
  };
} {
  const repairedTunnel = createRepairedTunnel(tunnel);
  const rejoinedTunnel = createRejoinedTunnel(tunnel);
  const prefixedOutboundRaw = buildOutboundGrpc(rejoinedTunnel.outboundInitial.normalized).raw;
  const unprefixedOutboundRaw = buildOutboundGrpc(repairedTunnel.outboundWithoutPrefix.normalized).raw;
  const prefixedRuleRaw = buildRoutingGrpc(rejoinedTunnel).raw;
  const unprefixedRuleRaw = buildRoutingGrpc(repairedTunnel).raw;
  const calls: string[] = [];
  let addRuleCalls = 0;

  return {
    calls,
    client: {
      async addOutbound(raw) {
        const tag = sameBytes(raw, prefixedOutboundRaw)
          ? rejoinedTunnel.outboundTagCurrent
          : sameBytes(raw, unprefixedOutboundRaw)
            ? repairedTunnel.outboundTagCurrent
            : '<unknown>';
        calls.push(`addOutbound:${tag}`);
      },
      async removeOutbound(tag) {
        calls.push(`removeOutbound:${tag}`);
      },
      async addRule(raw) {
        addRuleCalls += 1;
        const rule = sameBytes(raw, prefixedRuleRaw)
          ? `${rejoinedTunnel.routeTag}->${rejoinedTunnel.outboundTagCurrent}`
          : sameBytes(raw, unprefixedRuleRaw)
            ? `${repairedTunnel.routeTag}->${repairedTunnel.outboundTagCurrent}`
            : '<unknown>';
        calls.push(`addRule:${rule}`);

        if (options.failFirstAddRule && addRuleCalls === 1) {
          throw new Error('addRule prefixed failed');
        }
        if (options.failEveryRollbackAddRule && addRuleCalls > 1) {
          throw new Error('rollback addRule failed');
        }
      },
      async removeRule(ruleTag) {
        calls.push(`removeRule:${ruleTag}`);
      },
    },
  };
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

test('runTargetMonitorTick real repair path uses injected mutation client and switches to unprefixed outbound', async () => {
  const manifest = buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
    'inline',
  );
  const target = createTarget({
    observatorySubjectSelectorPrefix: 'x-observe-',
  });
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('source-1', target.address);
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  replaceTargetTopology(targetState, buildTargetTopology(manifest, target), {
    outbound: 'hash-a',
    inbound: 'hash-b',
    routing: 'hash-c',
  });
  const runtimeState = Object.values(targetState.tunnels)[0]!;
  const recorder = createRecordingMutationClient(runtimeState.tunnel);

  await runTargetMonitorTick('source-1', target, memoryState, {
    info() {},
    error() {},
    warn() {},
  } as never, {
    requestViaSocksFn: async () => {
      throw new Error('probe failed');
    },
    createMutationClient: () => recorder.client,
  });

  assert.deepEqual(recorder.calls, [
    `removeRule:${runtimeState.tunnel.routeTag}`,
    `removeOutbound:${runtimeState.tunnel.outboundTagInitial}`,
    `addOutbound:${runtimeState.tunnel.outboundWithoutPrefix.tag}`,
    `addRule:${runtimeState.tunnel.routeTag}->${runtimeState.tunnel.outboundWithoutPrefix.tag}`,
  ]);
  const updatedRuntimeState = targetState.tunnels[runtimeState.tunnel.baseTunnelId]!;
  assert.equal(updatedRuntimeState.monitor.state, 'degraded');
  assert.equal(updatedRuntimeState.monitor.lastLatencyMs, undefined);
  assert.equal(updatedRuntimeState.tunnel.outboundTagCurrent, runtimeState.tunnel.outboundWithoutPrefix.tag);
  assert.equal(buildStatusSnapshot(memoryState)[0]?.balanced, false);
});

test('runTargetMonitorTick rejoins repaired tunnel after successful probe', async () => {
  const manifest = buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
    'inline',
  );
  const target = createTarget({
    observatorySubjectSelectorPrefix: 'x-observe-',
  });
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('source-1', target.address);
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  replaceTargetTopology(targetState, buildTargetTopology(manifest, target), {
    outbound: 'hash-a',
    inbound: 'hash-b',
    routing: 'hash-c',
  });
  const runtimeState = Object.values(targetState.tunnels)[0]!;
  runtimeState.tunnel = {
    ...runtimeState.tunnel,
    outboundTagCurrent: runtimeState.tunnel.outboundWithoutPrefix.tag,
  };

  const rejoined: string[] = [];

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
    rejoinTunnelFn: async (_subscriptionId, _target, _memoryState, repairedRuntimeState) => {
      rejoined.push(repairedRuntimeState.tunnel.baseTunnelId);
      repairedRuntimeState.tunnel.outboundTagCurrent = repairedRuntimeState.tunnel.outboundTagInitial;
    },
  });

  assert.deepEqual(rejoined, [runtimeState.tunnel.baseTunnelId]);
  assert.equal(runtimeState.tunnel.outboundTagCurrent, runtimeState.tunnel.outboundTagInitial);
  assert.equal(runtimeState.monitor.state, 'healthy');
  assert.equal(runtimeState.monitor.lastLatencyMs, 25);
});

test('runTargetMonitorTick keeps healthy check local when balancer rejoin fails', async () => {
  const manifest = buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
    'inline',
  );
  const target = createTarget({
    observatorySubjectSelectorPrefix: 'x-observe-',
  });
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('source-1', target.address);
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  replaceTargetTopology(targetState, buildTargetTopology(manifest, target), {
    outbound: 'hash-a',
    inbound: 'hash-b',
    routing: 'hash-c',
  });
  const runtimeState = Object.values(targetState.tunnels)[0]!;
  runtimeState.tunnel = {
    ...runtimeState.tunnel,
    outboundTagCurrent: runtimeState.tunnel.outboundWithoutPrefix.tag,
  };
  const events: string[] = [];

  await runTargetMonitorTick('source-1', target, memoryState, {
    info() {},
    error(payload: { event?: string }) {
      if (payload.event) {
        events.push(payload.event);
      }
    },
    warn() {},
  } as never, {
    requestViaSocksFn: async () => ({
      statusCode: 200,
      latencyMs: 25,
      bodyBytes: 100,
    }),
    rejoinTunnelFn: async () => {
      throw new Error('xray rejected rejoin');
    },
  });

  assert.equal(runtimeState.monitor.state, 'healthy');
  assert.equal(runtimeState.monitor.lastLatencyMs, 25);
  assert.equal(runtimeState.tunnel.outboundTagCurrent, runtimeState.tunnel.outboundWithoutPrefix.tag);
  assert.match(runtimeState.monitor.lastError ?? '', /rejoin failed: xray rejected rejoin/);
  assert.ok(events.includes('tunnel_rejoin_failed'));
});

test('runTargetMonitorTick real rejoin path restores prefixed outbound and routing after successful probe', async () => {
  const manifest = buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
    'inline',
  );
  const target = createTarget({
    observatorySubjectSelectorPrefix: 'x-observe-',
  });
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('source-1', target.address);
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  replaceTargetTopology(targetState, buildTargetTopology(manifest, target), {
    outbound: 'hash-a',
    inbound: 'hash-b',
    routing: 'hash-c',
  });
  const runtimeState = Object.values(targetState.tunnels)[0]!;
  runtimeState.tunnel = createRepairedTunnel(runtimeState.tunnel);
  runtimeState.monitor.lastError = 'previous repair';
  const recorder = createRecordingMutationClient(runtimeState.tunnel);
  const events: string[] = [];

  await runTargetMonitorTick('source-1', target, memoryState, {
    info(payload: { event?: string }) {
      if (payload.event) {
        events.push(payload.event);
      }
    },
    error() {},
    warn() {},
  } as never, {
    requestViaSocksFn: async () => ({
      statusCode: 200,
      latencyMs: 25,
      bodyBytes: 100,
    }),
    createMutationClient: () => recorder.client,
  });

  assert.deepEqual(recorder.calls, [
    `addOutbound:${runtimeState.tunnel.outboundTagInitial}`,
    `removeRule:${runtimeState.tunnel.routeTag}`,
    `addRule:${runtimeState.tunnel.routeTag}->${runtimeState.tunnel.outboundTagInitial}`,
    `removeOutbound:${runtimeState.tunnel.outboundWithoutPrefix.tag}`,
  ]);
  assert.equal(targetState.tunnels[runtimeState.tunnel.baseTunnelId]?.tunnel.outboundTagCurrent, runtimeState.tunnel.outboundTagInitial);
  assert.equal(targetState.tunnels[runtimeState.tunnel.baseTunnelId]?.monitor.lastError, undefined);
  assert.equal(buildStatusSnapshot(memoryState)[0]?.balanced, true);
  assert.ok(events.includes('tunnel_rejoined_balancer'));
});

test('runTargetMonitorTick real rejoin path skips Xray mutation when tunnel is already balanced', async () => {
  const manifest = buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
    'inline',
  );
  const target = createTarget({
    observatorySubjectSelectorPrefix: 'x-observe-',
  });
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('source-1', target.address);
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  replaceTargetTopology(targetState, buildTargetTopology(manifest, target), {
    outbound: 'hash-a',
    inbound: 'hash-b',
    routing: 'hash-c',
  });
  const runtimeState = Object.values(targetState.tunnels)[0]!;
  let createClientCalled = false;

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
    createMutationClient: () => {
      createClientCalled = true;
      throw new Error('mutation client should not be created');
    },
  });

  assert.equal(createClientCalled, false);
  assert.equal(runtimeState.monitor.state, 'healthy');
  assert.equal(runtimeState.tunnel.outboundTagCurrent, runtimeState.tunnel.outboundTagInitial);
});

test('runTargetMonitorTick real rejoin path rolls back when prefixed routing add fails', async () => {
  const manifest = buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
    'inline',
  );
  const target = createTarget({
    observatorySubjectSelectorPrefix: 'x-observe-',
  });
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('source-1', target.address);
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  replaceTargetTopology(targetState, buildTargetTopology(manifest, target), {
    outbound: 'hash-a',
    inbound: 'hash-b',
    routing: 'hash-c',
  });
  const runtimeState = Object.values(targetState.tunnels)[0]!;
  runtimeState.tunnel = createRepairedTunnel(runtimeState.tunnel);
  const recorder = createRecordingMutationClient(runtimeState.tunnel, {
    failFirstAddRule: true,
  });
  const events: string[] = [];

  await runTargetMonitorTick('source-1', target, memoryState, {
    info() {},
    error(payload: { event?: string }) {
      if (payload.event) {
        events.push(payload.event);
      }
    },
    warn() {},
  } as never, {
    requestViaSocksFn: async () => ({
      statusCode: 200,
      latencyMs: 25,
      bodyBytes: 100,
    }),
    createMutationClient: () => recorder.client,
  });

  assert.deepEqual(recorder.calls, [
    `addOutbound:${runtimeState.tunnel.outboundTagInitial}`,
    `removeRule:${runtimeState.tunnel.routeTag}`,
    `addRule:${runtimeState.tunnel.routeTag}->${runtimeState.tunnel.outboundTagInitial}`,
    `removeRule:${runtimeState.tunnel.routeTag}`,
    `addRule:${runtimeState.tunnel.routeTag}->${runtimeState.tunnel.outboundWithoutPrefix.tag}`,
    `removeOutbound:${runtimeState.tunnel.outboundTagInitial}`,
  ]);
  assert.equal(runtimeState.monitor.state, 'healthy');
  assert.equal(runtimeState.monitor.lastLatencyMs, 25);
  assert.equal(runtimeState.tunnel.outboundTagCurrent, runtimeState.tunnel.outboundWithoutPrefix.tag);
  assert.match(runtimeState.monitor.lastError ?? '', /balancer rejoin failed: addRule prefixed failed/);
  assert.equal(buildStatusSnapshot(memoryState)[0]?.balanced, false);
  assert.ok(events.includes('tunnel_rejoin_failed'));
});

test('runTargetMonitorTick real rejoin rollback failure does not fail whole monitor tick', async () => {
  const manifest = buildManifest(
    [
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf6@example.com:443?type=tcp&security=tls#🇧🇪 Брюссель, Бельгия, Extra',
    ].join('\n'),
    'inline',
  );
  const target = createTarget({
    observatorySubjectSelectorPrefix: 'x-observe-',
  });
  const memoryState = createSyncMemoryState();
  const targetKey = buildTargetStateKey('source-1', target.address);
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  replaceTargetTopology(targetState, buildTargetTopology(manifest, target), {
    outbound: 'hash-a',
    inbound: 'hash-b',
    routing: 'hash-c',
  });
  const runtimeStates = Object.values(targetState.tunnels);
  const repairedRuntimeState = runtimeStates[0]!;
  const balancedRuntimeState = runtimeStates[1]!;
  repairedRuntimeState.tunnel = createRepairedTunnel(repairedRuntimeState.tunnel);
  const recorder = createRecordingMutationClient(repairedRuntimeState.tunnel, {
    failFirstAddRule: true,
    failEveryRollbackAddRule: true,
  });
  const probedPorts: number[] = [];

  await runTargetMonitorTick('source-1', target, memoryState, {
    info() {},
    error() {},
    warn() {},
  } as never, {
    requestViaSocksFn: async ({ proxyPort }) => {
      probedPorts.push(proxyPort);
      return {
        statusCode: 200,
        latencyMs: proxyPort === repairedRuntimeState.tunnel.port ? 25 : 30,
        bodyBytes: 100,
      };
    },
    createMutationClient: () => recorder.client,
  });

  assert.deepEqual(new Set(probedPorts), new Set([repairedRuntimeState.tunnel.port, balancedRuntimeState.tunnel.port]));
  assert.equal(repairedRuntimeState.monitor.state, 'healthy');
  assert.match(repairedRuntimeState.monitor.lastError ?? '', /balancer rejoin failed: addRule prefixed failed/);
  assert.equal(repairedRuntimeState.tunnel.outboundTagCurrent, repairedRuntimeState.tunnel.outboundWithoutPrefix.tag);
  assert.equal(balancedRuntimeState.monitor.state, 'healthy');
  assert.equal(balancedRuntimeState.monitor.lastError, undefined);
  assert.ok(recorder.calls.includes(`removeOutbound:${repairedRuntimeState.tunnel.outboundTagInitial}`));
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
        url: 'https://push.example.test/api/push/token?keep=1',
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
  assert.equal(targetState.balancerMonitor.remotePingLastReportedStatus, 'up');
  assert.equal(targetState.balancerMonitor.remotePingLastReportedMsg, 'OK');
  assert.equal(targetState.balancerMonitor.remotePingLastReportedPingMs, 33);
  assert.deepEqual(remoteCalls, [
    { url: 'https://push.example.test/api/push/token?keep=1&status=up&msg=OK&ping=33' },
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
        url: 'https://push.example.test/api/push/token',
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
  assert.equal(targetState.balancerMonitor.remotePingLastReportedStatus, 'down');
  assert.match(targetState.balancerMonitor.remotePingLastReportedMsg ?? '', /Expected HTTP 204, got 200/);
  assert.deepEqual(remoteCalls, [
    { url: 'https://push.example.test/api/push/token?status=down&msg=Expected+HTTP+204%2C+got+200.' },
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
        url: 'https://push.example.test/api/push/token',
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
        url: 'https://push.example.test/api/push/token',
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
  const logEntries: Array<Record<string, unknown>> = [];

  await runTargetBalancerMonitorTick('source-1', target, memoryState, {
    info(payload: Record<string, unknown>) {
      logEntries.push(payload);
    },
    warn(payload: Record<string, unknown>) {
      logEntries.push(payload);
    },
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
    url: 'https://push.example.test/api/push/token?status=up&msg=OK&ping=33',
    method: 'GET',
    timeoutMs: 3000,
  });
  assert.equal(targetState.balancerMonitor.state, 'healthy');
  assert.equal(targetState.balancerMonitor.remotePingState, 'failed');
  assert.equal(targetState.balancerMonitor.remotePingLastStatusCode, 502);
  assert.deepEqual(
    logEntries
      .filter((entry) => entry.event === 'balancer_monitor_remote_ping_started')
      .map((entry) => ({
        reportedStatus: entry.reportedStatus,
        viaSocks: entry.viaSocks,
        remoteHost: entry.remoteHost,
        proxyHost: entry.proxyHost,
        proxyPort: entry.proxyPort,
      })),
    [{
      reportedStatus: 'up',
      viaSocks: true,
      remoteHost: 'push.example.test',
      proxyHost: '127.0.0.10',
      proxyPort: 1080,
    }],
  );
  assert.deepEqual(
    logEntries
      .filter((entry) => entry.event === 'balancer_monitor_remote_ping_failed')
      .map((entry) => ({
        reportedStatus: entry.reportedStatus,
        viaSocks: entry.viaSocks,
        remoteHost: entry.remoteHost,
        proxyHost: entry.proxyHost,
        proxyPort: entry.proxyPort,
        statusCode: entry.statusCode,
      })),
    [{
      reportedStatus: 'up',
      viaSocks: true,
      remoteHost: 'push.example.test',
      proxyHost: '127.0.0.10',
      proxyPort: 1080,
      statusCode: 502,
    }],
  );
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
      url: 'https://push.example.test/api/push/token?status=up',
      timeoutMs: 5000,
    });

    assert.equal(response.statusCode, 204);
    assert.equal(cancelCalled, true);
    assert.deepEqual(calls, [
      { url: 'https://push.example.test/api/push/token?status=up', method: 'GET' },
    ]);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('buildRemotePingUrl preserves existing query params and overwrites push params', () => {
  assert.equal(
    buildRemotePingUrl('https://push.example.test/api/push/token?keep=1&status=down&msg=old&ping=999', {
      status: 'up',
      msg: 'OK',
      pingMs: 42,
    }),
    'https://push.example.test/api/push/token?keep=1&status=up&msg=OK&ping=42',
  );
  assert.equal(
    buildRemotePingUrl('https://push.example.test/api/push/token?keep=1&ping=999', {
      status: 'down',
      msg: 'failed',
    }),
    'https://push.example.test/api/push/token?keep=1&status=down&msg=failed',
  );
});

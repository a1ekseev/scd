import assert from 'node:assert/strict';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';

import { buildInboundGrpc } from '../src/builders/build-inbound-grpc.ts';
import { buildOutboundGrpc } from '../src/builders/build-outbound-grpc.ts';
import { concatBytes, encodeRepeatedStringField, encodeStringField, type RawInboundConfig, type RawOutboundConfig, type RawRoutingRule } from '../src/api/protobuf.ts';
import { buildManifest } from '../src/manifest.ts';
import { buildCurrentRuntimeStateSnapshot } from '../src/runtime/current-runtime-state.ts';
import { groupStatusSnapshot } from '../src/runtime/status-server.ts';
import type { LoadedConfig, StatusSnapshotTunnel, SubscriptionConfig, SubscriptionTargetConfig } from '../src/types.ts';
import { buildTargetTopology } from '../src/topology/build-tunnel-topology.ts';

function createTarget(overrides: Partial<SubscriptionTargetConfig> = {}): SubscriptionTargetConfig {
  return {
    address: '127.0.0.1:8080',
    timeoutMs: 5000,
    fixedOutbounds: ['direct', 'blocked'],
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
    speedtest: {
      enabled: false,
      method: 'GET',
      timeoutMs: 15000,
      maxParallel: 3,
    },
    observatorySubjectSelectorPrefix: 'x-observe-',
    ...overrides,
  };
}

function createLoadedConfig(subscription: SubscriptionConfig): LoadedConfig {
  return {
    configPath: '/tmp/config.yml',
    config: {
      subscriptions: [subscription],
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
        enabled: false,
        runtimeState: {
          enabled: true,
          includeRaw: false,
          includeSecrets: false,
        },
      },
    },
  };
}

function buildRoutingRuleRaw(outboundTag: string, inboundTag: string, ruleTag: string): Uint8Array {
  return concatBytes([
    encodeStringField(1, outboundTag),
    encodeRepeatedStringField(8, [inboundTag]),
    encodeStringField(19, ruleTag),
  ]);
}

test('buildCurrentRuntimeStateSnapshot combines local config with runtime API state and redacts by default', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-runtime-state-'));

  try {
    const content = 'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra';
    const inputPath = join(tempDir, 'subscription.txt');
    await writeFile(inputPath, content, 'utf8');

    const target = createTarget();
    const subscription: SubscriptionConfig = {
      id: 'source-1',
      input: inputPath,
      enabled: true,
      format: 'plain',
      fetchTimeoutMs: 5000,
      targets: [target],
    };
    const loadedConfig = createLoadedConfig(subscription);
    const manifest = buildManifest(content, subscription.id);
    const topology = buildTargetTopology(manifest, target);
    const tunnel = topology.tunnels[0]!;

    const outbounds: RawOutboundConfig[] = [
      { tag: 'direct', raw: new Uint8Array([1, 2, 3]) },
      { tag: tunnel.outboundTagCurrent, raw: buildOutboundGrpc(tunnel.outboundInitial.normalized).raw },
    ];
    const inbounds: RawInboundConfig[] = [
      { tag: tunnel.inboundTag, raw: buildInboundGrpc(tunnel).raw },
    ];
    const rules: RawRoutingRule[] = [
      {
        tag: tunnel.outboundTagCurrent,
        ruleTag: tunnel.routeTag,
        inboundTags: [tunnel.inboundTag],
        raw: buildRoutingRuleRaw(tunnel.outboundTagCurrent, tunnel.inboundTag, tunnel.routeTag),
      },
    ];

    const snapshot = await buildCurrentRuntimeStateSnapshot(loadedConfig, subscription.id, target.address, {
      createClient: () => ({
        listOutbounds: async () => outbounds,
        listInbounds: async () => inbounds,
        listRules: async () => rules,
      }),
    });

    assert.ok(snapshot);
    assert.equal(snapshot?.config.subscription.id, 'source-1');
    assert.equal(snapshot?.expected.outbounds.length, 1);
    assert.equal(snapshot?.runtime.outbounds[0]?.classification, 'fixed');
    assert.equal(snapshot?.runtime.outbounds[1]?.classification, 'managed-initial');
    assert.equal(snapshot?.runtime.inbounds[0]?.classification, 'managed');
    assert.equal(snapshot?.runtime.routingRules[0]?.classification, 'managed');
    assert.deepEqual(snapshot?.diff.outbounds.missing, []);
    assert.deepEqual(snapshot?.diff.inbounds.missing, []);
    assert.deepEqual(snapshot?.diff.routingRules.missing, []);
    assert.equal(snapshot?.runtime.outbounds[1]?.parsed?.protocol, 'vless');
    assert.equal(snapshot?.runtime.outbounds[1]?.parsed?.uuid, undefined);
    assert.equal(snapshot?.runtime.outbounds[1]?.rawBase64, undefined);
    assert.equal(snapshot?.runtime.inbounds[0]?.parsed?.protocol, 'socks');
    assert.equal(snapshot?.runtime.routingRules[0]?.parsed.outboundTag, tunnel.outboundTagCurrent);
    assert.equal(snapshot?.serviceState?.balancerMonitor, undefined);
  } finally {
    await rm(tempDir, { recursive: true, force: true });
  }
});

test('buildCurrentRuntimeStateSnapshot reflects target-specific vision udp443 override in expected and runtime views', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-runtime-flow-'));

  try {
    const content =
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?security=reality&encryption=none&fp=chrome&headerType=none&type=tcp&flow=xtls-rprx-vision&sni=io.example.test&pbk=CMkW1axrhEXoiJ6anMz9XEjlfqlAtEZya7L0b5ZPMyw&sid=abe4a59b9f2407e3#🇩🇪 Германия, Extra';
    const inputPath = join(tempDir, 'subscription.txt');
    await writeFile(inputPath, content, 'utf8');

    const target = createTarget({
      visionUdp443Override: true,
    });
    const subscription: SubscriptionConfig = {
      id: 'source-1',
      input: inputPath,
      enabled: true,
      format: 'plain',
      fetchTimeoutMs: 5000,
      targets: [target],
    };
    const loadedConfig = createLoadedConfig(subscription);
    const manifest = buildManifest(content, subscription.id);
    const topology = buildTargetTopology(manifest, target);
    const tunnel = topology.tunnels[0]!;

    const snapshot = await buildCurrentRuntimeStateSnapshot(loadedConfig, subscription.id, target.address, {
      createClient: () => ({
        listOutbounds: async () => [
          { tag: tunnel.outboundTagCurrent, raw: buildOutboundGrpc(tunnel.outboundInitial.normalized).raw },
        ],
        listInbounds: async () => [],
        listRules: async () => [],
      }),
    });

    assert.ok(snapshot);
    assert.equal(snapshot?.expected.outbounds[0]?.flow, 'xtls-rprx-vision-udp443');
    assert.equal(snapshot?.runtime.outbounds[0]?.parsed?.flow, 'xtls-rprx-vision-udp443');
  } finally {
    await rm(tempDir, { recursive: true, force: true });
  }
});

test('buildCurrentRuntimeStateSnapshot treats repaired outbound tag as fallback match', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-runtime-fallback-'));

  try {
    const content = 'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra';
    const inputPath = join(tempDir, 'subscription.txt');
    await writeFile(inputPath, content, 'utf8');

    const target = createTarget();
    const subscription: SubscriptionConfig = {
      id: 'source-1',
      input: inputPath,
      enabled: true,
      format: 'plain',
      fetchTimeoutMs: 5000,
      targets: [target],
    };
    const loadedConfig = createLoadedConfig(subscription);
    const manifest = buildManifest(content, subscription.id);
    const topology = buildTargetTopology(manifest, target);
    const tunnel = topology.tunnels[0]!;

    const snapshot = await buildCurrentRuntimeStateSnapshot(loadedConfig, subscription.id, target.address, {
      createClient: () => ({
        listOutbounds: async () => [
          { tag: tunnel.outboundWithoutPrefix.tag, raw: buildOutboundGrpc(tunnel.outboundWithoutPrefix.normalized).raw },
        ],
        listInbounds: async () => [],
        listRules: async () => [],
      }),
    });

    assert.ok(snapshot);
    assert.deepEqual(snapshot?.diff.outbounds.matched, []);
    assert.deepEqual(snapshot?.diff.outbounds.matchedFallback, [tunnel.outboundWithoutPrefix.tag]);
    assert.deepEqual(snapshot?.diff.outbounds.missing, []);
  } finally {
    await rm(tempDir, { recursive: true, force: true });
  }
});

test('buildCurrentRuntimeStateSnapshot does not refetch remote subscriptions and redacts remote input by default', async () => {
  const target = createTarget();
  const subscription: SubscriptionConfig = {
    id: 'source-1',
    input: 'https://example.test/tokenized/path?secret=abc',
    enabled: true,
    format: 'plain',
    fetchTimeoutMs: 5000,
    targets: [target],
  };
  const loadedConfig = createLoadedConfig(subscription);
  const memoryState = {
    syncInProgress: false,
    targetLocks: {},
    targets: {
      [`${subscription.id}::${target.address}`]: {
        resources: {},
        topologyGeneration: 0,
        tunnels: {},
        balancerMonitor: {
          state: 'healthy' as const,
          consecutiveFailures: 0,
          lastStatusCode: 204,
        },
      },
    },
  };

  let localLoadCalled = false;
  const snapshot = await buildCurrentRuntimeStateSnapshot(loadedConfig, subscription.id, target.address, {
    createClient: () => ({
      listOutbounds: async () => [],
      listInbounds: async () => [],
      listRules: async () => [],
    }),
    loadLocalInputFn: async () => {
      localLoadCalled = true;
      throw new Error('should not be called');
    },
    memoryState: memoryState as never,
  });

  assert.ok(snapshot);
  assert.equal(localLoadCalled, false);
  assert.equal(snapshot?.config.subscription.input, 'https://example.test/<redacted>');
  assert.match(snapshot?.expected.error ?? '', /does not refetch subscription sources/);
  assert.equal(snapshot?.serviceState?.balancerMonitor?.state, 'healthy');
});

test('buildCurrentRuntimeStateSnapshot exposes raw and secrets only when explicitly enabled', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-runtime-secrets-'));

  try {
    const content = 'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra';
    const inputPath = join(tempDir, 'subscription.txt');
    await writeFile(inputPath, content, 'utf8');

    const target = createTarget();
    const subscription: SubscriptionConfig = {
      id: 'source-1',
      input: inputPath,
      enabled: true,
      format: 'plain',
      fetchTimeoutMs: 5000,
      targets: [target],
    };
    const loadedConfig = createLoadedConfig(subscription);
    const manifest = buildManifest(content, subscription.id);
    const topology = buildTargetTopology(manifest, target);
    const tunnel = topology.tunnels[0]!;

    const snapshot = await buildCurrentRuntimeStateSnapshot(
      loadedConfig,
      subscription.id,
      target.address,
      {
        createClient: () => ({
          listOutbounds: async () => [
            { tag: tunnel.outboundTagCurrent, raw: buildOutboundGrpc(tunnel.outboundInitial.normalized).raw },
          ],
          listInbounds: async () => [],
          listRules: async () => [],
        }),
      },
      {
        includeRaw: true,
        includeSecrets: true,
      },
    );

    assert.ok(snapshot?.runtime.outbounds[0]?.rawBase64);
    assert.equal(snapshot?.runtime.outbounds[0]?.parsed?.uuid, '7d1b6590-1069-4372-92be-8d0a0ae6eaf5');
  } finally {
    await rm(tempDir, { recursive: true, force: true });
  }
});

test('groupStatusSnapshot groups tunnels by subscription and target', () => {
  const snapshot: StatusSnapshotTunnel[] = [
    {
      subscriptionId: 'sub-b',
      targetAddress: '127.0.0.1:8081',
      displayName: 'Beta',
      endpoint: '127.0.0.1:20001',
      state: 'healthy',
    },
    {
      subscriptionId: 'sub-a',
      targetAddress: '127.0.0.1:8080',
      displayName: 'Alpha',
      endpoint: '127.0.0.1:20000',
      state: 'idle',
    },
    {
      subscriptionId: 'sub-a',
      targetAddress: '127.0.0.1:8082',
      displayName: 'Gamma',
      endpoint: '127.0.0.1:20002',
      state: 'degraded',
    },
  ];

  const grouped = groupStatusSnapshot(snapshot);

  assert.equal(grouped.length, 2);
  assert.equal(grouped[0]?.subscriptionId, 'sub-a');
  assert.equal(grouped[0]?.targets.length, 2);
  assert.equal(grouped[0]?.targets[0]?.targetAddress, '127.0.0.1:8080');
  assert.equal(grouped[0]?.targets[1]?.targetAddress, '127.0.0.1:8082');
  assert.equal(grouped[1]?.subscriptionId, 'sub-b');
});

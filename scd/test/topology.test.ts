import assert from 'node:assert/strict';
import test from 'node:test';

import { buildManifest } from '../src/manifest.ts';
import { buildStatusSnapshot, createSyncMemoryState, getOrCreateTargetState, replaceTargetTopology } from '../src/runtime/run-state.ts';
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
    ...overrides,
  };
}

test('buildTargetTopology allocates stable ports and target-scoped tags', () => {
  const manifest = buildManifest(
    [
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf6@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия',
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия',
    ].join('\n'),
    'inline',
  );

  const topology = buildTargetTopology(
    manifest,
    createTarget({
      observatorySubjectSelectorPrefix: 'x-observe-',
    }),
  );

  assert.equal(topology.tunnels.length, 2);
  assert.equal(topology.tunnels[0]?.port, 20000);
  assert.equal(topology.tunnels[1]?.port, 20001);
  assert.match(topology.tunnels[0]?.outboundTagInitial ?? '', /^x-observe-/);
  assert.equal(topology.tunnels[0]?.outboundWithoutPrefix.tag, topology.tunnels[0]?.baseOutboundTag);
  assert.match(topology.tunnels[0]?.inboundTag ?? '', /^in-/);
  assert.match(topology.tunnels[0]?.routeTag ?? '', /^route-/);
});

test('buildTargetTopology overrides reality vision flow per target when enabled', () => {
  const manifest = buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?security=reality&encryption=none&fp=chrome&headerType=none&type=tcp&flow=xtls-rprx-vision&sni=io.example.test&pbk=CMkW1axrhEXoiJ6anMz9XEjlfqlAtEZya7L0b5ZPMyw&sid=abe4a59b9f2407e3#🇩🇪 Германия, Extra',
    'inline',
  );

  const regularTarget = buildTargetTopology(manifest, createTarget());
  const udp443Target = buildTargetTopology(
    manifest,
    createTarget({
      visionUdp443Override: true,
    }),
  );

  assert.equal(regularTarget.tunnels[0]?.outboundInitial.normalized.flow, 'xtls-rprx-vision');
  assert.equal(regularTarget.tunnels[0]?.outboundWithoutPrefix.normalized.flow, 'xtls-rprx-vision');
  assert.equal(udp443Target.tunnels[0]?.outboundInitial.normalized.flow, 'xtls-rprx-vision-udp443');
  assert.equal(udp443Target.tunnels[0]?.outboundWithoutPrefix.normalized.flow, 'xtls-rprx-vision-udp443');
});

test('buildTargetTopology fails when port range is too small', () => {
  const manifest = buildManifest(
    [
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf6@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия',
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия',
    ].join('\n'),
    'inline',
  );

  assert.throws(
    () =>
      buildTargetTopology(
        manifest,
        createTarget({
          inboundSocks: {
            listen: '127.0.0.1',
            portRange: {
              start: 20000,
              end: 20000,
            },
          },
        }),
      ),
    /can allocate only 1 ports, but 2 tunnels were generated/,
  );
});

test('buildStatusSnapshot exposes display name, country and endpoint from memory state', () => {
  const manifest = buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия',
    'inline',
  );
  const memoryState = createSyncMemoryState();
  const targetKey = 'source-1::127.0.0.1:8080';
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  const topology = buildTargetTopology(manifest, createTarget());
  replaceTargetTopology(targetState, topology, { outbound: 'hash-a', inbound: 'hash-b', routing: 'hash-c' });

  const snapshot = buildStatusSnapshot(memoryState);

  assert.equal(snapshot.length, 1);
  assert.equal(snapshot[0]?.subscriptionId, 'source-1');
  assert.equal(snapshot[0]?.targetAddress, '127.0.0.1:8080');
  assert.equal(snapshot[0]?.countryIso2, 'AT');
  assert.equal(snapshot[0]?.endpoint, '127.0.0.1:20000');
  assert.match(snapshot[0]?.displayName ?? '', /Вена/);
});

import assert from 'node:assert/strict';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import test from 'node:test';

import { applyOutbounds } from '../src/apply/apply-outbounds.ts';
import { createOutboundApplicator } from '../src/apply/outbound-applicator.ts';
import { loadConfig } from '../src/config/load-config.ts';
import { createLogger } from '../src/logging/create-logger.ts';
import { buildManifest } from '../src/manifest.ts';
import { loadSubscriptions } from '../src/runtime/generate-manifest-from-source.ts';
import { createSyncMemoryState } from '../src/runtime/run-state.ts';
import { syncWithConfig } from '../src/runtime/sync-once.ts';
import type { LoadedConfig, ResourceConfig, StatusServerConfig, SubscriptionTargetConfig } from '../src/types.ts';

function createTargetConfig(overrides: Partial<SubscriptionTargetConfig> = {}): SubscriptionTargetConfig {
  return {
    address: '127.0.0.1:8080',
    timeoutMs: 5000,
    fixedOutbounds: [],
    fixedInbounds: [],
    fixedRouting: [],
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

function createResourcesConfig(overrides: Partial<ResourceConfig> = {}): ResourceConfig {
  return {
    outbounds: {
      enabled: true,
      ...(overrides.outbounds ?? {}),
    },
    inbounds: {
      enabled: false,
      ...(overrides.inbounds ?? {}),
    },
    routing: {
      enabled: false,
      ...(overrides.routing ?? {}),
    },
  };
}

function createStatusServerConfig(overrides: Partial<StatusServerConfig> = {}): StatusServerConfig {
  return {
    enabled: false,
    runtimeState: {
      enabled: true,
      includeRaw: false,
      includeSecrets: false,
    },
    ...overrides,
  };
}

test('loadConfig resolves relative paths, targets and interpolates env vars', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-config-'));
  process.env.XRAY_API_ADDRESS = '127.0.0.1:8080';

  try {
    const configPath = join(tempDir, 'config.yml');
    await writeFile(
      configPath,
      [
        'subscriptions:',
        '  - id: source-1',
        '    input: ./subscription.txt',
        '    enabled: true',
        '    format: auto',
        '    fetchTimeoutMs: 7000',
        '    filters:',
        '      countryAllowlist:',
        '        - de',
        '      labelIncludeRegex: /,\\s*Extra$/i',
        '    targets:',
        '      - address: ${XRAY_API_ADDRESS}',
        '        timeoutMs: 5000',
        '        fixedOutbounds:',
        '          - direct',
        '        observatorySubjectSelectorPrefix: x-observe-',
        'runtime:',
        '  mode: run-once',
        'logging:',
        '  level: info',
        '  format: pretty',
        'resources:',
        '  outbounds:',
        '    enabled: true',
      ].join('\n'),
    );

    const loaded = await loadConfig(configPath);
    assert.equal(loaded.config.subscriptions[0]?.targets[0]?.address, '127.0.0.1:8080');
    assert.equal(loaded.config.subscriptions[0]?.fetchTimeoutMs, 7000);
    assert.deepEqual(loaded.config.subscriptions[0]?.filters?.countryAllowlist, ['DE']);
    assert.equal(loaded.config.subscriptions[0]?.filters?.labelIncludeRegex, '/,\\s*Extra$/i');
    assert.equal(loaded.config.subscriptions[0]?.targets[0]?.timeoutMs, 5000);
    assert.deepEqual(loaded.config.subscriptions[0]?.targets[0]?.fixedOutbounds, ['direct']);
    assert.equal(loaded.config.subscriptions[0]?.targets[0]?.observatorySubjectSelectorPrefix, 'x-observe-');
    assert.equal(loaded.config.subscriptions[0]?.input, resolve(tempDir, 'subscription.txt'));
  } finally {
    delete process.env.XRAY_API_ADDRESS;
    await rm(tempDir, { recursive: true, force: true });
  }
});

test('loadConfig rejects duplicate target addresses', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-duplicate-address-'));

  try {
    const configPath = join(tempDir, 'config.yml');
    await writeFile(
      configPath,
      [
        'subscriptions:',
        '  - id: source-1',
        '    input: ./subscription-1.txt',
        '    fetchTimeoutMs: 5000',
        '    targets:',
        '      - address: 127.0.0.1:8080',
        '  - id: source-2',
        '    input: ./subscription-2.txt',
        '    fetchTimeoutMs: 5000',
        '    targets:',
        '      - address: 127.0.0.1:8080',
        'runtime:',
        '  mode: run-once',
        'logging:',
        '  level: info',
        '  format: json',
        'resources:',
        '  outbounds:',
        '    enabled: true',
      ].join('\n'),
    );

    await assert.rejects(() => loadConfig(configPath), /Duplicate target address "127\.0\.0\.1:8080"/);
  } finally {
    await rm(tempDir, { recursive: true, force: true });
  }
});

test('loadConfig rejects invalid daemon cron expression', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-cron-'));

  try {
    const configPath = join(tempDir, 'config.yml');
    await writeFile(
      configPath,
      [
        'subscriptions:',
        '  - id: source-1',
        '    input: ./subscription.txt',
        '    fetchTimeoutMs: 5000',
        '    targets:',
        '      - address: 127.0.0.1:8080',
        'runtime:',
        '  mode: daemon',
        '  schedule: "not-a-cron"',
        'logging:',
        '  level: info',
        '  format: json',
        'resources:',
        '  outbounds:',
        '    enabled: true',
      ].join('\n'),
    );

    await assert.rejects(() => loadConfig(configPath), /runtime\.schedule:/);
  } finally {
    await rm(tempDir, { recursive: true, force: true });
  }
});

test('loadConfig rejects unsupported country code in filters', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-invalid-country-filter-'));

  try {
    const configPath = join(tempDir, 'config.yml');
    await writeFile(
      configPath,
      [
        'subscriptions:',
        '  - id: source-1',
        '    input: ./subscription.txt',
        '    filters:',
        '      countryAllowlist:',
        '        - ZZ',
        '    targets:',
        '      - address: 127.0.0.1:8080',
        'runtime:',
        '  mode: run-once',
        'logging:',
        '  level: info',
        '  format: json',
        'resources:',
        '  outbounds:',
        '    enabled: true',
      ].join('\n'),
    );

    await assert.rejects(() => loadConfig(configPath), /Unsupported ISO2 country code "ZZ"/);
  } finally {
    await rm(tempDir, { recursive: true, force: true });
  }
});

test('loadConfig rejects invalid regex literal in filters', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-invalid-regex-filter-'));

  try {
    const configPath = join(tempDir, 'config.yml');
    await writeFile(
      configPath,
      [
        'subscriptions:',
        '  - id: source-1',
        '    input: ./subscription.txt',
        '    filters:',
        '      labelIncludeRegex: /(/i',
        '    targets:',
        '      - address: 127.0.0.1:8080',
        'runtime:',
        '  mode: run-once',
        'logging:',
        '  level: info',
        '  format: json',
        'resources:',
        '  outbounds:',
        '    enabled: true',
      ].join('\n'),
    );

    await assert.rejects(() => loadConfig(configPath), /labelIncludeRegex:/);
  } finally {
    await rm(tempDir, { recursive: true, force: true });
  }
});

test('loadConfig accepts multiple speedtest urls', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-speedtest-urls-'));

  try {
    const configPath = join(tempDir, 'config.yml');
    await writeFile(
      configPath,
      [
        'subscriptions:',
        '  - id: source-1',
        '    input: ./subscription.txt',
        '    targets:',
        '      - address: 127.0.0.1:8080',
        '        fixedOutbounds: []',
        '        fixedInbounds: []',
        '        fixedRouting: []',
        '        inboundSocks:',
        '          listen: 127.0.0.1',
        '          portRange:',
        '            start: 20000',
        '            end: 20010',
        '        speedtest:',
        '          enabled: true',
        '          schedule: "*/10 * * * *"',
        '          urls:',
        '            - https://example.com/test-10mb.bin',
        '            - https://example.com/test-50mb.bin',
        '          method: GET',
        '          timeoutMs: 15000',
        '          maxParallel: 2',
        'runtime:',
        '  mode: run-once',
        'logging:',
        '  level: info',
        '  format: json',
        'resources:',
        '  outbounds:',
        '    enabled: true',
        '  inbounds:',
        '    enabled: true',
        '  routing:',
        '    enabled: true',
      ].join('\n'),
    );

    const loaded = await loadConfig(configPath);
    assert.deepEqual(loaded.config.subscriptions[0]?.targets[0]?.speedtest.urls, [
      'https://example.com/test-10mb.bin',
      'https://example.com/test-50mb.bin',
    ]);
    assert.equal(loaded.config.subscriptions[0]?.targets[0]?.speedtest.maxParallel, 2);
  } finally {
    await rm(tempDir, { recursive: true, force: true });
  }
});

test('loadConfig rejects invalid speedtest maxParallel', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-speedtest-max-parallel-'));

  try {
    const configPath = join(tempDir, 'config.yml');
    await writeFile(
      configPath,
      [
        'subscriptions:',
        '  - id: source-1',
        '    input: ./subscription.txt',
        '    targets:',
        '      - address: 127.0.0.1:8080',
        '        fixedOutbounds: []',
        '        fixedInbounds: []',
        '        fixedRouting: []',
        '        inboundSocks:',
        '          listen: 127.0.0.1',
        '          portRange:',
        '            start: 20000',
        '            end: 20010',
        '        speedtest:',
        '          enabled: true',
        '          schedule: "*/10 * * * *"',
        '          urls:',
        '            - https://example.com/test-10mb.bin',
        '          method: GET',
        '          timeoutMs: 15000',
        '          maxParallel: 0',
        'runtime:',
        '  mode: run-once',
        'logging:',
        '  level: info',
        '  format: json',
        'resources:',
        '  outbounds:',
        '    enabled: true',
        '  inbounds:',
        '    enabled: true',
        '  routing:',
        '    enabled: true',
      ].join('\n'),
    );

    await assert.rejects(() => loadConfig(configPath), /maxParallel/);
  } finally {
    await rm(tempDir, { recursive: true, force: true });
  }
});

test('loadConfig accepts monitor.maxParallel and statusServer.runtimeState defaults', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-monitor-status-config-'));

  try {
    const configPath = join(tempDir, 'config.yml');
    await writeFile(
      configPath,
      [
        'subscriptions:',
        '  - id: source-1',
        '    input: ./subscription.txt',
        '    targets:',
        '      - address: 127.0.0.1:8080',
        '        fixedOutbounds: []',
        '        fixedInbounds: []',
        '        fixedRouting: []',
        '        inboundSocks:',
        '          listen: 127.0.0.1',
        '          portRange:',
        '            start: 20000',
        '            end: 20010',
        '        monitor:',
        '          enabled: true',
        '          schedule: "*/2 * * * *"',
        '          maxParallel: 4',
        '          request:',
        '            url: https://example.com/health',
        '            method: GET',
        '            expectedStatus: 200',
        '            timeoutMs: 5000',
        'runtime:',
        '  mode: run-once',
        'logging:',
        '  level: info',
        '  format: json',
        'resources:',
        '  outbounds:',
        '    enabled: true',
        '  inbounds:',
        '    enabled: true',
        '  routing:',
        '    enabled: true',
        'statusServer:',
        '  enabled: true',
        '  listen: 127.0.0.1:9090',
      ].join('\n'),
    );

    const loaded = await loadConfig(configPath);
    assert.equal(loaded.config.subscriptions[0]?.targets[0]?.monitor.maxParallel, 4);
    assert.equal(loaded.config.statusServer.runtimeState.enabled, true);
    assert.equal(loaded.config.statusServer.runtimeState.includeRaw, false);
    assert.equal(loaded.config.statusServer.runtimeState.includeSecrets, false);
  } finally {
    await rm(tempDir, { recursive: true, force: true });
  }
});

test('loadConfig accepts balancerMonitor config and rejects incomplete balancerMonitor config', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-balancer-monitor-config-'));

  try {
    const validPath = join(tempDir, 'valid.yml');
    await writeFile(
      validPath,
      [
        'subscriptions:',
        '  - id: source-1',
        '    input: ./subscription.txt',
        '    targets:',
        '      - address: 127.0.0.1:8080',
        '        balancerMonitor:',
        '          enabled: true',
        '          schedule: "*/2 * * * *"',
        '          socks5:',
        '            host: 127.0.0.1',
        '            port: 1080',
        '          request:',
        '            url: https://example.com/health',
        '            method: GET',
        '            expectedStatus: 200',
        '            timeoutMs: 5000',
        '          successGet:',
        '            url: https://example.com/ping.txt',
        '            expectedStatus: 200',
        '            timeoutMs: 5000',
        'runtime:',
        '  mode: run-once',
        'logging:',
        '  level: info',
        '  format: json',
        'resources:',
        '  outbounds:',
        '    enabled: true',
      ].join('\n'),
    );

    const loaded = await loadConfig(validPath);
    assert.equal(loaded.config.subscriptions[0]?.targets[0]?.balancerMonitor.enabled, true);
    assert.equal(loaded.config.subscriptions[0]?.targets[0]?.balancerMonitor.socks5?.port, 1080);
    assert.equal(loaded.config.subscriptions[0]?.targets[0]?.balancerMonitor.successGet?.expectedStatus, 200);

    const invalidPath = join(tempDir, 'invalid.yml');
    await writeFile(
      invalidPath,
      [
        'subscriptions:',
        '  - id: source-1',
        '    input: ./subscription.txt',
        '    targets:',
        '      - address: 127.0.0.1:8080',
        '        balancerMonitor:',
        '          enabled: true',
        'runtime:',
        '  mode: run-once',
        'logging:',
        '  level: info',
        '  format: json',
        'resources:',
        '  outbounds:',
        '    enabled: true',
      ].join('\n'),
    );

    await assert.rejects(() => loadConfig(invalidPath), /balancerMonitor\.(schedule|socks5|request)/);
  } finally {
    await rm(tempDir, { recursive: true, force: true });
  }
});

test('loadSubscriptions sorts enabled subscriptions by id', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-sort-subscriptions-'));

  try {
    const sourceAPath = join(tempDir, 'a.txt');
    const sourceBPath = join(tempDir, 'b.txt');
    const payload = 'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия';
    await writeFile(sourceAPath, payload);
    await writeFile(sourceBPath, payload);

    const result = await loadSubscriptions([
      {
        id: 'source-b',
        input: sourceBPath,
        enabled: true,
        format: 'auto',
        fetchTimeoutMs: 5000,
        targets: [createTargetConfig({ address: '127.0.0.1:8081' })],
      },
      {
        id: 'source-a',
        input: sourceAPath,
        enabled: true,
        format: 'auto',
        fetchTimeoutMs: 5000,
        targets: [createTargetConfig({ address: '127.0.0.1:8080' })],
      },
    ]);

    assert.deepEqual(result.failed, []);
    assert.deepEqual(
      result.loaded.map((subscription) => subscription.id),
      ['source-a', 'source-b'],
    );
  } finally {
    await rm(tempDir, { recursive: true, force: true });
  }
});

test('outbound applicator stabilizes manifest hash and managed ids for reordered lines', () => {
  const applicator = createOutboundApplicator(() => {
    throw new Error('client should not be created in buildPlan test');
  });
  const first = applicator.buildPlan({
    id: 'source-1',
    input: 'inline',
    source: 'inline',
    encoding: 'plain',
    targets: [],
    content: [
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия',
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf6@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия',
    ].join('\n'),
  });
  const second = applicator.buildPlan({
    id: 'source-1',
    input: 'inline',
    source: 'inline',
    encoding: 'plain',
    targets: [],
    content: [
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf6@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия',
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия',
    ].join('\n'),
  });

  assert.equal(first.manifestHash, second.manifestHash);
  assert.deepEqual(first.managedIds, second.managedIds);
  assert.deepEqual(
    first.manifest.entries.map((entry) => entry.tag),
    [...first.manifest.entries.map((entry) => entry.tag)].sort(),
  );
});

test('outbound applicator applies target-specific observatory prefix to effective tags', () => {
  const applicator = createOutboundApplicator(() => {
    throw new Error('client should not be created in preparePlanForTarget test');
  });
  const basePlan = applicator.buildPlan({
    id: 'source-1',
    input: 'inline',
    source: 'inline',
    encoding: 'plain',
    targets: [],
    content: 'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия',
  });

  const targetPlan = applicator.preparePlanForTarget(
    basePlan,
    createTargetConfig({
      fixedOutbounds: ['direct', 'blocked'],
      observatorySubjectSelectorPrefix: 'x-observe-',
    }),
  );

  assert.notEqual(basePlan.manifestHash, targetPlan.manifestHash);
  assert.match(targetPlan.managedIds[0] ?? '', /^x-observe-/);
  assert.match(targetPlan.manifest.entries[0]?.tag ?? '', /^x-observe-/);
  assert.match(targetPlan.manifest.entries[0]?.normalized.tag ?? '', /^x-observe-/);
  assert.match(String(targetPlan.manifest.entries[0]?.jsonOutbound.tag ?? ''), /^x-observe-/);
});

test('applyOutbounds removes all current non-fixed tags and keeps fixed outbound tags', async () => {
  const manifest = buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия',
    'inline',
  );
  const entry = manifest.entries[0]!;
  const removed: string[] = [];
  const added: string[] = [];

  const client = {
    async listOutbounds() {
      return [
        { tag: 'old-managed', raw: new Uint8Array([1]) },
        { tag: 'old-other', raw: new Uint8Array([3]) },
        { tag: 'fixed-direct', raw: new Uint8Array([2]) },
      ];
    },
    async removeOutbound(tag: string) {
      removed.push(tag);
    },
    async addOutbound(rawOutbound: Uint8Array) {
      assert.ok(rawOutbound.length > 0);
      added.push(entry.tag);
    },
  };

  const report = await applyOutbounds(manifest, client, 'inline', {
    subscriptionId: 'source-1',
    targetAddress: '127.0.0.1:8080',
    fixedOutbounds: ['fixed-direct'],
  });

  assert.equal(report.subscriptionId, 'source-1');
  assert.equal(report.targetAddress, '127.0.0.1:8080');
  assert.equal(report.removed, 2);
  assert.equal(report.added, 1);
  assert.equal(report.replaced, 0);
  assert.deepEqual(report.deletedIds, ['old-managed', 'old-other']);
  assert.deepEqual(report.appliedIds, [entry.tag]);
  assert.deepEqual(removed, ['old-managed', 'old-other']);
  assert.deepEqual(added, [entry.tag]);
});

test('applyOutbounds rolls back removed tags when add fails', async () => {
  const manifest = buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия',
    'inline',
  );
  const removeCalls: string[] = [];
  const addCalls: Uint8Array[] = [];

  const client = {
    async listOutbounds() {
      return [{ tag: 'old-managed', raw: new Uint8Array([7, 7]) }];
    },
    async removeOutbound(tag: string) {
      removeCalls.push(tag);
    },
    async addOutbound(rawOutbound: Uint8Array) {
      addCalls.push(new Uint8Array(rawOutbound));
      if (addCalls.length === 1) {
        throw new Error('boom');
      }
    },
  };

  const report = await applyOutbounds(manifest, client, 'inline', {
    subscriptionId: 'source-1',
    targetAddress: '127.0.0.1:8080',
    fixedOutbounds: [],
  });

  assert.equal(report.failed, 1);
  assert.equal(removeCalls[0], 'old-managed');
  assert.equal(addCalls.length, 2);
  assert.deepEqual(Array.from(addCalls[1] ?? new Uint8Array()), [7, 7]);
});

test('syncWithConfig skips Xray API when subscription manifest is unchanged in memory', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-sync-'));
  const logger = createLogger({
    level: 'silent',
    format: 'json',
  });
  const memoryState = createSyncMemoryState();
  const loadedConfig: LoadedConfig = {
    configPath: join(tempDir, 'config.yml'),
    config: {
      subscriptions: [
        {
          id: 'source-1',
          input: resolve(tempDir, 'subscription.txt'),
          enabled: true,
          format: 'auto',
          fetchTimeoutMs: 5000,
          targets: [
            createTargetConfig({
              observatorySubjectSelectorPrefix: 'x-observe-',
            }),
          ],
        },
      ],
      runtime: {
        mode: 'run-once',
      },
      logging: {
        level: 'silent',
        format: 'json',
      },
      resources: createResourcesConfig(),
      statusServer: createStatusServerConfig(),
    },
  };

  let listCalls = 0;
  const services = {
    async loadSubscriptionsFn() {
      return {
        loaded: [
          {
            id: 'source-1',
            input: resolve(tempDir, 'subscription.txt'),
            source: resolve(tempDir, 'subscription.txt'),
            content: 'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия',
            encoding: 'plain' as const,
            targets: loadedConfig.config.subscriptions[0]!.targets,
          },
        ],
        failed: [],
      };
    },
    applicators: [
      createOutboundApplicator(() => ({
        async listOutbounds() {
          listCalls += 1;
          return [];
        },
        async removeOutbound() {
          assert.fail('removeOutbound should not be called');
        },
        async addOutbound() {
          return undefined;
        },
      })),
    ],
  };

  const firstReport = await syncWithConfig(loadedConfig, logger, memoryState, services);
  const secondReport = await syncWithConfig(loadedConfig, logger, memoryState, services);

  assert.equal(firstReport.failed, 0);
  assert.equal(secondReport.failed, 0);
  assert.equal(secondReport.unchanged, 1);
  assert.deepEqual(secondReport.targets[0]?.unchangedKinds, ['outbound']);
  assert.equal(listCalls, 1);

  await rm(tempDir, { recursive: true, force: true });
});

test('syncWithConfig applies one subscription to multiple targets independently', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-multi-target-'));
  const logger = createLogger({
    level: 'silent',
    format: 'json',
  });
  const memoryState = createSyncMemoryState();
  const loadedConfig: LoadedConfig = {
    configPath: join(tempDir, 'config.yml'),
    config: {
      subscriptions: [
        {
          id: 'source-1',
          input: resolve(tempDir, 'subscription.txt'),
          enabled: true,
          format: 'auto',
          fetchTimeoutMs: 5000,
          targets: [
            createTargetConfig({
              address: '127.0.0.1:8080',
              observatorySubjectSelectorPrefix: 'x-t1-',
            }),
            createTargetConfig({
              address: '127.0.0.1:8081',
              observatorySubjectSelectorPrefix: 'x-t2-',
            }),
          ],
        },
      ],
      runtime: {
        mode: 'run-once',
      },
      logging: {
        level: 'silent',
        format: 'json',
      },
      resources: createResourcesConfig(),
      statusServer: createStatusServerConfig(),
    },
  };

  const createdClients: string[] = [];
  const report = await syncWithConfig(loadedConfig, logger, memoryState, {
    async loadSubscriptionsFn() {
      return {
        loaded: [
          {
            id: 'source-1',
            input: resolve(tempDir, 'subscription.txt'),
            source: resolve(tempDir, 'subscription.txt'),
            content: 'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия',
            encoding: 'plain' as const,
            targets: loadedConfig.config.subscriptions[0]!.targets,
          },
        ],
        failed: [],
      };
    },
    applicators: [
      createOutboundApplicator((target) => {
        createdClients.push(target.address);
        return {
          async listOutbounds() {
            return [];
          },
          async removeOutbound() {
            return undefined;
          },
          async addOutbound() {
            return undefined;
          },
        };
      }),
    ],
  });

  assert.equal(report.failed, 0);
  assert.equal(report.targets.length, 2);
  assert.deepEqual(createdClients, ['127.0.0.1:8080', '127.0.0.1:8081']);
  assert.deepEqual(
    report.targets.map((item) => item.targetAddress),
    ['127.0.0.1:8080', '127.0.0.1:8081'],
  );

  await rm(tempDir, { recursive: true, force: true });
});

test('syncWithConfig skips overlapping runs inside one process', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-overlap-'));
  const logger = createLogger({
    level: 'silent',
    format: 'json',
  });
  const memoryState = createSyncMemoryState();
  const loadedConfig: LoadedConfig = {
    configPath: join(tempDir, 'config.yml'),
    config: {
      subscriptions: [
        {
          id: 'source-1',
          input: resolve(tempDir, 'subscription.txt'),
          enabled: true,
          format: 'auto',
          fetchTimeoutMs: 5000,
          targets: [
            createTargetConfig({
              observatorySubjectSelectorPrefix: 'x-observe-',
            }),
          ],
        },
      ],
      runtime: {
        mode: 'run-once',
      },
      logging: {
        level: 'silent',
        format: 'json',
      },
      resources: createResourcesConfig(),
      statusServer: createStatusServerConfig(),
    },
  };

  let release!: () => void;
  const blocker = new Promise<void>((resolvePromise) => {
    release = resolvePromise;
  });
  let loadCalls = 0;

  try {
    const firstRun = syncWithConfig(loadedConfig, logger, memoryState, {
      async loadSubscriptionsFn() {
        loadCalls += 1;
        await blocker;
        return {
          loaded: [],
          failed: [],
        };
      },
      applicators: [],
    });

    const overlapped = await syncWithConfig(loadedConfig, logger, memoryState, {
      async loadSubscriptionsFn() {
        assert.fail('overlap run must not execute loader');
      },
      applicators: [],
    });

    assert.equal(overlapped.failed, 0);
    assert.equal(overlapped.targets.length, 0);
    assert.equal(loadCalls, 1);

    release();
    await firstRun;
  } finally {
    await rm(tempDir, { recursive: true, force: true });
  }
});

test('syncWithConfig keeps other subscriptions running when one source load fails', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-source-fail-'));
  const logger = createLogger({
    level: 'silent',
    format: 'json',
  });
  const memoryState = createSyncMemoryState();
  const loadedConfig: LoadedConfig = {
    configPath: join(tempDir, 'config.yml'),
    config: {
      subscriptions: [
        {
          id: 'source-a',
          input: resolve(tempDir, 'source-a.txt'),
          enabled: true,
          format: 'auto',
          fetchTimeoutMs: 5000,
          targets: [
            createTargetConfig({
              address: '127.0.0.1:8080',
              observatorySubjectSelectorPrefix: 'x-a-',
            }),
          ],
        },
        {
          id: 'source-b',
          input: resolve(tempDir, 'source-b.txt'),
          enabled: true,
          format: 'auto',
          fetchTimeoutMs: 5000,
          targets: [
            createTargetConfig({
              address: '127.0.0.1:8081',
              observatorySubjectSelectorPrefix: 'x-b-',
            }),
          ],
        },
      ],
      runtime: {
        mode: 'run-once',
      },
      logging: {
        level: 'silent',
        format: 'json',
      },
      resources: createResourcesConfig(),
      statusServer: createStatusServerConfig(),
    },
  };

  const createdClients: string[] = [];
  const report = await syncWithConfig(loadedConfig, logger, memoryState, {
    async loadSubscriptionsFn() {
      return {
        loaded: [
          {
            id: 'source-b',
            input: resolve(tempDir, 'source-b.txt'),
            source: resolve(tempDir, 'source-b.txt'),
            content: 'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия',
            encoding: 'plain' as const,
            targets: loadedConfig.config.subscriptions[1]!.targets,
          },
        ],
        failed: [
          {
            id: 'source-a',
            input: resolve(tempDir, 'source-a.txt'),
            source: resolve(tempDir, 'source-a.txt'),
            error: 'Input source timed out',
            targets: loadedConfig.config.subscriptions[0]!.targets,
          },
        ],
      };
    },
    applicators: [
      createOutboundApplicator((target) => {
        createdClients.push(target.address);
        return {
          async listOutbounds() {
            return [];
          },
          async removeOutbound() {
            return undefined;
          },
          async addOutbound() {
            return undefined;
          },
        };
      }),
    ],
  });

  assert.equal(report.failed, 1);
  assert.equal(report.targets.length, 2);
  assert.equal(report.targets[0]?.targetAddress, '127.0.0.1:8080');
  assert.equal(report.targets[0]?.failed, 1);
  assert.equal(report.targets[1]?.targetAddress, '127.0.0.1:8081');
  assert.deepEqual(createdClients, ['127.0.0.1:8081']);

  await rm(tempDir, { recursive: true, force: true });
});

test('syncWithConfig does not call Xray API when manifest has no valid entries', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-no-valid-entries-'));
  const logger = createLogger({
    level: 'silent',
    format: 'json',
  });
  const memoryState = createSyncMemoryState();
  const loadedConfig: LoadedConfig = {
    configPath: join(tempDir, 'config.yml'),
    config: {
      subscriptions: [
        {
          id: 'source-invalid',
          input: resolve(tempDir, 'subscription.txt'),
          enabled: true,
          format: 'auto',
          fetchTimeoutMs: 5000,
          targets: [createTargetConfig()],
        },
        {
          id: 'source-valid',
          input: resolve(tempDir, 'subscription-valid.txt'),
          enabled: true,
          format: 'auto',
          fetchTimeoutMs: 5000,
          targets: [createTargetConfig({ address: '127.0.0.1:8081' })],
        },
      ],
      runtime: {
        mode: 'run-once',
      },
      logging: {
        level: 'silent',
        format: 'json',
      },
      resources: createResourcesConfig(),
      statusServer: createStatusServerConfig(),
    },
  };

  const createdClients: string[] = [];
  const report = await syncWithConfig(loadedConfig, logger, memoryState, {
    async loadSubscriptionsFn() {
      return {
        loaded: [
          {
            id: 'source-invalid',
            input: resolve(tempDir, 'subscription.txt'),
            source: resolve(tempDir, 'subscription.txt'),
            content: 'trojan://example',
            encoding: 'plain' as const,
            targets: loadedConfig.config.subscriptions[0]!.targets,
          },
          {
            id: 'source-valid',
            input: resolve(tempDir, 'subscription-valid.txt'),
            source: resolve(tempDir, 'subscription-valid.txt'),
            content: 'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия',
            encoding: 'plain' as const,
            targets: loadedConfig.config.subscriptions[1]!.targets,
          },
        ],
        failed: [],
      };
    },
    applicators: [
      createOutboundApplicator((target) => {
        createdClients.push(target.address);
        return {
          async listOutbounds() {
            return [];
          },
          async removeOutbound() {
            return undefined;
          },
          async addOutbound() {
            return undefined;
          },
        };
      }),
    ],
  });

  assert.equal(report.targets.length, 2);
  assert.equal(report.targets[0]?.targetAddress, '127.0.0.1:8080');
  assert.equal(report.targets[0]?.failed, 1);
  assert.equal(report.targets[1]?.targetAddress, '127.0.0.1:8081');
  assert.equal(report.targets[1]?.failed, 0);
  assert.deepEqual(createdClients, ['127.0.0.1:8081']);

  await rm(tempDir, { recursive: true, force: true });
});

test('syncWithConfig does not call Xray API when filters remove all entries', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-filtered-out-'));
  const logger = createLogger({
    level: 'silent',
    format: 'json',
  });
  const memoryState = createSyncMemoryState();
  const loadedConfig: LoadedConfig = {
    configPath: join(tempDir, 'config.yml'),
    config: {
      subscriptions: [
        {
          id: 'source-filtered',
          input: resolve(tempDir, 'subscription.txt'),
          enabled: true,
          format: 'auto',
          fetchTimeoutMs: 5000,
          filters: {
            countryAllowlist: ['US'],
          },
          targets: [createTargetConfig()],
        },
      ],
      runtime: {
        mode: 'run-once',
      },
      logging: {
        level: 'silent',
        format: 'json',
      },
      resources: createResourcesConfig(),
      statusServer: createStatusServerConfig(),
    },
  };

  const createdClients: string[] = [];
  const report = await syncWithConfig(loadedConfig, logger, memoryState, {
    async loadSubscriptionsFn() {
      return {
        loaded: [
          {
            id: 'source-filtered',
            input: resolve(tempDir, 'subscription.txt'),
            source: resolve(tempDir, 'subscription.txt'),
            content: 'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
            encoding: 'plain' as const,
            filters: loadedConfig.config.subscriptions[0]!.filters,
            targets: loadedConfig.config.subscriptions[0]!.targets,
          },
        ],
        failed: [],
      };
    },
    applicators: [
      createOutboundApplicator((target) => {
        createdClients.push(target.address);
        return {
          async listOutbounds() {
            return [];
          },
          async removeOutbound() {
            return undefined;
          },
          async addOutbound() {
            return undefined;
          },
        };
      }),
    ],
  });

  assert.equal(report.targets.length, 1);
  assert.equal(report.targets[0]?.failed, 1);
  assert.deepEqual(createdClients, []);

  await rm(tempDir, { recursive: true, force: true });
});

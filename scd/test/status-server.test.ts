import assert from 'node:assert/strict';
import test from 'node:test';

import { ApiRequestError } from '../src/errors.ts';
import { createSyncMemoryState } from '../src/runtime/run-state.ts';
import { handleStatusServerRequest } from '../src/runtime/status-server.ts';
import type { LoadedConfig } from '../src/types.ts';

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
          targets: [
            {
              address: '127.0.0.1:8080',
              timeoutMs: 5000,
              fixedOutbounds: [],
              fixedInbounds: [],
              fixedRouting: [],
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
            },
          ],
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
          target: loadedConfig.config.subscriptions[0]!.targets[0]!,
          resources: loadedConfig.config.resources,
        },
        serviceState: {
          balancerMonitor: {
            state: 'healthy',
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

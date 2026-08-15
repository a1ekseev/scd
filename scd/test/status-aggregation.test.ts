import assert from 'node:assert/strict';
import test from 'node:test';

import {
  buildAggregatedStatusSnapshot,
  collectRemoteStatus,
  createStatusAggregationState,
} from '../src/runtime/status-aggregation.ts';
import type { InstanceStatusSnapshot, RemoteStatusInstanceConfig } from '../src/types.ts';

const remote: RemoteStatusInstanceConfig = {
  id: 'edge-eu',
  apiUrl: 'https://edge.example.test/api/instance-status',
  schedule: '*/2 * * * *',
  timeoutMs: 1000,
};

const snapshot: InstanceStatusSnapshot = {
  version: 1,
  instanceId: 'edge-eu-api',
  capturedAt: '2026-08-15T10:00:00.000Z',
  subscriptions: [],
  tunnels: [],
};

const logger = {
  info() {},
  warn() {},
};

test('remote collector stores the latest successful status snapshot', async () => {
  const state = createStatusAggregationState([remote]);
  let nowIndex = 0;
  const dates = [new Date('2026-08-15T10:00:01.000Z'), new Date('2026-08-15T10:00:02.000Z')];

  await collectRemoteStatus(remote, state, logger as never, {
    nowFn: () => dates[nowIndex++]!,
    fetchFn: async () => new Response(JSON.stringify(snapshot), {
      status: 200,
      headers: { 'content-type': 'application/json' },
    }),
  });

  assert.deepEqual(state.remotes['edge-eu']?.snapshot, snapshot);
  assert.equal(state.remotes['edge-eu']?.lastAttemptAt, '2026-08-15T10:00:01.000Z');
  assert.equal(state.remotes['edge-eu']?.lastSuccessAt, '2026-08-15T10:00:02.000Z');
  assert.equal(state.remotes['edge-eu']?.lastError, undefined);
});

test('remote collector keeps the last successful snapshot after an error', async () => {
  const state = createStatusAggregationState([remote]);
  state.remotes['edge-eu']!.snapshot = snapshot;
  state.remotes['edge-eu']!.lastSuccessAt = '2026-08-15T10:00:02.000Z';

  await collectRemoteStatus(remote, state, logger as never, {
    nowFn: () => new Date('2026-08-15T10:02:00.000Z'),
    fetchFn: async () => new Response('unavailable', { status: 503 }),
  });

  const aggregated = buildAggregatedStatusSnapshot(state);
  assert.deepEqual(aggregated.instances[0]?.snapshot, snapshot);
  assert.equal(aggregated.instances[0]?.lastSuccessAt, '2026-08-15T10:00:02.000Z');
  assert.match(aggregated.instances[0]?.lastError ?? '', /HTTP 503/);
});

test('remote collector rejects incompatible API payloads without replacing state', async () => {
  const state = createStatusAggregationState([remote]);

  await collectRemoteStatus(remote, state, logger as never, {
    fetchFn: async () => new Response(JSON.stringify({ tunnels: [] }), { status: 200 }),
  });

  assert.equal(state.remotes['edge-eu']?.snapshot, undefined);
  assert.match(state.remotes['edge-eu']?.lastError ?? '', /unsupported schema/);
});

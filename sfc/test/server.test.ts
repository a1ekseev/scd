import assert from 'node:assert/strict';
import test from 'node:test';

import { handleServerRequest } from '../src/runtime/server.ts';
import type { AppState } from '../src/types.ts';

function createState(): AppState {
  return {
    refreshInProgress: false,
    outputs: {
      available: {
        id: 'available',
        subscriptionId: 'main',
        regex: '/Extra/i',
        lastGoodBase64: 'dmxlc3M6Ly9leGFtcGxl',
        lastGoodPlain: 'vless://example',
        lastGoodLineCount: 1,
      },
      unavailable: {
        id: 'unavailable',
        subscriptionId: 'main',
        regex: '/Extra/i',
      },
    },
  };
}

test('server returns healthz', () => {
  const response = handleServerRequest('/healthz', createState());
  assert.equal(response.statusCode, 200);
  assert.equal(response.body, 'ok\n');
});

test('server returns 404 for unknown output id', () => {
  const response = handleServerRequest('/s/missing', createState());
  assert.equal(response.statusCode, 404);
});

test('server returns 502 when no successful cache exists yet', () => {
  const response = handleServerRequest('/s/unavailable', createState());
  assert.equal(response.statusCode, 502);
});

test('server returns cached base64 payload for successful output', () => {
  const response = handleServerRequest('/s/available', createState());
  assert.equal(response.statusCode, 200);
  assert.equal(response.body, 'dmxlc3M6Ly9leGFtcGxl');
});

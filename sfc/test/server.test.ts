import assert from 'node:assert/strict';
import test from 'node:test';

import { handleServerRequest } from '../src/runtime/server.ts';
import type { AppState } from '../src/types.ts';

function createState(): AppState {
  const available = {
    id: 'available',
    subscriptionId: 'main',
    pathRoute: '/profiles',
    regex: '/Extra/i',
    userAgent: ['Clash', 'Stash'],
    profileTitle: 'Available Profile',
    profileUpdateInterval: 6,
    lastGoodBase64: 'dmxlc3M6Ly9leGFtcGxl',
    lastGoodPlain: 'vless://example',
    lastGoodLineCount: 1,
  };
  const noUa = {
    id: 'no-ua',
    subscriptionId: 'main',
    pathRoute: '/profiles',
    regex: '/Extra/i',
    profileTitle: 'No UA Profile',
    profileUpdateInterval: 12,
    lastGoodBase64: 'bm8tdWE=',
  };
  const unavailable = {
    id: 'unavailable',
    subscriptionId: 'main',
    pathRoute: '/profiles',
    regex: '/Extra/i',
  };

  return {
    refreshInProgress: false,
    outputs: {
      available,
      'no-ua': noUa,
      unavailable,
    },
    outputsByPath: {
      '/profiles/available': available,
      '/profiles/no-ua': noUa,
      '/profiles/unavailable': unavailable,
    },
  };
}

test('server returns healthz', () => {
  const response = handleServerRequest('/healthz', createState());
  assert.equal(response.statusCode, 200);
  assert.equal(response.body, 'ok\n');
});

test('server returns 404 for unknown output id', () => {
  const response = handleServerRequest('/profiles/missing', createState());
  assert.equal(response.statusCode, 404);
});

test('server returns 502 when no successful cache exists yet', () => {
  const response = handleServerRequest('/profiles/unavailable', createState());
  assert.equal(response.statusCode, 502);
});

test('server returns cached base64 payload for successful output', () => {
  const response = handleServerRequest('/profiles/available', createState());
  assert.equal(response.statusCode, 200);
  assert.equal(response.body, 'dmxlc3M6Ly9leGFtcGxl');
  assert.equal(response.headers, undefined);
});

test('server adds profile headers when User-Agent matches configured prefix', () => {
  const response = handleServerRequest('/profiles/available', createState(), {
    'user-agent': 'ClashMeta/1.0',
  });
  assert.equal(response.statusCode, 200);
  assert.equal(response.headers?.['profile-title'], 'base64:QXZhaWxhYmxlIFByb2ZpbGU=');
  assert.equal(response.headers?.['profile-update-interval'], '6');
});

test('server does not add profile headers when User-Agent does not match', () => {
  const response = handleServerRequest('/profiles/available', createState(), {
    'user-agent': 'Shadowrocket/1.0',
  });
  assert.equal(response.statusCode, 200);
  assert.equal(response.headers, undefined);
});

test('server does not add profile headers when output has no userAgent config', () => {
  const response = handleServerRequest('/profiles/no-ua', createState(), {
    'user-agent': 'ClashMeta/1.0',
  });
  assert.equal(response.statusCode, 200);
  assert.equal(response.headers, undefined);
});

test('server returns 404 when pathRoute does not match output subscription route', () => {
  const response = handleServerRequest('/s/available', createState());
  assert.equal(response.statusCode, 404);
});

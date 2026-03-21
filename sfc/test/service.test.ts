import assert from 'node:assert/strict';
import test from 'node:test';
import { Buffer } from 'node:buffer';

import { loadConfig } from '../src/config/load-config.ts';
import { createAppState, refreshWithConfig } from '../src/runtime/refresh.ts';
import type { LoadedConfig } from '../src/types.ts';

function createLoadedConfig(input: string): LoadedConfig {
  return {
    configPath: '/tmp/config.yml',
    config: {
      subscriptions: [
        {
          id: 'main',
          input,
          enabled: true,
          format: 'auto',
          fetchTimeoutMs: 5000,
          outputs: [
            {
              id: 'out-germany',
              enabled: true,
              name: 'germany',
              labelIncludeRegex: '/Германия.*Extra/i',
            },
            {
              id: 'out-europe',
              enabled: true,
              name: 'europe',
              labelIncludeRegex: '/Extra$/i',
            },
          ],
        },
      ],
      runtime: {
        refreshSchedule: '*/10 * * * *',
      },
      server: {
        listen: '127.0.0.1:8081',
      },
      logging: {
        level: 'silent',
        format: 'json',
      },
    },
  };
}

test('loadConfig rejects invalid regex and duplicate output id', async () => {
  const tempPath = '/tmp/sfc-invalid-config.yml';
  await import('node:fs/promises').then(({ writeFile }) =>
    writeFile(
      tempPath,
      [
        'subscriptions:',
        '  - id: main',
        '    input: ./vpn',
        '    outputs:',
        '      - id: dup',
        '        labelIncludeRegex: "/broken"',
        '      - id: dup',
        '        labelIncludeRegex: "/ok/i"',
        'runtime:',
        '  refreshSchedule: "*/10 * * * *"',
        'server:',
        '  listen: 127.0.0.1:8081',
      ].join('\n'),
    ),
  );

  await assert.rejects(() => loadConfig(tempPath), /Duplicate output id|Regex literal/);
});

test('refresh filters plain remote source and preserves raw line order', async () => {
  const originalFetch = globalThis.fetch;
  let fetchCount = 0;
  globalThis.fetch = async () => {
    fetchCount += 1;
    return new Response(
      [
        'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf1@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия, Extra',
        'trojan://unsupported',
        'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf2@example.com:443?type=tcp&security=tls#🇳🇱 Амстердам, Нидерланды, Extra',
      ].join('\n'),
      { status: 200 },
    );
  };

  try {
    const loaded = createLoadedConfig('https://example.test/sub.txt');
    const state = createAppState(loaded.config);
    const report = await refreshWithConfig(loaded, state);

    assert.equal(fetchCount, 1);
    assert.equal(report.successful, 2);
    assert.equal(state.outputs['out-germany']?.lastGoodPlain, 'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf1@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия, Extra');
    assert.equal(
      state.outputs['out-europe']?.lastGoodPlain,
      [
        'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf1@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия, Extra',
        'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf2@example.com:443?type=tcp&security=tls#🇳🇱 Амстердам, Нидерланды, Extra',
      ].join('\n'),
    );
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('refresh decodes base64 input and repacks output to base64', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () =>
    new Response(
      Buffer.from(
        'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf1@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия, Extra',
        'utf8',
      ).toString('base64'),
      { status: 200 },
    );

  try {
    const loaded = createLoadedConfig('https://example.test/base64.txt');
    const state = createAppState(loaded.config);
    const report = await refreshWithConfig(loaded, state);
    assert.equal(report.successful, 2);
    assert.equal(
      Buffer.from(state.outputs['out-germany']?.lastGoodBase64 ?? '', 'base64').toString('utf8'),
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf1@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия, Extra',
    );
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('refresh keeps last good cache when upstream fetch fails', async () => {
  const originalFetch = globalThis.fetch;
  let shouldFail = false;
  globalThis.fetch = async () => {
    if (shouldFail) {
      throw new Error('network down');
    }
    return new Response(
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf1@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия, Extra',
      { status: 200 },
    );
  };

  try {
    const loaded = createLoadedConfig('https://example.test/sub.txt');
    const state = createAppState(loaded.config);
    await refreshWithConfig(loaded, state);
    const previous = state.outputs['out-germany']?.lastGoodBase64;

    shouldFail = true;
    const report = await refreshWithConfig(loaded, state);
    assert.equal(report.failed, 2);
    assert.equal(state.outputs['out-germany']?.lastGoodBase64, previous);
    assert.match(state.outputs['out-germany']?.lastError ?? '', /network down/);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('refresh keeps last good cache when filtered output becomes empty', async () => {
  const originalFetch = globalThis.fetch;
  let source = 'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf1@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия, Extra';
  globalThis.fetch = async () => new Response(source, { status: 200 });

  try {
    const loaded = createLoadedConfig('https://example.test/sub.txt');
    const state = createAppState(loaded.config);
    await refreshWithConfig(loaded, state);
    const previous = state.outputs['out-germany']?.lastGoodBase64;

    source = 'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf2@example.com:443?type=tcp&security=tls#🇳🇱 Амстердам, Нидерланды, Extra';
    const report = await refreshWithConfig(loaded, state);

    const germanyReport = report.outputs.find((item) => item.id === 'out-germany');
    assert.equal(germanyReport?.ok, false);
    assert.equal(germanyReport?.usedCachedValue, true);
    assert.equal(state.outputs['out-germany']?.lastGoodBase64, previous);
    assert.match(state.outputs['out-germany']?.lastError ?? '', /empty/);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

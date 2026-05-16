import assert from 'node:assert/strict';
import test from 'node:test';
import { Buffer } from 'node:buffer';
import { mkdir, mkdtemp, readdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { isAbsolute, join, relative, sep } from 'node:path';

import { loadConfig } from '../src/config/load-config.ts';
import { createAppState, refreshWithConfig } from '../src/runtime/refresh.ts';
import { buildSourceCacheDir, buildSourceCachePath, writeSourceCache } from '../src/runtime/source-cache.ts';
import type { LoadedConfig } from '../src/types.ts';

function createLoadedConfig(input: string, options: { configPath?: string; subscriptionId?: string } = {}): LoadedConfig {
  return {
    configPath: options.configPath ?? '/tmp/config.yml',
    config: {
      subscriptions: [
        {
          id: options.subscriptionId ?? 'main',
          input,
          enabled: true,
          format: 'auto',
          fetchTimeoutMs: 5000,
          pathRoute: '/s',
          outputs: [
            {
              id: 'out-germany',
              enabled: true,
              name: 'germany',
              labelIncludeRegex: '/Германия.*Extra/i',
              userAgent: ['Clash', 'Stash'],
              profileTitle: 'Germany Extra',
              profileUpdateInterval: 6,
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

async function createTempConfigPath(): Promise<{ dir: string; configPath: string }> {
  const dir = await mkdtemp(join(tmpdir(), 'sfc-test-'));
  return {
    dir,
    configPath: join(dir, 'config.yml'),
  };
}

test('loadConfig rejects invalid regex and duplicate output id', async () => {
  const tempPath = '/tmp/sfc-invalid-config.yml';
  await writeFile(
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
  );

  await assert.rejects(() => loadConfig(tempPath), /Duplicate output id|Regex literal/);
});

test('loadConfig parses pathRoute and rejects invalid values', async () => {
  const validPath = '/tmp/sfc-valid-path-route.yml';
  await writeFile(
    validPath,
    [
      'subscriptions:',
      '  - id: main',
      '    input: ./vpn',
      '    pathRoute: /profiles',
      '    outputs:',
      '      - id: good',
      '        labelIncludeRegex: "/ok/i"',
      'runtime:',
      '  refreshSchedule: "*/10 * * * *"',
      'server:',
      '  listen: 127.0.0.1:8081',
    ].join('\n'),
  );
  const loaded = await loadConfig(validPath);
  assert.equal(loaded.config.subscriptions[0]?.pathRoute, '/profiles');

  const missingPath = '/tmp/sfc-missing-path-route.yml';
  await writeFile(
    missingPath,
    [
      'subscriptions:',
      '  - id: main',
      '    input: ./vpn',
      '    outputs:',
      '      - id: good',
      '        labelIncludeRegex: "/ok/i"',
      'runtime:',
      '  refreshSchedule: "*/10 * * * *"',
      'server:',
      '  listen: 127.0.0.1:8081',
    ].join('\n'),
  );
  await assert.rejects(() => loadConfig(missingPath), /expected string, received undefined/);

  const invalidPath = '/tmp/sfc-invalid-path-route.yml';
  await writeFile(
    invalidPath,
    [
      'subscriptions:',
      '  - id: main',
      '    input: ./vpn',
      '    pathRoute: profiles/',
      '    outputs:',
      '      - id: bad',
      '        labelIncludeRegex: "/ok/i"',
      'runtime:',
      '  refreshSchedule: "*/10 * * * *"',
      'server:',
      '  listen: 127.0.0.1:8081',
    ].join('\n'),
  );
  await assert.rejects(() => loadConfig(invalidPath), /pathRoute/);
});

test('loadConfig validates userAgent list and profileUpdateInterval range', async () => {
  const invalidPath = '/tmp/sfc-invalid-profile-config.yml';
  await writeFile(
    invalidPath,
    [
      'subscriptions:',
      '  - id: main',
      '    input: ./vpn',
      '    outputs:',
      '      - id: bad-ua',
      '        labelIncludeRegex: "/ok/i"',
      '        userAgent:',
      '          - ""',
      '      - id: bad-interval',
      '        labelIncludeRegex: "/ok/i"',
      '        profileUpdateInterval: 25',
      'runtime:',
      '  refreshSchedule: "*/10 * * * *"',
      'server:',
      '  listen: 127.0.0.1:8081',
    ].join('\n'),
  );

  await assert.rejects(() => loadConfig(invalidPath), /Too small|Too big/);
});

test('loadConfig accepts output userAgent and profile headers config', async () => {
  const validPath = '/tmp/sfc-valid-profile-config.yml';
  await writeFile(
    validPath,
    [
      'subscriptions:',
      '  - id: main',
      '    input: ./vpn',
      '    pathRoute: /profiles',
      '    outputs:',
      '      - id: good',
      '        labelIncludeRegex: "/ok/i"',
      '        userAgent:',
      '          - Clash',
      '          - Stash',
      '        profileTitle: Main Profile',
      '        profileUpdateInterval: 24',
      'runtime:',
      '  refreshSchedule: "*/10 * * * *"',
      'server:',
      '  listen: 127.0.0.1:8081',
    ].join('\n'),
  );

  const loaded = await loadConfig(validPath);
  const output = loaded.config.subscriptions[0]?.outputs[0];
  assert.deepEqual(output?.userAgent, ['Clash', 'Stash']);
  assert.equal(output?.profileTitle, 'Main Profile');
  assert.equal(output?.profileUpdateInterval, 24);
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
    assert.equal(state.outputs['out-germany']?.pathRoute, '/s');
    assert.equal(state.outputsByPath['/s/out-germany']?.id, 'out-germany');
    assert.deepEqual(state.outputs['out-germany']?.userAgent, ['Clash', 'Stash']);
    assert.equal(state.outputs['out-germany']?.profileTitle, 'Germany Extra');
    assert.equal(state.outputs['out-germany']?.profileUpdateInterval, 6);
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

test('refresh writes source cache and uses it when upstream fetch fails', async () => {
  const originalFetch = globalThis.fetch;
  const { dir, configPath } = await createTempConfigPath();
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
    const loaded = createLoadedConfig('https://example.test/sub.txt', { configPath });
    const state = createAppState(loaded.config);
    await refreshWithConfig(loaded, state);
    const previous = state.outputs['out-germany']?.lastGoodBase64;
    const cachePath = buildSourceCachePath(configPath, 'main');
    assert.equal(
      await readFile(cachePath, 'utf8'),
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf1@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия, Extra',
    );

    shouldFail = true;
    const report = await refreshWithConfig(loaded, state);
    assert.equal(report.successful, 2);
    assert.equal(report.failed, 0);
    assert.equal(report.outputs.every((item) => item.usedSourceCache), true);
    assert.equal(state.outputs['out-germany']?.lastGoodBase64, previous);
    assert.equal(state.outputsByPath['/s/out-germany'], state.outputs['out-germany']);
    assert.equal(state.outputs['out-germany']?.lastError, undefined);
  } finally {
    globalThis.fetch = originalFetch;
    await rm(dir, { recursive: true, force: true });
  }
});

test('refresh keeps current failure behavior when upstream fetch fails and source cache is missing', async () => {
  const originalFetch = globalThis.fetch;
  const { dir, configPath } = await createTempConfigPath();
  const events: Array<{ event?: string; cachePath?: string; cacheError?: string; error?: string }> = [];
  globalThis.fetch = async () => {
    throw new Error('network down');
  };

  try {
    const loaded = createLoadedConfig('https://example.test/sub.txt', { configPath });
    const state = createAppState(loaded.config);
    const report = await refreshWithConfig(loaded, state, {
      info() {},
      warn() {},
      error(payload: { event?: string; cachePath?: string; cacheError?: string; error?: string }) {
        events.push(payload);
      },
    } as never);

    assert.equal(report.failed, 2);
    assert.equal(report.outputs.every((item) => item.usedSourceCache !== true), true);
    assert.match(state.outputs['out-germany']?.lastError ?? '', /network down/);
    const missed = events.find((event) => event.event === 'subscription_source_cache_missed');
    assert.equal(missed?.cachePath, buildSourceCachePath(configPath, 'main'));
    assert.match(missed?.cacheError ?? '', /ENOENT/);
    assert.match(missed?.error ?? '', /network down/);
  } finally {
    globalThis.fetch = originalFetch;
    await rm(dir, { recursive: true, force: true });
  }
});

test('refresh keeps last good cache when filtered output becomes empty', async () => {
  const originalFetch = globalThis.fetch;
  const { dir, configPath } = await createTempConfigPath();
  let source = 'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf1@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия, Extra';
  globalThis.fetch = async () => new Response(source, { status: 200 });

  try {
    const loaded = createLoadedConfig('https://example.test/sub.txt', { configPath });
    const state = createAppState(loaded.config);
    await refreshWithConfig(loaded, state);
    const previous = state.outputs['out-germany']?.lastGoodBase64;

    source = 'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf2@example.com:443?type=tcp&security=tls#🇳🇱 Амстердам, Нидерланды, Extra';
    const report = await refreshWithConfig(loaded, state);

    const germanyReport = report.outputs.find((item) => item.id === 'out-germany');
    assert.equal(germanyReport?.ok, false);
    assert.equal(germanyReport?.usedCachedValue, true);
    assert.equal(state.outputs['out-germany']?.lastGoodBase64, previous);
    assert.equal(state.outputsByPath['/s/out-germany'], state.outputs['out-germany']);
    assert.match(state.outputs['out-germany']?.lastError ?? '', /empty/);
    assert.equal(await readFile(buildSourceCachePath(configPath, 'main'), 'utf8'), source);
  } finally {
    globalThis.fetch = originalFetch;
    await rm(dir, { recursive: true, force: true });
  }
});

test('source cache path is scoped to config directory for unsafe subscription ids', async () => {
  const { dir, configPath } = await createTempConfigPath();
  try {
    const cacheDir = buildSourceCacheDir(configPath);
    const cachePath = buildSourceCachePath(configPath, '../main/slash id');
    const relativePath = relative(cacheDir, cachePath);

    assert.equal(cacheDir, join(dir, '.sfc-cache'));
    assert.equal(isAbsolute(relativePath), false);
    assert.equal(relativePath === '..' || relativePath.startsWith(`..${sep}`), false);
    assert.equal(cachePath.endsWith('..%2Fmain%2Fslash%20id.subscription'), true);

    const writtenPath = await writeSourceCache(configPath, '../main/slash id', 'cached content');
    assert.equal(writtenPath, cachePath);
    assert.equal(await readFile(cachePath, 'utf8'), 'cached content');
  } finally {
    await rm(dir, { recursive: true, force: true });
  }
});

test('writeSourceCache removes temp file when atomic write fails', async () => {
  const { dir, configPath } = await createTempConfigPath();
  const originalDateNow = Date.now;
  Date.now = () => 1234567890;

  try {
    const cachePath = buildSourceCachePath(configPath, 'main');
    const tempPath = `${cachePath}.${process.pid}.1234567890.tmp`;
    await mkdir(cachePath, { recursive: true });

    await assert.rejects(() => writeSourceCache(configPath, 'main', 'cached content'));
    await assert.rejects(() => readdir(tempPath), /ENOENT/);
  } finally {
    Date.now = originalDateNow;
    await rm(dir, { recursive: true, force: true });
  }
});

test('refresh keeps outputsByPath consistent across repeated successful refreshes', async () => {
  const originalFetch = globalThis.fetch;
  let source = 'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf1@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия, Extra';
  globalThis.fetch = async () => new Response(source, { status: 200 });

  try {
    const loaded = createLoadedConfig('https://example.test/sub.txt');
    const state = createAppState(loaded.config);

    await refreshWithConfig(loaded, state);
    const firstState = state.outputs['out-germany'];

    source = 'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf1@example.com:443?type=tcp&security=tls#🇩🇪 Франкфурт, Германия, Extra';
    await refreshWithConfig(loaded, state);

    assert.equal(state.outputsByPath['/s/out-germany'], state.outputs['out-germany']);
    assert.notEqual(state.outputs['out-germany'], firstState);
    assert.match(state.outputs['out-germany']?.lastGoodPlain ?? '', /Франкфурт/);
    assert.deepEqual(state.outputs['out-germany']?.userAgent, ['Clash', 'Stash']);
    assert.equal(state.outputs['out-germany']?.profileTitle, 'Germany Extra');
    assert.equal(state.outputs['out-germany']?.pathRoute, '/s');
  } finally {
    globalThis.fetch = originalFetch;
  }
});

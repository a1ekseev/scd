import test from 'node:test';
import assert from 'node:assert/strict';
import { Buffer } from 'node:buffer';
import { readFile } from 'node:fs/promises';
import { Readable } from 'node:stream';
import { mkdtemp, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { generateCommand } from '../src/commands/generate.ts';
import { applyOutbounds } from '../src/apply/apply-outbounds.ts';
import { buildOutboundGrpc } from '../src/builders/build-outbound-grpc.ts';
import { buildOutboundJson } from '../src/builders/build-outbound-json.ts';
import { extractCountryInfo } from '../src/flag-country-map/index.ts';
import { decodeSubscriptionContent, loadInputSource } from '../src/input/load-input.ts';
import { buildManifest } from '../src/manifest.ts';
import { normalizeVless } from '../src/normalize/normalize-vless.ts';
import { generateManifestFromSubscription } from '../src/runtime/generate-manifest-from-source.ts';
import { parseSubscriptionLine } from '../src/subscription/parse-subscription-line.ts';
import { scanLines } from '../src/subscription/scan-lines.ts';

const vpnPath = fileURLToPath(new URL('../../vpn', import.meta.url));

test('scanLines reads current vpn sample', async () => {
  const source = await readFile(vpnPath, 'utf8');
  const lines = scanLines(source);
  assert.ok(lines.length > 10);
});

test('buildManifest parses current vpn sample into supported profiles', async () => {
  const source = await readFile(vpnPath, 'utf8');
  const manifest = buildManifest(source, vpnPath);
  const profiles = new Set(manifest.entries.map((entry) => entry.profile));
  assert.ok(profiles.has('tcp-tls'));
  assert.ok(profiles.has('ws-tls'));
  assert.ok(profiles.has('tcp-reality-vision'));
  assert.equal(manifest.summary.parsed, manifest.entries.length);
});

test('generateManifestFromSubscription filters by country allowlist', () => {
  const { manifest } = generateManifestFromSubscription({
    id: 'source-1',
    input: 'inline',
    source: 'inline',
    encoding: 'plain',
    filters: {
      countryAllowlist: ['DE'],
    },
    targets: [],
    content: [
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf6@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия, Extra',
    ].join('\n'),
  });

  assert.equal(manifest.entries.length, 1);
  assert.equal(manifest.summary.parsed, 1);
  assert.equal(manifest.summary.filtered, 1);
  assert.equal(manifest.summary.filteredByCountry, 1);
  assert.equal(manifest.summary.filteredByLabelRegex, 0);
  assert.equal(manifest.entries[0]?.country?.iso2, 'DE');
});

test('generateManifestFromSubscription filters by label regex', () => {
  const { manifest } = generateManifestFromSubscription({
    id: 'source-1',
    input: 'inline',
    source: 'inline',
    encoding: 'plain',
    filters: {
      labelIncludeRegex: '/,\\s*Extra(?!\\s*Whitelist)\\s*$/i',
    },
    targets: [],
    content: [
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf6@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия, Extra Whitelist#2',
    ].join('\n'),
  });

  assert.equal(manifest.entries.length, 1);
  assert.equal(manifest.summary.parsed, 1);
  assert.equal(manifest.summary.filtered, 1);
  assert.equal(manifest.summary.filteredByCountry, 0);
  assert.equal(manifest.summary.filteredByLabelRegex, 1);
  assert.match(manifest.entries[0]?.label ?? '', /Extra$/);
});

test('generateManifestFromSubscription applies country and label filters sequentially', () => {
  const { manifest } = generateManifestFromSubscription({
    id: 'source-1',
    input: 'inline',
    source: 'inline',
    encoding: 'plain',
    filters: {
      countryAllowlist: ['DE', 'NL'],
      labelIncludeRegex: '/,\\s*Extra(?!\\s*Whitelist)\\s*$/i',
    },
    targets: [],
    content: [
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra',
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf6@example.com:443?type=tcp&security=tls#🇩🇪 Берлин, Германия, Extra',
      'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf7@example.com:443?type=tcp&security=tls#🇳🇱 Амстердам, Нидерланды, Extra Whitelist#2',
    ].join('\n'),
  });

  assert.equal(manifest.entries.length, 1);
  assert.equal(manifest.summary.parsed, manifest.entries.length);
  assert.equal(manifest.summary.filtered, 2);
  assert.equal(manifest.summary.filteredByCountry, 1);
  assert.equal(manifest.summary.filteredByLabelRegex, 1);
  assert.equal(manifest.entries[0]?.country?.iso2, 'DE');
  assert.match(manifest.entries[0]?.label ?? '', /Extra$/);
});

test('country map resolves Austria from emoji', () => {
  assert.deepEqual(extractCountryInfo('🇦🇹 Вена, Австрия, Extra'), {
    emoji: '🇦🇹',
    iso2: 'AT',
    nameEn: 'Austria',
    nameRu: 'Австрия',
  });
});

test('same country with different cities yields different tags', () => {
  const source = [
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия',
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Зальцбург, Австрия',
  ].join('\n');
  const manifest = buildManifest(source, 'inline');
  assert.equal(manifest.entries.length, 2);
  assert.notEqual(manifest.entries[0]?.tag, manifest.entries[1]?.tag);
});

test('non-vless scheme is skipped as unsupported', () => {
  const line = scanLines('trojan://example\n')[0]!;
  const parsed = parseSubscriptionLine(line);
  assert.equal(parsed.ok, false);
  if (!parsed.ok) {
    assert.equal(parsed.skipped.reasonCode, 'unsupported_scheme');
  }
});

test('unknown vless parameter is skipped', () => {
  const line = scanLines(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls&foo=bar#🇦🇹 Вена, Австрия',
  )[0]!;
  const parsed = parseSubscriptionLine(line);
  assert.equal(parsed.ok, false);
  if (!parsed.ok) {
    assert.equal(parsed.skipped.reasonCode, 'unsupported_param');
  }
});

test('base64 subscription content is decoded before parsing', () => {
  const encoded = Buffer.from(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия',
    'utf8',
  ).toString('base64');
  const decoded = decodeSubscriptionContent(encoded);
  assert.match(decoded, /^vless:\/\//);
});

test('loadInputSource downloads from URL and decodes base64', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (_input, init) => {
    assert.equal(init?.headers, undefined);
    assert.ok(init?.signal instanceof AbortSignal);
    return new Response(
      Buffer.from(
        'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия',
        'utf8',
      ).toString('base64'),
      {
        status: 200,
        headers: { 'content-type': 'text/plain' },
      },
    );
  };

  try {
    const loaded = await loadInputSource('https://example.com/subscription.txt');
    assert.equal(loaded.source, 'https://example.com/subscription.txt');
    assert.equal(loaded.encoding, 'base64');
    assert.match(loaded.content, /^vless:\/\//);
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('loadInputSource rejects timed out remote content', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async (_input, init) =>
    await new Promise<Response>((_resolve, reject) => {
      init?.signal?.addEventListener('abort', () => {
        reject(new Error('aborted'));
      });
    });

  try {
    await assert.rejects(
      () => loadInputSource('https://example.com/hang.txt', { fetchTimeoutMs: 10 }),
      /timed out after 10ms/,
    );
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('loadInputSource rejects empty remote content', async () => {
  const originalFetch = globalThis.fetch;
  globalThis.fetch = async () =>
    new Response('', {
      status: 200,
      headers: { 'content-type': 'text/plain' },
    });

  try {
    await assert.rejects(
      () => loadInputSource('https://example.com/empty.txt'),
      /returned empty content/,
    );
  } finally {
    globalThis.fetch = originalFetch;
  }
});

test('loadInputSource rejects empty local file content', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-empty-file-'));
  const inputPath = join(tempDir, 'empty.txt');
  await writeFile(inputPath, '   \n');

  try {
    await assert.rejects(() => loadInputSource(inputPath), /returned empty content/);
  } finally {
    await rm(tempDir, { recursive: true, force: true });
  }
});

test('loadInputSource plain file does not depend on fetch timeout', async () => {
  const loaded = await loadInputSource(vpnPath, { fetchTimeoutMs: 1 });
  assert.equal(loaded.source, vpnPath);
  assert.match(loaded.content, /^vless:\/\//);
});

test('loadInputSource reads from stdin and decodes base64', async () => {
  const originalStdin = process.stdin;
  const encoded = Buffer.from(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия',
    'utf8',
  ).toString('base64');

  Object.defineProperty(process, 'stdin', {
    configurable: true,
    value: Readable.from([encoded]),
  });

  try {
    const loaded = await loadInputSource('-');
    assert.equal(loaded.source, 'stdin');
    assert.equal(loaded.encoding, 'base64');
    assert.match(loaded.content, /^vless:\/\//);
  } finally {
    Object.defineProperty(process, 'stdin', {
      configurable: true,
      value: originalStdin,
    });
  }
});

test('loadInputSource rejects empty stdin content', async () => {
  const originalStdin = process.stdin;

  Object.defineProperty(process, 'stdin', {
    configurable: true,
    value: Readable.from(['   \n']),
  });

  try {
    await assert.rejects(() => loadInputSource('-'), /returned empty content/);
  } finally {
    Object.defineProperty(process, 'stdin', {
      configurable: true,
      value: originalStdin,
    });
  }
});

test('unsupported vless combination is skipped', () => {
  const line = scanLines(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=grpc&security=tls#🇦🇹 Вена, Австрия',
  )[0]!;
  const parsed = parseSubscriptionLine(line);
  assert.equal(parsed.ok, true);
  if (parsed.ok) {
    const normalized = normalizeVless(parsed);
    assert.ok('reasonCode' in normalized);
    if ('reasonCode' in normalized) {
      assert.equal(normalized.reasonCode, 'unsupported_combo');
    }
  }
});

test('buildManifest ignores trailing empty lines', () => {
  const manifest = buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия\n',
    'inline',
  );
  assert.equal(manifest.summary.totalLines, 1);
  assert.equal(manifest.summary.parsed, 1);
  assert.equal(manifest.summary.skipped, 0);
});

test('builders produce JSON and gRPC payload for ws+tls', () => {
  const line = scanLines(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?encryption=none&type=ws&fp=chrome&sni=cdn.example.net&host=edge.example.net&path=%2Fstream&security=tls&allowInsecure=1#🇩🇪 Германия, Extra',
  )[0]!;
  const parsed = parseSubscriptionLine(line);
  assert.equal(parsed.ok, true);
  if (!parsed.ok) {
    return;
  }
  const normalized = normalizeVless(parsed);
  assert.ok(!('reasonCode' in normalized));
  if ('reasonCode' in normalized) {
    return;
  }
  const json = buildOutboundJson(normalized);
  const grpc = buildOutboundGrpc(normalized);
  assert.equal(json.streamSettings.network, 'ws');
  assert.equal(json.streamSettings.security, 'tls');
  assert.ok(grpc.raw.length > 0);
  const grpcText = Buffer.from(grpc.raw).toString('utf8');
  assert.match(grpcText, /websocket/);
  assert.match(grpcText, /xray\.transport\.internet\.tls\.Config/);
});

test('grpc payload uses reality type names expected by Xray', () => {
  const line = scanLines(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?security=reality&encryption=none&fp=chrome&headerType=none&type=tcp&flow=xtls-rprx-vision&sni=io.ozone.ru&pbk=CMkW1axrhEXoiJ6anMz9XEjlfqlAtEZya7L0b5ZPMyw&sid=abe4a59b9f2407e3#🇩🇪 Германия, Extra',
  )[0]!;
  const parsed = parseSubscriptionLine(line);
  assert.equal(parsed.ok, true);
  if (!parsed.ok) {
    return;
  }
  const normalized = normalizeVless(parsed);
  assert.ok(!('reasonCode' in normalized));
  if ('reasonCode' in normalized) {
    return;
  }
  const grpc = buildOutboundGrpc(normalized);
  const grpcText = Buffer.from(grpc.raw).toString('utf8');
  assert.match(grpcText, /xray\.transport\.internet\.reality\.Config/);
  assert.doesNotMatch(grpcText, /\bws\b/);
});

test('applyOutbounds replaces existing tags', async () => {
  const manifest = buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия',
    'inline',
  );
  const calls: string[] = [];
  const client = {
    async listOutbounds() {
      return [{ tag: manifest.entries[0]!.tag, raw: new Uint8Array([1, 2, 3]) }];
    },
    async removeOutbound(tag: string) {
      calls.push(`remove:${tag}`);
    },
    async addOutbound(rawOutbound: Uint8Array) {
      calls.push(`add:${rawOutbound.length}`);
    },
  };

  const report = await applyOutbounds(manifest, client, 'inline', {
    fixedOutbounds: [],
  });
  assert.equal(report.items[0]?.status, 'removed');
  assert.equal(report.items[1]?.status, 'replaced');
  assert.equal(calls[0], `remove:${manifest.entries[0]!.tag}`);
  assert.match(calls[1] ?? '', /^add:/);
});

test('applyOutbounds rolls back on add failure', async () => {
  const manifest = buildManifest(
    'vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@example.com:443?type=tcp&security=tls#🇦🇹 Вена, Австрия',
    'inline',
  );
  const rollbackRaw = new Uint8Array([9, 9, 9]);
  const calls: string[] = [];
  let addCallCount = 0;
  const client = {
    async listOutbounds() {
      return [{ tag: manifest.entries[0]!.tag, raw: rollbackRaw }];
    },
    async removeOutbound(tag: string) {
      calls.push(`remove:${tag}`);
    },
    async addOutbound(rawOutbound: Uint8Array) {
      addCallCount += 1;
      calls.push(`add:${rawOutbound[0] ?? 0}`);
      if (addCallCount === 1) {
        throw new Error('boom');
      }
    },
  };

  const report = await applyOutbounds(manifest, client, 'inline', {
    fixedOutbounds: [],
  });
  assert.equal(report.items.at(-1)?.status, 'failed');
  assert.equal(calls[0], `remove:${manifest.entries[0]!.tag}`);
  assert.deepEqual(calls.slice(-1), ['add:9']);
});

test('generateCommand rejects manifest with no valid entries', async () => {
  const tempDir = await mkdtemp(join(tmpdir(), 'scd-generate-fail-'));
  const inputPath = join(tempDir, 'subscription.txt');
  const outputPath = join(tempDir, 'manifest.json');
  const logPath = join(tempDir, 'parse.log.json');
  await writeFile(inputPath, 'trojan://example\n');

  try {
    await assert.rejects(
      () =>
        generateCommand({
          input: inputPath,
          output: outputPath,
          log: logPath,
        }),
      /produced no valid outbound entries/,
    );
  } finally {
    await rm(tempDir, { recursive: true, force: true });
  }
});

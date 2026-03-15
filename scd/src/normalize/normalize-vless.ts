import { createHash } from 'node:crypto';

import { extractCountryInfo } from '../flag-country-map/index.ts';
import type { NormalizedOutbound, ParseSuccess, ProfileKind, SkippedEntry } from '../types.ts';

function boolFromString(value: string | undefined): boolean | undefined {
  if (value === undefined || value === '') {
    return undefined;
  }
  if (value === '1' || value.toLowerCase() === 'true') {
    return true;
  }
  if (value === '0' || value.toLowerCase() === 'false') {
    return false;
  }
  return undefined;
}

function parseAlpn(value: string | undefined): string[] {
  if (!value) {
    return [];
  }
  return value
    .split(',')
    .map((item) => item.trim())
    .filter(Boolean);
}

function slugify(value: string): string {
  return value
    .normalize('NFKD')
    .replace(/[\u0300-\u036f]/g, '')
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 32);
}

function buildShortHash(raw: string): string {
  return createHash('sha256').update(raw).digest('hex').slice(0, 8);
}

function inferCity(label: string): string | undefined {
  const withoutFlag = label.replace(/^[\p{Regional_Indicator}\s]+/u, '').trim();
  if (!withoutFlag) {
    return undefined;
  }
  const [head] = withoutFlag.split(',');
  const city = head.trim();
  return city && !/extra/i.test(city) ? city : undefined;
}

function determineProfile(parse: ParseSuccess): ProfileKind | undefined {
  const network = parse.params.type?.toLowerCase() ?? 'tcp';
  const security = parse.params.security?.toLowerCase() ?? '';
  const flow = parse.params.flow ?? '';

  if (network === 'tcp' && security === 'tls') {
    return 'tcp-tls';
  }
  if (network === 'ws' && security === 'tls') {
    return 'ws-tls';
  }
  if (network === 'tcp' && security === 'reality' && flow === 'xtls-rprx-vision') {
    return 'tcp-reality-vision';
  }
  return undefined;
}

function buildTag(parse: ParseSuccess, profile: ProfileKind): string {
  const country = extractCountryInfo(parse.label)?.iso2.toLowerCase() ?? 'xx';
  const inferredCity = inferCity(parse.label);
  const city =
    slugify(inferredCity ?? '') ||
    (inferredCity ? buildShortHash(inferredCity) : '') ||
    slugify(parse.address) ||
    'endpoint';
  const hash = buildShortHash(parse.raw);
  return ['vless', country, city, profile, hash].join('-');
}

export function normalizeVless(parse: ParseSuccess): NormalizedOutbound | SkippedEntry {
  const profile = determineProfile(parse);
  if (!profile) {
    return {
      line: parse.line,
      raw: parse.raw,
      label: parse.label,
      reasonCode: 'unsupported_combo',
      reason: 'Unsupported VLESS type/security/flow combination.',
      details: [
        `type=${parse.params.type ?? 'tcp'}`,
        `security=${parse.params.security ?? ''}`,
        `flow=${parse.params.flow ?? ''}`,
      ],
    };
  }

  const encryption = parse.params.encryption;
  if (encryption !== undefined && encryption !== '' && encryption !== 'none') {
    return {
      line: parse.line,
      raw: parse.raw,
      label: parse.label,
      reasonCode: 'unsupported_value',
      reason: 'Unsupported VLESS encryption value.',
      details: [`encryption=${encryption}`],
    };
  }

  if (profile === 'ws-tls' && !parse.params.path) {
    return {
      line: parse.line,
      raw: parse.raw,
      label: parse.label,
      reasonCode: 'missing_required_field',
      reason: 'WebSocket VLESS requires a path.',
    };
  }

  if (profile === 'tcp-reality-vision') {
    for (const required of ['sni', 'fp', 'pbk', 'sid']) {
      if (!parse.params[required]) {
        return {
          line: parse.line,
          raw: parse.raw,
          label: parse.label,
          reasonCode: 'missing_required_field',
          reason: `REALITY VLESS requires parameter "${required}".`,
        };
      }
    }
  }

  const allowInsecure = boolFromString(parse.params.allowinsecure);
  if (parse.params.allowinsecure !== undefined && allowInsecure === undefined) {
    return {
      line: parse.line,
      raw: parse.raw,
      label: parse.label,
      reasonCode: 'unsupported_value',
      reason: 'allowInsecure must be 0, 1, true, or false.',
      details: [`allowInsecure=${parse.params.allowinsecure}`],
    };
  }

  const headerType = parse.params.headertype ?? undefined;
  if (headerType !== undefined && headerType !== '' && headerType !== 'none') {
    return {
      line: parse.line,
      raw: parse.raw,
      label: parse.label,
      reasonCode: 'unsupported_value',
      reason: 'Only headerType=none is supported in v1.',
      details: [`headerType=${headerType}`],
    };
  }

  const country = extractCountryInfo(parse.label);
  const city = inferCity(parse.label);

  return {
    kind: 'outbound',
    protocol: 'vless',
    profile,
    tag: buildTag(parse, profile),
    line: parse.line,
    raw: parse.raw,
    label: parse.label,
    address: parse.address,
    port: parse.port,
    uuid: parse.uuid,
    encryption: 'none',
    network: profile === 'ws-tls' ? 'ws' : 'tcp',
    security: profile === 'tcp-reality-vision' ? 'reality' : 'tls',
    flow: parse.params.flow || undefined,
    alpn: parseAlpn(parse.params.alpn),
    sni: parse.params.sni || undefined,
    host: parse.params.host || undefined,
    path: parse.params.path || undefined,
    fingerprint: parse.params.fp || undefined,
    allowInsecure,
    headerType,
    publicKey: parse.params.pbk || undefined,
    shortId: parse.params.sid || undefined,
    country,
    city,
    query: parse.params,
  };
}

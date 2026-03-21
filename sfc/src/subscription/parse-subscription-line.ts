import type { ParsedSubscriptionEntry, SourceLine } from '../types.ts';

const SUPPORTED_SCHEME = 'vless:';
const SUPPORTED_PARAMS = new Set([
  'type',
  'path',
  'security',
  'alpn',
  'encryption',
  'fp',
  'sni',
  'flow',
  'host',
  'allowinsecure',
  'headertype',
  'pbk',
  'sid',
]);

function validateUuid(uuid: string): boolean {
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(uuid);
}

export function parseSubscriptionLine(line: SourceLine): ParsedSubscriptionEntry | undefined {
  if (!line.trimmed || !line.trimmed.includes('://')) {
    return undefined;
  }

  let uri: URL;
  try {
    uri = new URL(line.trimmed);
  } catch {
    return undefined;
  }

  if (uri.protocol !== SUPPORTED_SCHEME) {
    return undefined;
  }

  const label = decodeURIComponent(uri.hash.startsWith('#') ? uri.hash.slice(1) : uri.hash);
  const uuid = decodeURIComponent(uri.username);
  if (!validateUuid(uuid)) {
    return undefined;
  }

  const address = uri.hostname;
  const port = uri.port ? Number(uri.port) : 0;
  if (!address || !Number.isInteger(port) || port <= 0 || port > 65535) {
    return undefined;
  }

  const params = new Set(Array.from(uri.searchParams.keys(), (key) => key.toLowerCase()));
  for (const key of params) {
    if (!SUPPORTED_PARAMS.has(key)) {
      return undefined;
    }
  }

  return {
    line: line.line,
    raw: line.raw,
    label,
  };
}

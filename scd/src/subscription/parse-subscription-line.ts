import type { ParseFailure, ParseResult, ParseSuccess, SkipReasonCode, SourceLine } from '../types.ts';

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

function failure(
  line: SourceLine,
  reasonCode: SkipReasonCode,
  reason: string,
  label?: string,
  details?: string[],
): ParseFailure {
  return {
    ok: false,
    skipped: {
      line: line.line,
      raw: line.raw,
      label,
      reasonCode,
      reason,
      details,
    },
  };
}

function validateUuid(uuid: string): boolean {
  return /^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/i.test(uuid);
}

export function parseSubscriptionLine(line: SourceLine): ParseResult {
  if (!line.trimmed) {
    return failure(line, 'empty_line', 'The line is empty.');
  }

  if (!line.trimmed.includes('://')) {
    return failure(line, 'unsupported_scheme', 'The line does not contain a supported URI scheme.');
  }

  let uri: URL;
  try {
    uri = new URL(line.trimmed);
  } catch {
    return failure(line, 'invalid_uri', 'The line is not a valid URI.');
  }

  if (uri.protocol !== SUPPORTED_SCHEME) {
    return failure(line, 'unsupported_scheme', `Unsupported scheme "${uri.protocol}".`);
  }

  const label = decodeURIComponent(uri.hash.startsWith('#') ? uri.hash.slice(1) : uri.hash);
  const uuid = decodeURIComponent(uri.username);
  if (!validateUuid(uuid)) {
    return failure(line, 'missing_required_field', 'The VLESS URI does not contain a valid UUID.', label);
  }

  const address = uri.hostname;
  const port = uri.port ? Number(uri.port) : 0;
  if (!address || !Number.isInteger(port) || port <= 0 || port > 65535) {
    return failure(line, 'missing_required_field', 'The VLESS URI does not contain a valid address and port.', label);
  }

  const params: Record<string, string> = {};
  for (const [rawKey, value] of uri.searchParams.entries()) {
    const key = rawKey.toLowerCase();
    params[key] = value;
  }

  const unknownParams = Object.keys(params).filter((key) => !SUPPORTED_PARAMS.has(key));
  if (unknownParams.length > 0) {
    return failure(
      line,
      'unsupported_param',
      `Unsupported VLESS parameter(s): ${unknownParams.join(', ')}.`,
      label,
      unknownParams,
    );
  }

  const success: ParseSuccess = {
    ok: true,
    kind: 'outbound',
    protocol: 'vless',
    line: line.line,
    raw: line.raw,
    uri,
    label,
    uuid,
    address,
    port,
    params,
  };

  return success;
}

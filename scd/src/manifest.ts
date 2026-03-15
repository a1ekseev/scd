import { buildOutboundJson } from './builders/build-outbound-json.ts';
import { normalizeVless } from './normalize/normalize-vless.ts';
import { parseSubscriptionLine } from './subscription/parse-subscription-line.ts';
import { scanLines } from './subscription/scan-lines.ts';
import type { ManifestSummary, OutboundManifest, SkippedEntry } from './types.ts';

function createSummary(totalLines: number): ManifestSummary {
  return {
    totalLines,
    parsed: 0,
    skipped: 0,
    filtered: 0,
    filteredByCountry: 0,
    filteredByLabelRegex: 0,
    unsupportedScheme: 0,
    unsupportedParam: 0,
    unsupportedValue: 0,
    unsupportedCombo: 0,
    invalidUri: 0,
    missingRequiredField: 0,
  };
}

function increment(summary: ManifestSummary, skipped: SkippedEntry) {
  summary.skipped += 1;
  if (skipped.reasonCode === 'unsupported_scheme') {
    summary.unsupportedScheme += 1;
  }
  if (skipped.reasonCode === 'unsupported_param') {
    summary.unsupportedParam += 1;
  }
  if (skipped.reasonCode === 'unsupported_value') {
    summary.unsupportedValue += 1;
  }
  if (skipped.reasonCode === 'unsupported_combo') {
    summary.unsupportedCombo += 1;
  }
  if (skipped.reasonCode === 'invalid_uri') {
    summary.invalidUri += 1;
  }
  if (skipped.reasonCode === 'missing_required_field') {
    summary.missingRequiredField += 1;
  }
}

export function buildManifest(text: string, sourceFile: string): OutboundManifest {
  const lines = scanLines(text);
  const summary = createSummary(lines.length);
  const entries: OutboundManifest['entries'] = [];
  const skipped: SkippedEntry[] = [];

  for (const line of lines) {
    const parsed = parseSubscriptionLine(line);
    if (!parsed.ok) {
      skipped.push(parsed.skipped);
      increment(summary, parsed.skipped);
      continue;
    }

    const normalized = normalizeVless(parsed);
    if ('reasonCode' in normalized) {
      skipped.push(normalized);
      increment(summary, normalized);
      continue;
    }

    const jsonOutbound = buildOutboundJson(normalized);
    entries.push({
      kind: normalized.kind,
      tag: normalized.tag,
      label: normalized.label,
      profile: normalized.profile,
      line: normalized.line,
      country: normalized.country,
      city: normalized.city,
      normalized,
      jsonOutbound,
    });
    summary.parsed += 1;
  }

  return {
    version: 1,
    generatedAt: new Date().toISOString(),
    sourceFile,
    entries,
    skipped,
    summary,
  };
}

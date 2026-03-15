import type { OutboundManifest, SkippedEntry } from '../types.ts';

export interface ParseLog {
  generatedAt: string;
  summary: OutboundManifest['summary'];
  skipped: SkippedEntry[];
}

export function buildParseLog(manifest: OutboundManifest): ParseLog {
  return {
    generatedAt: manifest.generatedAt,
    summary: manifest.summary,
    skipped: manifest.skipped,
  };
}

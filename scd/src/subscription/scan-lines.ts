import type { SourceLine } from '../types.ts';

export function scanLines(text: string): SourceLine[] {
  return text
    .split(/\r?\n/)
    .map((raw, index) => ({
      line: index + 1,
      raw,
      trimmed: raw.trim(),
    }))
    .filter((line) => line.trimmed.length > 0);
}

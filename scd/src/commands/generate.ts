import { writeFile } from 'node:fs/promises';
import { resolve } from 'node:path';

import { loadInputSource } from '../input/load-input.ts';
import { buildParseLog } from '../logging/format.ts';
import { buildManifest } from '../manifest.ts';
import { validateManifestOrThrow } from '../runtime/generate-manifest-from-source.ts';
import type { GenerateOptions } from '../types.ts';

export async function generateCommand(options: GenerateOptions) {
  const outputPath = resolve(options.output);
  const logPath = resolve(options.log);
  const loaded = await loadInputSource(options.input);
  const manifest = validateManifestOrThrow(buildManifest(loaded.content, loaded.source), loaded.source);

  await writeFile(outputPath, JSON.stringify(manifest, null, 2));
  await writeFile(logPath, JSON.stringify(buildParseLog(manifest), null, 2));

  return manifest;
}

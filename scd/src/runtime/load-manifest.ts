import { readFile } from 'node:fs/promises';
import { resolve } from 'node:path';

import type { OutboundManifest } from '../types.ts';

export async function loadManifest(input: string): Promise<OutboundManifest> {
  const inputPath = resolve(input);
  return JSON.parse(await readFile(inputPath, 'utf8')) as OutboundManifest;
}

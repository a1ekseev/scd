import { mkdir, readFile, rename, unlink, writeFile } from 'node:fs/promises';
import { dirname, join, resolve } from 'node:path';

const CACHE_DIR_NAME = '.sfc-cache';
const CACHE_FILE_EXTENSION = '.subscription';

export function buildSourceCacheDir(configPath: string): string {
  return join(dirname(resolve(configPath)), CACHE_DIR_NAME);
}

export function buildSourceCachePath(configPath: string, subscriptionId: string): string {
  return join(buildSourceCacheDir(configPath), `${encodeURIComponent(subscriptionId)}${CACHE_FILE_EXTENSION}`);
}

export async function writeSourceCache(configPath: string, subscriptionId: string, content: string): Promise<string> {
  const cacheDir = buildSourceCacheDir(configPath);
  await mkdir(cacheDir, { recursive: true });

  const cachePath = buildSourceCachePath(configPath, subscriptionId);
  const tempPath = `${cachePath}.${process.pid}.${Date.now()}.tmp`;
  try {
    await writeFile(tempPath, content, 'utf8');
    await rename(tempPath, cachePath);
  } catch (error) {
    await unlink(tempPath).catch(() => undefined);
    throw error;
  }
  return cachePath;
}

export async function readSourceCache(configPath: string, subscriptionId: string): Promise<{ path: string; content: string }> {
  const cachePath = buildSourceCachePath(configPath, subscriptionId);
  const content = await readFile(cachePath, 'utf8');
  return {
    path: cachePath,
    content,
  };
}

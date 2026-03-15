import { Buffer } from 'node:buffer';
import { readFile } from 'node:fs/promises';
import { resolve } from 'node:path';

import { InputError } from '../errors.ts';
import type { SubscriptionInputFormat } from '../types.ts';

export interface LoadedInput {
  source: string;
  content: string;
  encoding: 'plain' | 'base64';
}

export interface LoadInputOptions {
  format?: SubscriptionInputFormat;
  fetchTimeoutMs?: number;
}

function isStdinInput(value: string): boolean {
  return value === '-';
}

function ensureNonEmptyContent(content: string, source: string): string {
  if (!content.trim()) {
    throw new InputError('EMPTY_INPUT', `Input source ${source} returned empty content.`);
  }
  return content;
}

async function downloadRemoteContent(input: string, fetchTimeoutMs = 5000): Promise<string> {
  const controller = new AbortController();
  const timeout = setTimeout(() => {
    controller.abort();
  }, fetchTimeoutMs);

  try {
    const response = await fetch(input, {
      redirect: 'follow',
      signal: controller.signal,
    });
    if (!response.ok) {
      throw new Error(`Unable to download input from ${input}: ${response.status} ${response.statusText}`);
    }
    return await response.text();
  } catch (error) {
    if (controller.signal.aborted) {
      throw new Error(`Input source ${input} timed out after ${fetchTimeoutMs}ms.`);
    }
    throw error;
  } finally {
    clearTimeout(timeout);
  }
}

async function readFromStdin(): Promise<string> {
  const chunks: Buffer[] = [];
  for await (const chunk of process.stdin) {
    chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
  }
  return Buffer.concat(chunks).toString('utf8');
}

function isHttpUrl(value: string): boolean {
  return /^https?:\/\//i.test(value);
}

function normalizeBase64(value: string): string {
  return value.replace(/\s+/g, '').trim().replace(/-/g, '+').replace(/_/g, '/');
}

export function decodeSubscriptionContent(input: string): string {
  const trimmed = input.trim();
  if (!trimmed || trimmed.includes('://')) {
    return input;
  }

  const normalized = normalizeBase64(trimmed);
  if (!normalized || /[^a-z0-9+/=]/i.test(normalized)) {
    return input;
  }

  try {
    const decoded = Buffer.from(normalized, 'base64').toString('utf8');
    if (decoded.includes('://')) {
      return decoded;
    }
  } catch {
    return input;
  }

  return input;
}

function applyInputFormat(raw: string, format: SubscriptionInputFormat = 'auto'): { content: string; encoding: 'plain' | 'base64' } {
  if (format === 'plain') {
    return { content: raw, encoding: 'plain' };
  }

  if (format === 'base64') {
    const content = Buffer.from(normalizeBase64(raw), 'base64').toString('utf8');
    return { content, encoding: 'base64' };
  }

  const content = decodeSubscriptionContent(raw);
  return {
    content,
    encoding: content === raw ? 'plain' : 'base64',
  };
}

export async function loadInputSource(input: string, options: LoadInputOptions = {}): Promise<LoadedInput> {
  if (isStdinInput(input)) {
    const raw = ensureNonEmptyContent(await readFromStdin(), 'stdin');
    const resolved = applyInputFormat(raw, options.format);
    return {
      source: 'stdin',
      content: ensureNonEmptyContent(resolved.content, 'stdin'),
      encoding: resolved.encoding,
    };
  }

  if (isHttpUrl(input)) {
    const raw = ensureNonEmptyContent(await downloadRemoteContent(input, options.fetchTimeoutMs), input);
    const resolved = applyInputFormat(raw, options.format);
    return {
      source: input,
      content: ensureNonEmptyContent(resolved.content, input),
      encoding: resolved.encoding,
    };
  }

  const inputPath = resolve(input);
  const raw = ensureNonEmptyContent(await readFile(inputPath, 'utf8'), inputPath);
  const resolved = applyInputFormat(raw, options.format);
  return {
    source: inputPath,
    content: ensureNonEmptyContent(resolved.content, inputPath),
    encoding: resolved.encoding,
  };
}

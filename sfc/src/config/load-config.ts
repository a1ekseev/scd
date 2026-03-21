import { readFile } from 'node:fs/promises';
import { dirname, isAbsolute, resolve } from 'node:path';

import { CronExpressionParser } from 'cron-parser';
import { parse as parseYaml } from 'yaml';
import { z } from 'zod';

import { ConfigError } from '../errors.ts';
import type { AppConfig, LoadedConfig } from '../types.ts';

const optionalTrimmedStringSchema = z.preprocess(
  (value) => {
    if (typeof value !== 'string') {
      return value;
    }
    const trimmed = value.trim();
    return trimmed.length > 0 ? trimmed : undefined;
  },
  z.string().trim().min(1).optional(),
);

export function parseRegexLiteral(value: string): RegExp {
  if (!value.startsWith('/')) {
    throw new Error('Regex literal must start with "/".');
  }

  const lastSlash = value.lastIndexOf('/');
  if (lastSlash <= 0) {
    throw new Error('Regex literal must end with "/flags".');
  }

  const pattern = value.slice(1, lastSlash);
  const flags = value.slice(lastSlash + 1);
  return new RegExp(pattern, flags);
}

function validateListenAddress(value: string): void {
  const separator = value.lastIndexOf(':');
  if (separator <= 0 || separator === value.length - 1) {
    throw new Error('Listen address must be in "host:port" format.');
  }

  const port = Number(value.slice(separator + 1));
  if (!Number.isInteger(port) || port <= 0 || port > 65535) {
    throw new Error('Listen address port must be an integer between 1 and 65535.');
  }
}

const outputSchema = z.object({
  id: z.string().trim().min(1),
  enabled: z.boolean().default(true),
  name: optionalTrimmedStringSchema,
  labelIncludeRegex: z.string().trim().min(1),
}).superRefine((output, context) => {
  try {
    parseRegexLiteral(output.labelIncludeRegex);
  } catch (error) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      message: error instanceof Error ? error.message : 'Invalid regex literal.',
      path: ['labelIncludeRegex'],
    });
  }
});

const subscriptionSchema = z.object({
  id: z.string().trim().min(1),
  input: z.string().trim().min(1),
  enabled: z.boolean().default(true),
  format: z.enum(['auto', 'plain', 'base64']).default('auto'),
  fetchTimeoutMs: z.number().int().positive().default(5000),
  outputs: z.array(outputSchema).min(1),
});

const appConfigSchema = z.object({
  subscriptions: z.array(subscriptionSchema).min(1),
  runtime: z.object({
    refreshSchedule: z.string().trim(),
  }),
  server: z.object({
    listen: z.string().trim().min(1),
  }),
  logging: z.object({
    level: z.enum(['fatal', 'error', 'warn', 'info', 'debug', 'trace', 'silent']).default('info'),
    format: z.enum(['json', 'pretty']).default('json'),
  }).default({ level: 'info', format: 'json' }),
}).superRefine((config, context) => {
  const enabledSubscriptions = config.subscriptions.filter((subscription) => subscription.enabled);
  if (enabledSubscriptions.length === 0) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      message: 'At least one subscription must be enabled.',
      path: ['subscriptions'],
    });
  }

  const enabledOutputs = enabledSubscriptions.flatMap((subscription) => subscription.outputs.filter((output) => output.enabled));
  if (enabledOutputs.length === 0) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      message: 'At least one output must be enabled.',
      path: ['subscriptions'],
    });
  }

  const subscriptionIds = new Map<string, number>();
  for (const subscription of config.subscriptions) {
    subscriptionIds.set(subscription.id, (subscriptionIds.get(subscription.id) ?? 0) + 1);
  }
  for (const [id, count] of subscriptionIds) {
    if (count > 1) {
      context.addIssue({
        code: z.ZodIssueCode.custom,
        message: `Duplicate subscription id "${id}" is not allowed.`,
        path: ['subscriptions'],
      });
    }
  }

  const outputIds = new Map<string, number>();
  for (const subscription of config.subscriptions) {
    for (const output of subscription.outputs) {
      outputIds.set(output.id, (outputIds.get(output.id) ?? 0) + 1);
    }
  }
  for (const [id, count] of outputIds) {
    if (count > 1) {
      context.addIssue({
        code: z.ZodIssueCode.custom,
        message: `Duplicate output id "${id}" is not allowed.`,
        path: ['subscriptions'],
      });
    }
  }

  try {
    CronExpressionParser.parse(config.runtime.refreshSchedule);
  } catch (error) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      message: error instanceof Error ? error.message : 'Invalid cron expression.',
      path: ['runtime', 'refreshSchedule'],
    });
  }

  try {
    validateListenAddress(config.server.listen);
  } catch (error) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      message: error instanceof Error ? error.message : 'Invalid listen address.',
      path: ['server', 'listen'],
    });
  }
});

function interpolateEnv(value: string): string {
  return value.replace(/\$\{([A-Z0-9_]+)\}/gi, (_, name: string) => process.env[name] ?? '');
}

function interpolateDeep<T>(value: T): T {
  if (typeof value === 'string') {
    return interpolateEnv(value) as T;
  }

  if (Array.isArray(value)) {
    return value.map((item) => interpolateDeep(item)) as T;
  }

  if (value && typeof value === 'object') {
    return Object.fromEntries(
      Object.entries(value).map(([key, item]) => [key, interpolateDeep(item)]),
    ) as T;
  }

  return value;
}

function resolveConfigPaths(config: AppConfig, configPath: string): AppConfig {
  const configDir = dirname(configPath);
  return {
    ...config,
    subscriptions: config.subscriptions.map((subscription) => ({
      ...subscription,
      input:
        subscription.input === '-' || /^https?:\/\//i.test(subscription.input) || isAbsolute(subscription.input)
          ? subscription.input
          : resolve(configDir, subscription.input),
    })),
  };
}

export async function loadConfig(configPath: string): Promise<LoadedConfig> {
  const resolvedPath = resolve(configPath);
  const source = await readFile(resolvedPath, 'utf8');
  const parsed = parseYaml(source);
  const normalized = interpolateDeep(parsed);
  const result = appConfigSchema.safeParse(normalized);
  if (!result.success) {
    throw new ConfigError(result.error.issues.map((issue) => issue.message).join('\n'));
  }

  return {
    configPath: resolvedPath,
    config: resolveConfigPaths(result.data, resolvedPath),
  };
}

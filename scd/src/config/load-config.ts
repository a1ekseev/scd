import { readFile } from 'node:fs/promises';
import { dirname, isAbsolute, resolve } from 'node:path';

import { CronExpressionParser } from 'cron-parser';
import { parse as parseYaml } from 'yaml';
import { z } from 'zod';

import { ConfigError } from '../errors.ts';
import { FLAG_COUNTRY_MAP } from '../flag-country-map/index.ts';
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

const supportedIso2Codes = new Set(Object.values(FLAG_COUNTRY_MAP).map((country) => country.iso2));

function parseRegexLiteral(value: string): RegExp {
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

const optionalCountryAllowlistSchema = z.preprocess(
  (value) => {
    if (!Array.isArray(value)) {
      return value;
    }
    return value.map((item) => (typeof item === 'string' ? item.trim().toUpperCase() : item));
  },
  z.array(z.string().trim().length(2)).optional(),
);

const filtersSchema = z
  .object({
    countryAllowlist: optionalCountryAllowlistSchema,
    labelIncludeRegex: optionalTrimmedStringSchema,
  })
  .superRefine((filters, context) => {
    for (const countryCode of filters.countryAllowlist ?? []) {
      if (!supportedIso2Codes.has(countryCode)) {
        context.addIssue({
          code: z.ZodIssueCode.custom,
          message: `Unsupported ISO2 country code "${countryCode}".`,
          path: ['countryAllowlist'],
        });
      }
    }

    if (filters.labelIncludeRegex) {
      try {
        parseRegexLiteral(filters.labelIncludeRegex);
      } catch (error) {
        context.addIssue({
          code: z.ZodIssueCode.custom,
          message: error instanceof Error ? error.message : 'Invalid regex literal.',
          path: ['labelIncludeRegex'],
        });
      }
    }
  })
  .optional();

const targetSchema = z.object({
  address: z.string().trim().min(1),
  timeoutMs: z.number().int().positive().default(5000),
  fixedOutbounds: z.array(z.string().trim().min(1)).default([]),
  observatorySubjectSelectorPrefix: optionalTrimmedStringSchema,
});

const subscriptionSchema = z.object({
  id: z.string().trim().min(1),
  input: z.string().trim().min(1),
  enabled: z.boolean().default(true),
  format: z.enum(['auto', 'plain', 'base64']).default('auto'),
  fetchTimeoutMs: z.number().int().positive().default(5000),
  filters: filtersSchema,
  targets: z.array(targetSchema).min(1),
});

const appConfigSchema = z.object({
  subscriptions: z.array(subscriptionSchema).min(1),
  runtime: z.object({
    mode: z.enum(['run-once', 'daemon']).default('run-once'),
    schedule: z.string().trim().optional(),
  }),
  logging: z.object({
    level: z.enum(['fatal', 'error', 'warn', 'info', 'debug', 'trace', 'silent']).default('info'),
    format: z.enum(['json', 'pretty']).default('json'),
  }),
  resources: z.object({
    outbounds: z.object({
      enabled: z.boolean().default(true),
    }),
  }),
}).superRefine((config, context) => {
  const enabledSubscriptions = config.subscriptions.filter((subscription) => subscription.enabled);
  if (enabledSubscriptions.length === 0) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      message: 'At least one subscription must be enabled.',
      path: ['subscriptions'],
    });
  }

  const addresses = new Map<string, number>();
  for (const subscription of config.subscriptions) {
    for (const target of subscription.targets) {
      addresses.set(target.address, (addresses.get(target.address) ?? 0) + 1);
    }
  }

  for (const [address, count] of addresses) {
    if (count > 1) {
      context.addIssue({
        code: z.ZodIssueCode.custom,
        message: `Duplicate target address "${address}" is not allowed.`,
        path: ['subscriptions'],
      });
    }
  }

  if (config.runtime.mode === 'daemon') {
    if (!config.runtime.schedule) {
      context.addIssue({
        code: z.ZodIssueCode.custom,
        message: 'runtime.schedule is required when runtime.mode is "daemon".',
        path: ['runtime', 'schedule'],
      });
      return;
    }

    try {
      CronExpressionParser.parse(config.runtime.schedule);
    } catch (error) {
      context.addIssue({
        code: z.ZodIssueCode.custom,
        message: error instanceof Error ? error.message : 'Invalid cron expression.',
        path: ['runtime', 'schedule'],
      });
    }
  }
});

function interpolateEnv(raw: string): string {
  return raw.replace(/\$\{([A-Z0-9_]+)\}/gi, (_match, name: string) => {
    const value = process.env[name];
    if (value === undefined) {
      throw new ConfigError(`Missing environment variable "${name}" required by config.`);
    }
    return value;
  });
}

function isRemoteInput(value: string): boolean {
  return /^https?:\/\//i.test(value) || value === '-';
}

function resolveMaybeRelative(configDir: string, value: string): string {
  if (isAbsolute(value)) {
    return value;
  }
  return resolve(configDir, value);
}

export async function loadConfig(configPath: string): Promise<LoadedConfig> {
  const resolvedPath = resolve(configPath);
  const configDir = dirname(resolvedPath);
  const raw = await readFile(resolvedPath, 'utf8');
  const interpolated = interpolateEnv(raw);
  const parsed = parseYaml(interpolated) ?? {};
  const result = appConfigSchema.safeParse(parsed);

  if (!result.success) {
    throw new ConfigError(
      `Invalid config at ${resolvedPath}: ${result.error.issues.map((issue) => `${issue.path.join('.') || '<root>'}: ${issue.message}`).join('; ')}`,
    );
  }

  return {
    configPath: resolvedPath,
    config: {
      ...result.data,
      subscriptions: result.data.subscriptions.map((subscription) => ({
        ...subscription,
        input: isRemoteInput(subscription.input)
          ? subscription.input
          : resolveMaybeRelative(configDir, subscription.input),
      })),
      runtime: result.data.runtime,
    } satisfies AppConfig,
  };
}

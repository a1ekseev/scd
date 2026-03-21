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
  fixedInbounds: z.array(z.string().trim().min(1)).default([]),
  fixedRouting: z.array(z.string().trim().min(1)).default([]),
  inboundSocks: z
    .object({
      listen: z.string().trim().min(1),
      portRange: z.object({
        start: z.number().int().positive(),
        end: z.number().int().positive(),
      }),
    })
    .superRefine((value, context) => {
      if (value.portRange.end < value.portRange.start) {
        context.addIssue({
          code: z.ZodIssueCode.custom,
          message: 'inboundSocks.portRange.end must be greater than or equal to start.',
          path: ['portRange', 'end'],
        });
      }
    })
    .optional(),
  monitor: z
    .object({
      enabled: z.boolean().default(false),
      schedule: z.string().trim().optional(),
      maxParallel: z.number().int().positive().default(10),
      request: z
        .object({
          url: z.string().trim().url(),
          method: z.enum(['GET', 'HEAD', 'POST']).default('GET'),
          expectedStatus: z.number().int().min(100).max(599),
          timeoutMs: z.number().int().positive().default(5000),
        })
        .optional(),
    })
    .default({ enabled: false, maxParallel: 10 }),
  balancerMonitor: z
    .object({
      enabled: z.boolean().default(false),
      schedule: z.string().trim().optional(),
      socks5: z
        .object({
          host: z.string().trim().min(1),
          port: z.number().int().min(1).max(65535),
        })
        .optional(),
      request: z
        .object({
          url: z.string().trim().url(),
          method: z.enum(['GET', 'HEAD', 'POST']).default('GET'),
          expectedStatus: z.number().int().min(100).max(599),
          timeoutMs: z.number().int().positive().default(5000),
        })
        .optional(),
      successGet: z
        .object({
          url: z.string().trim().url(),
          expectedStatus: z.number().int().min(100).max(599),
          timeoutMs: z.number().int().positive().default(5000),
        })
        .optional(),
    })
    .default({ enabled: false }),
  speedtest: z
    .object({
      enabled: z.boolean().default(false),
      schedule: z.string().trim().optional(),
      urls: z.array(z.string().trim().url()).min(1).optional(),
      method: z.literal('GET').default('GET'),
      expectedSizeBytes: z.number().int().positive().optional(),
      timeoutMs: z.number().int().positive().default(15000),
      maxParallel: z.number().int().positive().default(3),
    })
    .default({ enabled: false, method: 'GET', timeoutMs: 15000, maxParallel: 3 }),
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
    }).default({ enabled: true }),
    inbounds: z.object({
      enabled: z.boolean().default(false),
    }).default({ enabled: false }),
    routing: z.object({
      enabled: z.boolean().default(false),
    }).default({ enabled: false }),
  }),
  statusServer: z.object({
    enabled: z.boolean().default(false),
    listen: z.string().trim().optional(),
    runtimeState: z.object({
      enabled: z.boolean().default(true),
      includeRaw: z.boolean().default(false),
      includeSecrets: z.boolean().default(false),
    }).default({ enabled: true, includeRaw: false, includeSecrets: false }),
  }).default({ enabled: false, runtimeState: { enabled: true, includeRaw: false, includeSecrets: false } }),
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

  if (config.resources.inbounds.enabled && !config.resources.outbounds.enabled) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      message: 'resources.inbounds.enabled requires resources.outbounds.enabled.',
      path: ['resources', 'inbounds'],
    });
  }

  if (
    config.resources.routing.enabled &&
    (!config.resources.outbounds.enabled || !config.resources.inbounds.enabled)
  ) {
    context.addIssue({
      code: z.ZodIssueCode.custom,
      message: 'resources.routing.enabled requires resources.outbounds.enabled and resources.inbounds.enabled.',
      path: ['resources', 'routing'],
    });
  }

  for (const subscription of config.subscriptions) {
    for (const target of subscription.targets) {
      if ((config.resources.inbounds.enabled || config.resources.routing.enabled) && !target.inboundSocks) {
        context.addIssue({
          code: z.ZodIssueCode.custom,
          message: 'inboundSocks is required when resources.inbounds.enabled or resources.routing.enabled is true.',
          path: ['subscriptions'],
        });
      }

      if (target.monitor.enabled) {
        if (!target.monitor.schedule) {
          context.addIssue({
            code: z.ZodIssueCode.custom,
            message: 'monitor.schedule is required when monitor.enabled is true.',
            path: ['subscriptions'],
          });
        } else {
          try {
            CronExpressionParser.parse(target.monitor.schedule);
          } catch (error) {
            context.addIssue({
              code: z.ZodIssueCode.custom,
              message: error instanceof Error ? error.message : 'Invalid cron expression.',
              path: ['subscriptions'],
            });
          }
        }

        if (!target.monitor.request) {
          context.addIssue({
            code: z.ZodIssueCode.custom,
            message: 'monitor.request is required when monitor.enabled is true.',
            path: ['subscriptions'],
          });
        }

        if (!target.inboundSocks) {
          context.addIssue({
            code: z.ZodIssueCode.custom,
            message: 'inboundSocks is required when monitor.enabled is true.',
            path: ['subscriptions'],
          });
        }

        if (!config.resources.inbounds.enabled || !config.resources.routing.enabled || !config.resources.outbounds.enabled) {
          context.addIssue({
            code: z.ZodIssueCode.custom,
            message: 'monitor.enabled requires resources.outbounds.enabled, resources.inbounds.enabled and resources.routing.enabled.',
            path: ['resources'],
          });
        }
      }

      if (target.speedtest.enabled) {
        if (!target.speedtest.schedule) {
          context.addIssue({
            code: z.ZodIssueCode.custom,
            message: 'speedtest.schedule is required when speedtest.enabled is true.',
            path: ['subscriptions'],
          });
        } else {
          try {
            CronExpressionParser.parse(target.speedtest.schedule);
          } catch (error) {
            context.addIssue({
              code: z.ZodIssueCode.custom,
              message: error instanceof Error ? error.message : 'Invalid cron expression.',
              path: ['subscriptions'],
            });
          }
        }

        if (!target.speedtest.urls || target.speedtest.urls.length === 0) {
          context.addIssue({
            code: z.ZodIssueCode.custom,
            message: 'speedtest.urls is required when speedtest.enabled is true.',
            path: ['subscriptions'],
          });
        }

        if (!target.inboundSocks) {
          context.addIssue({
            code: z.ZodIssueCode.custom,
            message: 'inboundSocks is required when speedtest.enabled is true.',
            path: ['subscriptions'],
          });
        }

        if (!config.resources.inbounds.enabled || !config.resources.routing.enabled || !config.resources.outbounds.enabled) {
          context.addIssue({
            code: z.ZodIssueCode.custom,
            message: 'speedtest.enabled requires resources.outbounds.enabled, resources.inbounds.enabled and resources.routing.enabled.',
            path: ['resources'],
          });
        }
      }

      if (target.balancerMonitor.enabled) {
        if (!target.balancerMonitor.schedule) {
          context.addIssue({
            code: z.ZodIssueCode.custom,
            message: 'balancerMonitor.schedule is required when balancerMonitor.enabled is true.',
            path: ['subscriptions'],
          });
        } else {
          try {
            CronExpressionParser.parse(target.balancerMonitor.schedule);
          } catch (error) {
            context.addIssue({
              code: z.ZodIssueCode.custom,
              message: error instanceof Error ? error.message : 'Invalid cron expression.',
              path: ['subscriptions'],
            });
          }
        }

        if (!target.balancerMonitor.socks5) {
          context.addIssue({
            code: z.ZodIssueCode.custom,
            message: 'balancerMonitor.socks5 is required when balancerMonitor.enabled is true.',
            path: ['subscriptions'],
          });
        }

        if (!target.balancerMonitor.request) {
          context.addIssue({
            code: z.ZodIssueCode.custom,
            message: 'balancerMonitor.request is required when balancerMonitor.enabled is true.',
            path: ['subscriptions'],
          });
        }
      }
    }
  }

  if (config.statusServer.enabled) {
    if (!config.statusServer.listen) {
      context.addIssue({
        code: z.ZodIssueCode.custom,
        message: 'statusServer.listen is required when statusServer.enabled is true.',
        path: ['statusServer', 'listen'],
      });
    } else {
      try {
        validateListenAddress(config.statusServer.listen);
      } catch (error) {
        context.addIssue({
          code: z.ZodIssueCode.custom,
          message: error instanceof Error ? error.message : 'Invalid listen address.',
          path: ['statusServer', 'listen'],
        });
      }
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

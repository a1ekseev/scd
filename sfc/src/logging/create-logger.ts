import pino from 'pino';

import type { LoggingConfig } from '../types.ts';

export type Logger = pino.Logger;

export function createLogger(config: LoggingConfig): Logger {
  if (config.format === 'pretty') {
    return pino({
      level: config.level,
      base: undefined,
      transport: {
        target: 'pino-pretty',
        options: {
          colorize: true,
          destination: 2,
          translateTime: 'SYS:standard',
          ignore: 'pid,hostname',
        },
      },
    });
  }

  return pino({
    level: config.level,
    base: undefined,
  }, pino.destination(2));
}

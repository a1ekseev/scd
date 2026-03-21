import { CronExpressionParser } from 'cron-parser';

import { loadConfig } from '../config/load-config.ts';
import type { Logger } from '../logging/create-logger.ts';
import { createLogger } from '../logging/create-logger.ts';
import { createAppState, refreshWithConfig } from './refresh.ts';
import { startServer } from './server.ts';

function wait(ms: number): Promise<void> {
  return new Promise((resolve) => {
    setTimeout(resolve, ms);
  });
}

function getNextCronDate(schedule: string, currentDate: Date): Date {
  return CronExpressionParser.parse(schedule, { currentDate }).next().toDate();
}

export function getErrorLogDetails(error: unknown): { message: string; causes?: string[] } {
  const message = error instanceof Error ? error.message : String(error);

  if (!(error instanceof AggregateError)) {
    return { message };
  }

  const causes = error.errors
    .map((item) => item instanceof Error ? item.message : String(item))
    .filter((item) => item.length > 0);

  return causes.length > 0
    ? { message, causes }
    : { message };
}

export async function runCronLoop(
  schedule: string,
  isStopping: () => boolean,
  task: () => Promise<void>,
  options: {
    nowFn?: () => Date;
    waitFn?: (ms: number) => Promise<void>;
    getNextDateFn?: (schedule: string, currentDate: Date) => Date;
    onOverrun?: (scheduledAt: Date) => void;
  } = {},
): Promise<void> {
  const nowFn = options.nowFn ?? (() => new Date());
  const waitFn = options.waitFn ?? wait;
  const getNextDateFn = options.getNextDateFn ?? getNextCronDate;
  let tickInProgress = false;
  let activeTask: Promise<void> | undefined;

  const startTask = () => {
    tickInProgress = true;
    let running!: Promise<void>;
    running = (async () => {
      try {
        await task();
      } finally {
        tickInProgress = false;
        if (activeTask === running) {
          activeTask = undefined;
        }
      }
    })();
    activeTask = running;
  };

  let scheduledAt = nowFn();
  startTask();

  while (!isStopping()) {
    scheduledAt = getNextDateFn(schedule, scheduledAt);
    const delayMs = Math.max(0, scheduledAt.getTime() - nowFn().getTime());
    await waitFn(delayMs);
    if (isStopping()) {
      break;
    }

    if (tickInProgress) {
      options.onOverrun?.(scheduledAt);
      continue;
    }

    startTask();
  }

  if (activeTask) {
    await activeTask;
  }
}

export async function runLoggedCronLoop(
  schedule: string,
  isStopping: () => boolean,
  task: () => Promise<void>,
  logger: Pick<Logger, 'warn' | 'error'>,
  config: {
    overrunEvent: string;
    overrunMessage: string;
    failureEvent: string;
    failureMessage: string;
    context?: Record<string, unknown>;
    runCronLoopOptions?: Parameters<typeof runCronLoop>[3];
  },
): Promise<void> {
  const { context, runCronLoopOptions } = config;

  await runCronLoop(schedule, isStopping, async () => {
    try {
      await task();
    } catch (error) {
      const details = getErrorLogDetails(error);
      logger.error(
        {
          ...(context ?? {}),
          event: config.failureEvent,
          error: details.message,
          ...(details.causes ? { causes: details.causes } : {}),
        },
        config.failureMessage,
      );
    }
  }, {
    ...runCronLoopOptions,
    onOverrun: (scheduledAt) => {
      logger.warn(
        {
          ...(context ?? {}),
          event: config.overrunEvent,
          scheduledAt: scheduledAt.toISOString(),
        },
        config.overrunMessage,
      );
      runCronLoopOptions?.onOverrun?.(scheduledAt);
    },
  });
}

export async function runServerCommand(configPath: string): Promise<void> {
  const loadedConfig = await loadConfig(configPath);
  const logger = createLogger(loadedConfig.config.logging);
  const state = createAppState(loadedConfig.config);

  let stopping = false;
  const stop = () => {
    stopping = true;
  };

  process.once('SIGINT', stop);
  process.once('SIGTERM', stop);

  logger.info({ event: 'config_loaded', configPath: loadedConfig.configPath }, 'Config loaded.');
  await refreshWithConfig(loadedConfig, state, logger);

  const server = await startServer(loadedConfig.config.server.listen, loadedConfig, state, logger);

  try {
    await runLoggedCronLoop(loadedConfig.config.runtime.refreshSchedule, () => stopping, async () => {
      logger.info({ event: 'refresh_tick' }, 'Refresh tick started.');
      await refreshWithConfig(loadedConfig, state, logger);
    }, logger, {
      overrunEvent: 'refresh_tick_skipped_overrun',
      overrunMessage: 'Refresh tick skipped because previous run is still in progress.',
      failureEvent: 'refresh_failed',
      failureMessage: 'Refresh tick failed.',
    });
  } finally {
    stopping = true;
    await server.close();
  }
}

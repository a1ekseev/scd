import { CronExpressionParser } from 'cron-parser';

import { loadConfig } from '../config/load-config.ts';
import type { Logger } from '../logging/create-logger.ts';
import { createLogger } from '../logging/create-logger.ts';
import { runTargetBalancerMonitorTick, runTargetMonitorTick, runTargetSpeedtestTick } from './monitoring.ts';
import { createSyncMemoryState } from './run-state.ts';
import { startStatusServer } from './status-server.ts';
import { syncWithConfig } from './sync-once.ts';

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

export async function runDaemon(configPath: string): Promise<void> {
  const loadedConfig = await loadConfig(configPath);
  const logger = createLogger(loadedConfig.config.logging);
  const memoryState = createSyncMemoryState();

  if (loadedConfig.config.runtime.mode !== 'daemon') {
    throw new Error(`Config at ${loadedConfig.configPath} is not in daemon mode.`);
  }

  let stopping = false;
  const stop = () => {
    stopping = true;
  };

  process.once('SIGINT', stop);
  process.once('SIGTERM', stop);

  logger.info({ event: 'config_loaded', configPath: loadedConfig.configPath }, 'Config loaded.');

  const statusServer = loadedConfig.config.statusServer.enabled && loadedConfig.config.statusServer.listen
    ? await startStatusServer(loadedConfig.config.statusServer.listen, loadedConfig, memoryState, logger)
    : undefined;

  try {
    const loops = [
      runLoggedCronLoop(loadedConfig.config.runtime.schedule!, () => stopping, async () => {
        logger.info({ event: 'daemon_tick' }, 'Daemon tick started.');
        await syncWithConfig(loadedConfig, logger, memoryState);
      }, logger, {
        overrunEvent: 'daemon_tick_skipped_overrun',
        overrunMessage: 'Daemon tick skipped because previous run is still in progress.',
        failureEvent: 'sync_failed',
        failureMessage: 'Daemon sync failed.',
      }),
      ...loadedConfig.config.subscriptions.flatMap((subscription) =>
        subscription.targets.flatMap((target) => {
          const tasks: Promise<void>[] = [];

          if (target.monitor.enabled && target.monitor.schedule) {
            tasks.push(
              runLoggedCronLoop(target.monitor.schedule, () => stopping, async () => {
                await runTargetMonitorTick(subscription.id, target, memoryState, logger);
              }, logger, {
                overrunEvent: 'monitor_tick_skipped_overrun',
                overrunMessage: 'Monitor tick skipped because previous run is still in progress.',
                failureEvent: 'monitor_tick_failed',
                failureMessage: 'Monitor tick failed.',
                context: {
                  subscriptionId: subscription.id,
                  targetAddress: target.address,
                },
              }),
            );
          }

          if (target.speedtest.enabled && target.speedtest.schedule) {
            tasks.push(
              runLoggedCronLoop(target.speedtest.schedule, () => stopping, async () => {
                await runTargetSpeedtestTick(subscription.id, target, memoryState);
              }, logger, {
                overrunEvent: 'speedtest_tick_skipped_overrun',
                overrunMessage: 'Speedtest tick skipped because previous run is still in progress.',
                failureEvent: 'speedtest_tick_failed',
                failureMessage: 'Speedtest tick failed.',
                context: {
                  subscriptionId: subscription.id,
                  targetAddress: target.address,
                },
              }),
            );
          }

          if (target.balancerMonitor.enabled && target.balancerMonitor.schedule) {
            tasks.push(
              runLoggedCronLoop(target.balancerMonitor.schedule, () => stopping, async () => {
                await runTargetBalancerMonitorTick(subscription.id, target, memoryState, logger);
              }, logger, {
                overrunEvent: 'balancer_monitor_tick_skipped_overrun',
                overrunMessage: 'Balancer monitor tick skipped because previous run is still in progress.',
                failureEvent: 'balancer_monitor_tick_failed',
                failureMessage: 'Balancer monitor tick failed.',
                context: {
                  subscriptionId: subscription.id,
                  targetAddress: target.address,
                },
              }),
            );
          }

          return tasks;
        }),
      ),
    ];

    await Promise.race(loops);
  } finally {
    stopping = true;
    if (statusServer) {
      await statusServer.close();
    }
  }
}

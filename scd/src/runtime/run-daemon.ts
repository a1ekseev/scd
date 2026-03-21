import { CronExpressionParser } from 'cron-parser';

import { loadConfig } from '../config/load-config.ts';
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

async function runCronLoop(
  schedule: string,
  isStopping: () => boolean,
  task: () => Promise<void>,
): Promise<void> {
  while (!isStopping()) {
    await task();
    if (isStopping()) {
      break;
    }

    const next = CronExpressionParser.parse(schedule, { currentDate: new Date() }).next();
    const delayMs = Math.max(0, next.getTime() - Date.now());
    await wait(delayMs);
  }
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
      runCronLoop(loadedConfig.config.runtime.schedule!, () => stopping, async () => {
        logger.info({ event: 'daemon_tick' }, 'Daemon tick started.');
        try {
          await syncWithConfig(loadedConfig, logger, memoryState);
        } catch (error) {
          logger.error(
            { event: 'sync_failed', error: error instanceof Error ? error.message : String(error) },
            'Daemon sync failed.',
          );
        }
      }),
      ...loadedConfig.config.subscriptions.flatMap((subscription) =>
        subscription.targets.flatMap((target) => {
          const tasks: Promise<void>[] = [];

          if (target.monitor.enabled && target.monitor.schedule) {
            tasks.push(
              runCronLoop(target.monitor.schedule, () => stopping, async () => {
                try {
                  await runTargetMonitorTick(subscription.id, target, memoryState, logger);
                } catch (error) {
                  logger.error(
                    {
                      event: 'monitor_tick_failed',
                      subscriptionId: subscription.id,
                      targetAddress: target.address,
                      error: error instanceof Error ? error.message : String(error),
                    },
                    'Monitor tick failed.',
                  );
                }
              }),
            );
          }

          if (target.speedtest.enabled && target.speedtest.schedule) {
            tasks.push(
              runCronLoop(target.speedtest.schedule, () => stopping, async () => {
                try {
                  await runTargetSpeedtestTick(subscription.id, target, memoryState);
                } catch (error) {
                  logger.error(
                    {
                      event: 'speedtest_tick_failed',
                      subscriptionId: subscription.id,
                      targetAddress: target.address,
                      error: error instanceof Error ? error.message : String(error),
                    },
                    'Speedtest tick failed.',
                  );
                }
              }),
            );
          }

          if (target.balancerMonitor.enabled && target.balancerMonitor.schedule) {
            tasks.push(
              runCronLoop(target.balancerMonitor.schedule, () => stopping, async () => {
                try {
                  await runTargetBalancerMonitorTick(subscription.id, target, memoryState, logger);
                } catch (error) {
                  logger.error(
                    {
                      event: 'balancer_monitor_tick_failed',
                      subscriptionId: subscription.id,
                      targetAddress: target.address,
                      error: error instanceof Error ? error.message : String(error),
                    },
                    'Balancer monitor tick failed.',
                  );
                }
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

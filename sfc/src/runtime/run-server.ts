import { CronExpressionParser } from 'cron-parser';

import { loadConfig } from '../config/load-config.ts';
import { createLogger } from '../logging/create-logger.ts';
import { createAppState, refreshWithConfig } from './refresh.ts';
import { startServer } from './server.ts';

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
    await runCronLoop(loadedConfig.config.runtime.refreshSchedule, () => stopping, async () => {
      logger.info({ event: 'refresh_tick' }, 'Refresh tick started.');
      try {
        await refreshWithConfig(loadedConfig, state, logger);
      } catch (error) {
        logger.error(
          { event: 'refresh_failed', error: error instanceof Error ? error.message : String(error) },
          'Refresh tick failed.',
        );
      }
    });
  } finally {
    stopping = true;
    await server.close();
  }
}

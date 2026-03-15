import { CronExpressionParser } from 'cron-parser';

import { loadConfig } from '../config/load-config.ts';
import { createLogger } from '../logging/create-logger.ts';
import { syncWithConfig } from './sync-once.ts';
import { createSyncMemoryState } from './run-state.ts';

function wait(ms: number): Promise<void> {
  return new Promise((resolve) => {
    setTimeout(resolve, ms);
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

  while (!stopping) {
    logger.info({ event: 'daemon_tick' }, 'Daemon tick started.');
    try {
      await syncWithConfig(loadedConfig, logger, memoryState);
    } catch (error) {
      logger.error(
        { event: 'sync_failed', error: error instanceof Error ? error.message : String(error) },
        'Daemon sync failed.',
      );
    }

    if (stopping) {
      break;
    }

    const schedule = loadedConfig.config.runtime.schedule!;
    const next = CronExpressionParser.parse(schedule, { currentDate: new Date() }).next();
    const delayMs = Math.max(0, next.getTime() - Date.now());
    await wait(delayMs);
  }
}

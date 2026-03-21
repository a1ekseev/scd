#!/usr/bin/env node
import { Command } from 'commander';

import { loadConfig } from './config/load-config.ts';
import { createLogger } from './logging/create-logger.ts';
import { createAppState, refreshWithConfig } from './runtime/refresh.ts';
import { runServerCommand } from './runtime/run-server.ts';

const program = new Command();

program
  .name('sfc')
  .description('Filter upstream subscriptions and serve them by public URL.')
  .showHelpAfterError();

program
  .command('serve')
  .requiredOption('--config <config>')
  .action(async (options) => {
    await runServerCommand(options.config);
  });

program
  .command('refresh')
  .requiredOption('--config <config>')
  .action(async (options) => {
    const loaded = await loadConfig(options.config);
    const state = createAppState(loaded.config);
    const logger = createLogger(loaded.config.logging);
    const report = await refreshWithConfig(loaded, state, logger);
    process.stdout.write(JSON.stringify(report, null, 2) + '\n');
    process.exitCode = report.failed > 0 ? 1 : 0;
  });

program
  .command('validate-config')
  .requiredOption('--config <config>')
  .action(async (options) => {
    const loaded = await loadConfig(options.config);
    process.stdout.write(
      JSON.stringify(
        {
          configPath: loaded.configPath,
          valid: true,
        },
        null,
        2,
      ) + '\n',
    );
  });

program
  .command('print-config')
  .requiredOption('--config <config>')
  .action(async (options) => {
    const loaded = await loadConfig(options.config);
    process.stdout.write(JSON.stringify(loaded.config, null, 2) + '\n');
  });

program.parseAsync(process.argv).catch((error) => {
  process.stderr.write(`${error instanceof Error ? error.message : String(error)}\n`);
  process.exitCode = 1;
});

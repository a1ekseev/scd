#!/usr/bin/env node
import { Command } from 'commander';

import { generateCommand } from './commands/generate.ts';
import { loadConfig } from './config/load-config.ts';
import { runDaemon } from './runtime/run-daemon.ts';
import { syncOnce } from './runtime/sync-once.ts';

const program = new Command();

program
  .name('scd')
  .description('Generate and apply Xray resources from subscriptions.')
  .showHelpAfterError();

program
  .command('generate')
  .requiredOption('--input <input>')
  .requiredOption('--output <output>')
  .requiredOption('--log <log>')
  .action(async (options) => {
    const manifest = await generateCommand(options);
    process.stdout.write(JSON.stringify(manifest.summary, null, 2) + '\n');
  });

program
  .command('sync')
  .requiredOption('--config <config>')
  .action(async (options) => {
    const report = await syncOnce(options.config);
    process.stdout.write(JSON.stringify(report, null, 2) + '\n');
    process.exitCode = report.failed > 0 ? 1 : 0;
  });

program
  .command('daemon')
  .requiredOption('--config <config>')
  .action(async (options) => {
    await runDaemon(options.config);
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

#!/usr/bin/env node
import { Command } from "commander";

import { loadConfig } from "./config/load-config.ts";
import { formatCliError, runServer } from "./runtime/run-server.ts";

const program = new Command();

program.name("gtl").description("Gen Test Load").version("1.0.2");

program
  .command("serve")
  .description("Start HTTP load generator")
  .requiredOption("--config <path>", "Path to config.yml")
  .action(async (options: { config: string }) => {
    await runCommand(async () => runServer(options.config));
  });

program
  .command("validate-config")
  .description("Validate config file")
  .requiredOption("--config <path>", "Path to config.yml")
  .action(async (options: { config: string }) => {
    await runCommand(async () => {
      await loadConfig(options.config);
      process.stdout.write("Config is valid\n");
    });
  });

program
  .command("print-config")
  .description("Print normalized config")
  .requiredOption("--config <path>", "Path to config.yml")
  .action(async (options: { config: string }) => {
    await runCommand(async () => {
      const loaded = await loadConfig(options.config);
      process.stdout.write(`${JSON.stringify(loaded.config, null, 2)}\n`);
    });
  });

await program.parseAsync();

async function runCommand(command: () => Promise<void>): Promise<void> {
  try {
    await command();
  } catch (error) {
    process.stderr.write(`${formatCliError(error)}\n`);
    process.exitCode = 1;
  }
}

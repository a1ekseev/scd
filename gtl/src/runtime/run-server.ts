import { once } from "node:events";

import { loadConfig } from "../config/load-config.ts";
import { ConfigError } from "../errors.ts";
import { createLogger } from "../logging/create-logger.ts";
import { startServer } from "./server.ts";

export async function runServer(configPath: string): Promise<void> {
  const loaded = await loadConfig(configPath);
  const logger = createLogger(loaded.config.logging);
  const server = startServer(loaded.config);

  logger.info({ listen: loaded.config.server.listen }, "gtl_server_started");

  const shutdown = async (signal: NodeJS.Signals): Promise<void> => {
    logger.info({ signal }, "gtl_server_stopping");
    server.close();
  };

  process.once("SIGINT", () => {
    void shutdown("SIGINT");
  });
  process.once("SIGTERM", () => {
    void shutdown("SIGTERM");
  });

  await once(server, "close");
  logger.info("gtl_server_stopped");
}

export function formatCliError(error: unknown): string {
  if (error instanceof ConfigError) {
    return error.message;
  }
  return error instanceof Error ? error.stack ?? error.message : String(error);
}

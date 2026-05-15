import pino from "pino";

import type { LoggingConfig } from "../types.ts";

export function createLogger(config: LoggingConfig): pino.Logger {
  if (config.format === "pretty") {
    return pino({
      level: config.level,
      transport: {
        target: "pino-pretty",
        options: {
          colorize: true,
          translateTime: "SYS:standard"
        }
      }
    });
  }

  return pino({ level: config.level });
}

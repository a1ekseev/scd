import { readFile } from "node:fs/promises";
import { parse as parseYaml } from "yaml";
import { z } from "zod";

import { ConfigError } from "../errors.ts";
import type { AppConfig, LoadedConfig } from "../types.ts";

const listenSchema = z.string().superRefine((value, context) => {
  const separator = value.lastIndexOf(":");
  if (separator <= 0 || separator === value.length - 1) {
    context.addIssue({ code: "custom", message: "listen must be in host:port format" });
    return;
  }

  const host = value.slice(0, separator);
  const port = Number(value.slice(separator + 1));
  if (host.length === 0 || !Number.isInteger(port) || port < 1 || port > 65535) {
    context.addIssue({ code: "custom", message: "listen must contain a valid host and TCP port" });
  }
});

const loadPathSchema = z.string().superRefine((value, context) => {
  if (!value.startsWith("/")) {
    context.addIssue({ code: "custom", message: "load.path must start with /" });
  }
  if (value === "/") {
    context.addIssue({ code: "custom", message: "load.path must not equal /" });
  }
  if (value.length > 1 && value.endsWith("/")) {
    context.addIssue({ code: "custom", message: "load.path must not end with /" });
  }
  if (value.includes("?") || value.includes("#")) {
    context.addIssue({ code: "custom", message: "load.path must not contain query or hash" });
  }
});

const configSchema = z
  .object({
    server: z
      .object({
        listen: listenSchema
      })
      .strict(),
    logging: z
      .object({
        level: z.enum(["fatal", "error", "warn", "info", "debug", "trace", "silent"]).default("info"),
        format: z.enum(["json", "pretty"]).default("json")
      })
      .strict()
      .default({ level: "info", format: "json" }),
    load: z
      .object({
        path: loadPathSchema,
        maxSizeKb: z
          .number()
          .int()
          .positive()
          .max(Math.floor(Number.MAX_SAFE_INTEGER / 1024))
          .default(10240)
      })
      .strict()
  })
  .strict();

export async function loadConfig(configPath: string): Promise<LoadedConfig> {
  let raw: string;
  try {
    raw = await readFile(configPath, "utf8");
  } catch (error) {
    throw new ConfigError(`Failed to read config ${configPath}: ${formatError(error)}`);
  }

  let parsed: unknown;
  try {
    parsed = parseYaml(raw);
  } catch (error) {
    throw new ConfigError(`Failed to parse config ${configPath}: ${formatError(error)}`);
  }

  const result = configSchema.safeParse(parsed);
  if (!result.success) {
    throw new ConfigError(z.prettifyError(result.error));
  }

  return {
    configPath,
    config: result.data satisfies AppConfig
  };
}

export function parseListenAddress(listen: string): { host: string; port: number } {
  const separator = listen.lastIndexOf(":");
  return {
    host: listen.slice(0, separator),
    port: Number(listen.slice(separator + 1))
  };
}

function formatError(error: unknown): string {
  return error instanceof Error ? error.message : String(error);
}

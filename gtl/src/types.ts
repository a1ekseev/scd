export type LoggingLevel = "fatal" | "error" | "warn" | "info" | "debug" | "trace" | "silent";

export type LoggingFormat = "json" | "pretty";

export interface ServerConfig {
  listen: string;
}

export interface LoggingConfig {
  level: LoggingLevel;
  format: LoggingFormat;
}

export interface LoadConfig {
  path: string;
  maxSizeKb: number;
}

export interface AppConfig {
  server: ServerConfig;
  logging: LoggingConfig;
  load: LoadConfig;
}

export interface LoadedConfig {
  configPath: string;
  config: AppConfig;
}

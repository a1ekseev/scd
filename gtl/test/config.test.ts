import { mkdtemp, rm, writeFile } from "node:fs/promises";
import { tmpdir } from "node:os";
import { join } from "node:path";
import test from "node:test";
import assert from "node:assert/strict";

import { loadConfig } from "../src/config/load-config.ts";

test("loadConfig accepts a valid config", async () => {
  await withConfig(
    `
server:
  listen: 127.0.0.1:8080
load:
  path: /load
  maxSizeKb: 64
logging:
  level: debug
  format: pretty
`,
    async (configPath) => {
      const loaded = await loadConfig(configPath);
      assert.equal(loaded.config.server.listen, "127.0.0.1:8080");
      assert.equal(loaded.config.load.maxSizeKb, 64);
      assert.equal(loaded.config.logging.level, "debug");
      assert.equal(loaded.config.logging.format, "pretty");
    }
  );
});

test("loadConfig applies defaults", async () => {
  await withConfig(
    `
server:
  listen: 0.0.0.0:8080
load:
  path: /load
`,
    async (configPath) => {
      const loaded = await loadConfig(configPath);
      assert.equal(loaded.config.load.path, "/load");
      assert.equal(loaded.config.load.maxSizeKb, 10240);
      assert.equal(loaded.config.logging.level, "info");
      assert.equal(loaded.config.logging.format, "json");
    }
  );
});

test("loadConfig rejects invalid listen address", async () => {
  await withConfig(
    `
server:
  listen: localhost
`,
    async (configPath) => {
      await assert.rejects(loadConfig(configPath), /listen must be in host:port format/);
    }
  );
});

test("loadConfig rejects non-positive maxSizeKb", async () => {
  await withConfig(
    `
server:
  listen: 127.0.0.1:8080
load:
  path: /load
  maxSizeKb: 0
`,
    async (configPath) => {
      await assert.rejects(loadConfig(configPath), /Too small/);
    }
  );
});

test("loadConfig rejects missing load.path", async () => {
  await withConfig(
    `
server:
  listen: 127.0.0.1:8080
load:
  maxSizeKb: 64
`,
    async (configPath) => {
      await assert.rejects(loadConfig(configPath), /path/);
    }
  );
});

for (const path of ["/", "load", "/load/", "/load?x=1", "/load#x"]) {
  test(`loadConfig rejects invalid load.path ${path}`, async () => {
    await withConfig(
      `
server:
  listen: 127.0.0.1:8080
load:
  path: ${JSON.stringify(path)}
  maxSizeKb: 64
`,
      async (configPath) => {
        await assert.rejects(loadConfig(configPath), /load\.path/);
      }
    );
  });
}

async function withConfig(content: string, callback: (configPath: string) => Promise<void>): Promise<void> {
  const directory = await mkdtemp(join(tmpdir(), "gtl-config-"));
  const configPath = join(directory, "config.yml");
  try {
    await writeFile(configPath, content, "utf8");
    await callback(configPath);
  } finally {
    await rm(directory, { recursive: true, force: true });
  }
}

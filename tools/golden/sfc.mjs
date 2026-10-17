// Task 0.3: reference outputs of TS sfc (refresh report, server cases, print-config,
// real-shapes, config keys). Run from the repo root with Node 24:
//   TZ=UTC node tools/golden/sfc.mjs
import { repoRoot, writeJSON, writeText } from "./lib/io.mjs";
import { FROZEN_NOW } from "./lib/freeze-time.mjs";
import { copyFileSync, mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, relative, resolve } from "node:path";

process.env.TZ = "UTC";

const { default: pino } = await import("../../sfc/node_modules/pino/pino.js");
const { loadConfig } = await import("../../sfc/dist/config/load-config.js");
const { createAppState, refreshWithConfig } = await import("../../sfc/dist/runtime/refresh.js");
const { handleServerRequest } = await import("../../sfc/dist/runtime/server.js");

const REF = "testdata/reference/sfc";
const rel = (p) => relative(repoRoot, p).split("\\").join("/");
const logger = pino({ level: "silent" });
const abs = (p) => resolve(repoRoot, p);

/** Copies a config into a temp dir, rewrites subscriptions[0].input, loads it and refreshes. */
async function refreshCopy(configRel, inputRel) {
  const dir = mkdtempSync(join(tmpdir(), "sfc-ref-"));
  try {
    const cfgPath = join(dir, "config.yml");
    copyFileSync(abs(configRel), cfgPath);
    let text = readFileSync(cfgPath, "utf8");
    const re = /^(\s+input:\s*).*$/m;
    if (!re.test(text)) throw new Error("no input line in " + configRel);
    text = text.replace(re, `$1${abs(inputRel)}`);
    const { writeFileSync } = await import("node:fs");
    writeFileSync(cfgPath, text);
    const loaded = await loadConfig(cfgPath);
    if (loaded.config.subscriptions[0].input !== abs(inputRel)) throw new Error("input not replaced");
    const state = createAppState(loaded.config);
    const report = await refreshWithConfig(loaded, state, logger);
    return { report, state, loaded };
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

// ---- 1. docker example: refresh report, outputs, server cases ------------
const fixture = "docker/fixtures/subscription.b64";
const docker = await refreshCopy("docker/sfc/config.example.yml", fixture);
if (docker.report.refreshedAt !== FROZEN_NOW) throw new Error(`time not frozen: ${docker.report.refreshedAt}`);
writeJSON(`${REF}/refresh-docker-example.json`, docker.report);
for (const o of Object.values(docker.state.outputs)) {
  if (!o.lastGoodBase64) throw new Error(`output ${o.id} has no data`);
  writeText(`${REF}/outputs/${o.id}.b64`, o.lastGoodBase64);
  writeText(`${REF}/outputs/${o.id}.plain.txt`, o.lastGoodPlain);
}
console.log("refresh docker:", docker.report.outputs.map((o) => `${o.id}=${o.matchedLines}`).join(" "));

{
  const cases = [
    { name: "healthz", path: "/healthz" },
    { name: "test-de clash", path: "/s/test-de", userAgent: "ClashMeta/1.0" },
    { name: "test-de shadowrocket", path: "/s/test-de", userAgent: "Shadowrocket/1.0" },
    { name: "test-nl", path: "/s/test-nl" },
    { name: "unknown", path: "/s/unknown" },
    { name: "dot-dot normalized", path: "/s/../s/test-all" },
    { name: "other prefix", path: "/other/test-all" },
  ];
  const out = cases.map((c) => {
    const headers = c.userAgent === undefined ? {} : { "user-agent": c.userAgent };
    const r = handleServerRequest(new URL(c.path, "http://localhost").pathname, docker.state, headers);
    return {
      name: c.name,
      path: c.path,
      ...(c.userAgent === undefined ? {} : { userAgent: c.userAgent }),
      status: r.statusCode,
      contentType: r.contentType,
      body: r.body,
      headers: r.headers ?? {},
    };
  });
  writeJSON(`${REF}/server-cases.json`, out);
  console.log("server cases:", out.map((c) => `${c.name}=${c.status}`).join(" "));
}

// ---- 2. print-config -------------------------------------------------------
async function printConfig(configRel, outRel) {
  const loaded = await loadConfig(abs(configRel));
  const cfg = JSON.parse(JSON.stringify(loaded.config));
  for (const s of cfg.subscriptions) {
    if (s.input.startsWith("/")) s.input = rel(s.input);
  }
  writeJSON(outRel, cfg);
}
await printConfig("docker/sfc/config.example.yml", `${REF}/print-config-docker-example.json`);

// ---- 3. real shapes (D33) ---------------------------------------------------
{
  const shapes = {};
  for (const [name, file] of [["provider2", "provider2.b64"], ["remnawave", "remnawave.txt"]]) {
    const r = await refreshCopy("docker/sfc/config.real-shape.yml", `docker/fixtures/real-shapes/${file}`);
    shapes[name] = r.report;
    console.log(`real-shapes ${name}:`, r.report.outputs.map((o) => `${o.id.slice(-3)}=${o.matchedLines}${o.error ? "(" + o.error + ")" : ""}`).join(" "));
  }
  writeJSON(`${REF}/real-shapes.json`, shapes);
}

// ---- 4. config keys -----------------------------------------------------------
{
  const full = await loadConfig(abs(`${REF}/config-full.yml`));
  const cfg = JSON.parse(JSON.stringify(full.config));
  const keys = new Set();
  const walk = (v, path) => {
    if (Array.isArray(v)) {
      if (v.length === 0) keys.add(`${path}[]`);
      for (const x of v) {
        if (x !== null && typeof x === "object") walk(x, `${path}[]`);
        else keys.add(`${path}[]`);
      }
    } else if (v !== null && typeof v === "object") {
      const ks = Object.keys(v);
      if (ks.length === 0) keys.add(path);
      for (const k of ks) walk(v[k], path ? `${path}.${k}` : k);
    } else {
      keys.add(path);
    }
  };
  walk(cfg, "");
  const sorted = [...keys].sort();
  writeJSON(`${REF}/config-keys.json`, sorted);
  console.log(`config-keys: ${sorted.length}`);
}
console.log("sfc: done");

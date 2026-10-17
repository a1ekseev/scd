// Task 0.2: reference outputs of the TS scd domain (parse corpus, generate,
// print-config, config keys, cron). Run from the repo root with Node 24:
//   TZ=UTC node tools/golden/scd-domain.mjs
import { FROZEN_NOW } from "./lib/freeze-time.mjs";
import { repoRoot, writeJSON, writeText } from "./lib/io.mjs";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, relative, resolve } from "node:path";

process.env.TZ = "UTC";

// Packages are imported by path: tools/golden has no node_modules.
const { CronExpressionParser } = await import("../../scd/node_modules/cron-parser/dist/index.js");
const { scanLines } = await import("../../scd/dist/subscription/scan-lines.js");
const { parseSubscriptionLine } = await import("../../scd/dist/subscription/parse-subscription-line.js");
const { normalizeVless } = await import("../../scd/dist/normalize/normalize-vless.js");
const { generateCommand } = await import("../../scd/dist/commands/generate.js");
const { loadConfig } = await import("../../scd/dist/config/load-config.js");
const { loadInputSource } = await import("../../scd/dist/input/load-input.js");

const REF = "testdata/reference";
const rel = (p) => relative(repoRoot, p).split("\\").join("/");

// ---- 1. parse corpus ----------------------------------------------------
{
  const text = readFileSync(resolve(repoRoot, REF, "scd/corpus/vless-corpus.txt"), "utf8");
  const lines = scanLines(text);
  const result = lines.map((l) => {
    let ts;
    try {
      const parsed = parseSubscriptionLine(l);
      if (!parsed.ok) {
        const s = parsed.skipped;
        ts = { ok: false, reasonCode: s.reasonCode, reason: s.reason, details: s.details, label: s.label };
      } else {
        const n = normalizeVless(parsed);
        if ("reasonCode" in n) {
          ts = { ok: false, reasonCode: n.reasonCode, reason: n.reason, details: n.details, label: n.label };
        } else {
          ts = { ok: true, label: n.label, profile: n.profile, tag: n.tag, normalized: n };
        }
      }
    } catch (e) {
      if (e instanceof URIError) ts = { error: "URI malformed" };
      else throw e;
    }
    return { line: l.line, raw: l.raw, ts };
  });
  if (lines.length !== text.split("\n").filter((x) => x !== "").length) throw new Error("corpus has blank lines");
  writeJSON(`${REF}/scd/corpus/ts-parse.json`, result);
  console.log(`ts-parse: ${result.length} lines`);
}

// ---- 2. generate --------------------------------------------------------
async function generate(name, input, { summary = false } = {}) {
  const dir = mkdtempSync(join(tmpdir(), "scd-gen-"));
  try {
    const out = join(dir, "out.json");
    const log = join(dir, "log.json");
    const abs = resolve(repoRoot, input);
    const loaded = await loadInputSource(abs);
    const manifest = await generateCommand({ input: abs, output: out, log });
    const m = JSON.parse(readFileSync(out, "utf8"));
    const l = JSON.parse(readFileSync(log, "utf8"));
    if (m.sourceFile !== abs) throw new Error(`unexpected sourceFile ${m.sourceFile}`);
    m.sourceFile = "<input>";
    writeJSON(`${REF}/scd/generate/${name}.manifest.json`, m);
    writeJSON(`${REF}/scd/generate/${name}.log.json`, l);
    if (summary) writeJSON(`${REF}/scd/generate/${name}.summary.json`, manifest.summary);
    console.log(`generate ${name}: parsed=${m.summary.parsed} skipped=${m.summary.skipped} encoding=${loaded.encoding}`);
    return { m, encoding: loaded.encoding };
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}
await generate("vpn-sample", "scd/test/fixtures/vpn-sample.txt", { summary: true });
await generate("docker-fixture", "docker/fixtures/subscription.txt", { summary: true });
{
  const r = await generate("real-shapes-provider2", "docker/fixtures/real-shapes/provider2.b64");
  if (r.encoding !== "base64") throw new Error(`provider2.b64 encoding ${r.encoding}`);
  const profiles = new Set(r.m.entries.map((e) => e.profile));
  console.log(`provider2 profiles: ${[...profiles].join(",")}`);
}
{
  const dir = mkdtempSync(join(tmpdir(), "scd-gen-"));
  try {
    const abs = resolve(repoRoot, "docker/fixtures/real-shapes/remnawave.txt");
    let message;
    try {
      await generateCommand({ input: abs, output: join(dir, "o.json"), log: join(dir, "l.json") });
      throw new Error("remnawave generate unexpectedly succeeded");
    } catch (e) {
      message = e.message;
    }
    if (message.includes("unexpectedly")) throw new Error(message);
    writeText(`${REF}/scd/generate/real-shapes-remnawave.error.txt`, message.split(abs).join("<input>") + "\n");
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

// ---- 3. print-config ----------------------------------------------------
async function printConfig(configRel, outRel) {
  const loaded = await loadConfig(resolve(repoRoot, configRel));
  const cfg = JSON.parse(JSON.stringify(loaded.config));
  for (const s of cfg.subscriptions) {
    if (s.input.startsWith("/")) s.input = rel(s.input);
  }
  writeJSON(outRel, cfg);
  return loaded.config;
}
await printConfig("docker/scd/config.example.yml", `${REF}/scd/print-config/docker-example.json`);
await printConfig(`${REF}/scd/print-config/minimal.yml`, `${REF}/scd/print-config/minimal.json`);

// ---- 4. config keys -----------------------------------------------------
{
  const full = await loadConfig(resolve(repoRoot, REF, "scd/config-full.yml"));
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
  writeJSON(`${REF}/scd/config-keys.json`, sorted);
  console.log(`config-keys: ${sorted.length}`);
}

// ---- 5. cron ------------------------------------------------------------
const FROM = FROZEN_NOW;
{
  const exprs = [
    "*/5 * * * *", "*/10 * * * *", "*/15 * * * * *", "0 3 * * *", "0 0 1 1 *", "0 9 * * MON-FRI",
    "30 2 1,15 * *", "0 */2 * * *", "5 4 * JAN,JUL SUN", "0 0 1 * 1", "@hourly", "@daily", "@weekly",
    "@monthly", "@yearly", "0 0 12 * * *",
  ];
  const rows = exprs.map((expr) => {
    const it = CronExpressionParser.parse(expr, { currentDate: new Date(FROM) });
    return { expr, from: FROM, next: [it.next().toISOString(), it.next().toISOString(), it.next().toISOString()] };
  });
  writeJSON(`${REF}/common/cron-next.json`, rows);
}
{
  // Recorded: how the TS cron-parser treats expressions that Go rejects (informational).
  const library = ["* * * * * * *", "0 0 L * *", "0 0 * * 1#2", "0 0 1W * *", "H * * * *", "* * * * 7", "@minutely", "@weekdays"];
  const guards = ["", "0 0 31 2 *", "0 0 30 2 *"];
  const probe = (expr) => {
    const r = { expr };
    try {
      const it = CronExpressionParser.parse(expr, { currentDate: new Date(FROM) });
      r.tsParse = "ok";
      try {
        r.tsNext = it.next().toISOString();
      } catch (e) {
        r.tsNext = `error: ${e.message}`;
      }
    } catch (e) {
      r.tsParse = `error: ${e.message}`;
    }
    return r;
  };
  const rows = [
    ...library.map((e) => ({ ...probe(e), why: "rejected by robfig/cron/v3 itself" })),
    ...guards.map((e) => ({ ...probe(e), why: "scd guard: empty or never-firing expression" })),
  ];
  writeJSON(`${REF}/common/cron-rejected.json`, rows);
}
console.log("scd-domain: done");

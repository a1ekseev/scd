// Task 0.4: reference screenshots of the TS status dashboard. Runs on the HOST (the
// node:24-alpine container has no browser), after tools/golden/status.mjs:
//   node tools/golden/screenshots.mjs
// Browser: env GOLDEN_CHROME, default Playwright headless_shell; falls back to the full
// Playwright Chromium when the default headless_shell is missing.
// Output: testdata/reference/scd/status/<name>/screenshot-{1280,390}.png (each exactly <w>x2400).
import { spawnSync } from "node:child_process";
import { existsSync, readdirSync, readFileSync, rmSync, statSync } from "node:fs";
import { join, resolve } from "node:path";
import { pathToFileURL } from "node:url";
import { repoRoot } from "./lib/io.mjs";

const DEFAULT_SHELL = "/opt/pw-browsers/chromium_headless_shell-1194/chrome-linux/headless_shell";
const FALLBACK_CHROME = "/opt/pw-browsers/chromium-1194/chrome-linux/chrome";
const WIDTHS = [1280, 390];
const HEIGHT = 2400;

let chrome = process.env.GOLDEN_CHROME;
if (!chrome) {
  chrome = existsSync(DEFAULT_SHELL) ? DEFAULT_SHELL : FALLBACK_CHROME;
}
if (!existsSync(chrome)) throw new Error(`browser not found: ${chrome} (set GOLDEN_CHROME)`);

function pngSize(file) {
  const b = readFileSync(file);
  if (b.length < 24 || b.toString("latin1", 1, 4) !== "PNG" || b.toString("latin1", 12, 16) !== "IHDR") {
    throw new Error(`${file}: not a PNG`);
  }
  return { width: b.readUInt32BE(16), height: b.readUInt32BE(20) };
}

const statusDir = resolve(repoRoot, "testdata/reference/scd/status");
const names = readdirSync(join(statusDir, "fixtures"))
  .filter((f) => f.endsWith(".json"))
  .map((f) => f.slice(0, -5))
  .sort();
if (names.length === 0) throw new Error("no fixtures");

const version = spawnSync(chrome, ["--version"], { encoding: "utf8" });
console.log(`browser: ${chrome} ${(version.stdout || "").trim()}`);

for (const name of names) {
  const html = join(statusDir, name, "index.html");
  if (!existsSync(html)) throw new Error(`${html} missing: run tools/golden/status.mjs first`);
  for (const width of WIDTHS) {
    const out = join(statusDir, name, `screenshot-${width}.png`);
    rmSync(out, { force: true });
    const args = [
      "--headless",
      "--no-sandbox",
      "--disable-gpu",
      "--hide-scrollbars",
      "--force-device-scale-factor=1",
      `--window-size=${width},${HEIGHT}`,
      `--screenshot=${out}`,
      pathToFileURL(html).href,
    ];
    const r = spawnSync(chrome, args, { encoding: "utf8", timeout: 60_000 });
    if (r.status !== 0 || !existsSync(out) || statSync(out).size === 0) {
      throw new Error(`${name}@${width}: screenshot failed (exit ${r.status ?? r.signal})\n${r.stderr}`);
    }
    const size = pngSize(out);
    if (size.width !== width || size.height !== HEIGHT) {
      throw new Error(`${name}@${width}: PNG is ${size.width}x${size.height}, want ${width}x${HEIGHT}`);
    }
    console.log(`screenshot ${name}@${width}: ${statSync(out).size} bytes`);
  }
}

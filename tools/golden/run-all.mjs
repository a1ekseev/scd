// Runs golden reference scripts sequentially; missing ones are skipped.
// screenshots.mjs (task 0.4) is host-only and intentionally not part of this list.
import { spawnSync } from "node:child_process";
import { existsSync } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const dir = dirname(fileURLToPath(import.meta.url));
const scripts = [
  "scd-domain.mjs", // 0.2
  "sfc.mjs", // 0.3
  "gtl.mjs", // 0.3
  "status.mjs", // 0.4
  "label-filter.mjs", // 0.6
];

for (const name of scripts) {
  const file = join(dir, name);
  if (!existsSync(file)) {
    console.log(`skipped: ${name} (not present yet)`);
    continue;
  }
  console.log(`running: ${name}`);
  const r = spawnSync(process.execPath, [file], { stdio: "inherit" });
  if (r.status !== 0) {
    console.error(`failed: ${name} (exit ${r.status ?? r.signal})`);
    process.exit(r.status ?? 1);
  }
}
console.log("done");

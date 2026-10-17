import { mkdirSync, writeFileSync } from "node:fs";
import { dirname, resolve } from "node:path";
import { fileURLToPath } from "node:url";

/** Absolute repository root (tools/golden/lib -> repo). */
export const repoRoot = resolve(dirname(fileURLToPath(import.meta.url)), "..", "..", "..");

function target(relPath) {
  const abs = resolve(repoRoot, relPath);
  mkdirSync(dirname(abs), { recursive: true });
  return abs;
}

/** Writes value as JSON (2-space indent, trailing newline) to a repo-relative path. */
export function writeJSON(relPath, value) {
  writeFileSync(target(relPath), JSON.stringify(value, null, 2) + "\n");
}

/** Writes text as-is to a repo-relative path. */
export function writeText(relPath, text) {
  writeFileSync(target(relPath), text);
}

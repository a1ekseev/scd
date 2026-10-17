// Task 0.6: decisions of the three production TS label regexes (D29) over a label corpus.
// Run from the repo root with Node 24:
//   node tools/golden/label-filter.mjs
import { repoRoot, writeJSON } from "./lib/io.mjs";
import { readFileSync } from "node:fs";
import { join } from "node:path";

const { parseRegexLiteral } = await import("../../sfc/dist/config/load-config.js");

const FILTERS = {
  en: "/^(?!.*Extra)(?!.*WhiteList).*$/i",
  extra: "/^(?=.*Extra)(?!.*WhiteList).*$/i",
  extraWhitelist: "/(?=.*Extra)(?=.*WhiteList)/i",
};
const regexes = Object.fromEntries(Object.entries(FILTERS).map(([k, v]) => [k, parseRegexLiteral(v)]));

/** Label = fragment after the first '#', decodeURIComponent. */
function labelsOf(file, expected) {
  const lines = readFileSync(join(repoRoot, "docker/fixtures/real-shapes", file), "utf8")
    .split("\n")
    .filter((l) => l.trim() !== "");
  const labels = lines.map((l) => {
    const i = l.indexOf("#");
    if (i < 0) throw new Error(`no '#' in a ${file} line`);
    return decodeURIComponent(l.slice(i + 1));
  });
  if (labels.length !== expected) throw new Error(`${file}: expected ${expected} labels, got ${labels.length}`);
  return labels;
}

const entries = [
  ...labelsOf("remnawave.txt", 4).map((label) => ({ label, source: "remnawave" })),
  ...labelsOf("provider2.txt", 50).map((label) => ({ label, source: "provider2" })),
];

const edge = [
  "🇩🇪DE-1-Extra", "🇳🇱NL-2-X WhiteList", "🇩🇪 WhiteList Extra", "extrawhitelist",
  "🇷🇺 Москва, Extra, WhiteList", "Extra\nWhiteList", "WhiteList\nExtra", "Extra\nfoo",
  "", " ", "Extra", "EXTRA", "extra", "ExTrA", "WhiteList", "Whitelist", "WHITELIST", "whitelist",
  "Extra WhiteList", "WhiteList Extra", "EXTRA WHITELIST", "extra whitelist", "Extra-Whitelist",
  "🇩🇪 Germany, Frankfurt", "🇺🇸 United States, San-Jose Extra", "🇯🇵 Tokyo WhiteList",
  "🇯🇵", "🇬🇧🇬🇧 London", "Россия, Москва", "Россия, Москва Extra", "Россия, Москва WhiteList",
  "Москва Extra WhiteList", "Extras", "NotWhiteListed", "Ext ra", "White List", "foo\nbar",
];
for (const label of edge) entries.push({ label, source: "edge" });

const out = entries.map(({ label, source }) => {
  const r = { label, source };
  for (const [k, re] of Object.entries(regexes)) {
    re.lastIndex = 0;
    r[k] = re.test(label);
  }
  return r;
});

const real = out.filter((e) => e.source !== "edge").length;
if (real !== 54) throw new Error(`expected 54 real labels, got ${real}`);
if (out.length < 74) throw new Error(`corpus too small: ${out.length}`);

writeJSON("testdata/reference/common/label-filter.json", out);
const combos = {};
for (const e of out) {
  const k = `en=${e.en} extra=${e.extra} extraWhitelist=${e.extraWhitelist}`;
  combos[k] = (combos[k] ?? 0) + 1;
}
console.log(`label-filter: ${out.length} labels (${real} real)`, combos);

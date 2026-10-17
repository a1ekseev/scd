// Task 0.3: reference outputs of TS gtl (payload vectors, routes, print-config, config keys).
// Run from the repo root with Node 24:
//   TZ=UTC node tools/golden/gtl.mjs
import { repoRoot, writeJSON } from "./lib/io.mjs";
import "./lib/freeze-time.mjs";
import { createHash } from "node:crypto";
import { relative, resolve } from "node:path";
import { Writable } from "node:stream";
import { createServer as createNetServer } from "node:net";

process.env.TZ = "UTC";

const { loadConfig } = await import("../../gtl/dist/config/load-config.js");
const { buildResponse, fillDeterministicPayload, startServer, writeRepeatedPayload } = await import(
  "../../gtl/dist/runtime/server.js"
);

const REF = "testdata/reference/gtl";
const abs = (p) => resolve(repoRoot, p);

// ---- 1. payload vectors ---------------------------------------------------
async function payload(size) {
  const hash = createHash("sha256");
  const first = Buffer.alloc(Math.min(16, size));
  let seen = 0;
  const sink = new Writable({
    write(chunk, _enc, cb) {
      if (seen < first.length) chunk.copy(first, seen, 0, Math.min(chunk.length, first.length - seen));
      seen += chunk.length;
      hash.update(chunk);
      cb();
    },
  });
  await writeRepeatedPayload(sink, size);
  await new Promise((r) => sink.once("finish", r));
  if (seen !== size) throw new Error(`size mismatch ${seen} != ${size}`);
  return { sizeBytes: size, sha256: hash.digest("hex"), first16hex: first.toString("hex") };
}
{
  const sizes = [0, 1, 16, 1024, 65536, 65537, 98311, 10485760];
  const vectors = [];
  for (const s of sizes) vectors.push(await payload(s));
  // fill8 entries are appended to the same array (jq '.[] | select(.sizeBytes==N)' must keep working).
  const fill8 = [0, 65536, 2 ** 32, 2 ** 32 + 65536].map((offset) => {
    const b = Buffer.alloc(8);
    fillDeterministicPayload(b, offset);
    return { offset, fill8: b.toString("hex") };
  });
  const v1024 = vectors.find((v) => v.sizeBytes === 1024).sha256;
  if (v1024 !== "8ab783947bae918d01b74448625dfd0fef119e16e97d6fc9fdf15e310c87cb01") {
    console.error(`CONCERN: sha256(1024) = ${v1024} differs from the brief`);
  }
  writeJSON(`${REF}/payload-vectors.json`, [...vectors, ...fill8]);
  console.log("payload sizes:", vectors.map((v) => v.sizeBytes).join(","));
  console.log("fill8:", fill8.map((f) => f.fill8).join(" "));
}

// ---- 2. routes ------------------------------------------------------------
const docker = await loadConfig(abs("docker/gtl/config.example.yml"));
{
  const config = { ...docker.config, load: { path: "/load", maxSizeKb: 64 } };
  const paths = [
    "/healthz", "/load/32", "/load/0", "/load/007", "/load/1?x=y", "/load/65", "/load/", "/load/1/",
    "/load", "/load/200_32", "/load/%33", "/unknown", "/load/99999999999999999999",
  ];
  const routes = paths.map((path) => {
    const b = buildResponse(new URL(path, "http://127.0.0.1").pathname, config);
    return {
      path,
      method: "GET",
      status: b.statusCode,
      headers: b.headers,
      ...(b.errorBody === undefined ? { bodyBytes: b.bodyBytes } : { body: b.errorBody }),
    };
  });

  // HEAD -> 405 exists only in the unexported handleRequest: use a real server.
  const port = await new Promise((res, rej) => {
    const s = createNetServer();
    s.once("error", rej);
    s.listen(0, "127.0.0.1", () => {
      const { port } = s.address();
      s.close(() => res(port));
    });
  });
  const server = startServer({ ...config, server: { listen: `127.0.0.1:${port}` } });
  try {
    await new Promise((res, rej) => {
      if (server.listening) res();
      server.once("listening", res);
      server.once("error", rej);
    });
    const r = await fetch(`http://127.0.0.1:${port}/load/1`, { method: "HEAD" });
    await r.arrayBuffer();
    const headers = {};
    r.headers.forEach((v, k) => {
      if (!["date", "connection", "keep-alive"].includes(k)) headers[k] = v;
    });
    routes.push({ path: "/load/1", method: "HEAD", status: r.status, headers, body: "method not allowed\n" });
  } finally {
    server.close();
    server.closeAllConnections?.();
  }
  writeJSON(`${REF}/routes.json`, routes);
  console.log("routes:", routes.map((r) => `${r.method} ${r.path}=${r.status}`).join(" | "));
}

// ---- 3. print-config ---------------------------------------------------------
writeJSON(`${REF}/print-config-docker-example.json`, JSON.parse(JSON.stringify(docker.config)));

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
console.log("gtl: done");

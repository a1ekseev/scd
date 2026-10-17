// Task 0.4: reference outputs of the TS scd status server and runtime-state.
// Run from the repo root with Node 24 (container template in tools/golden/README.md):
//   TZ=UTC node tools/golden/status.mjs
// Inputs:  testdata/reference/scd/status/fixtures/<name>.json (fixture contract: testdata/reference/README.md)
// Outputs: testdata/reference/scd/status/<name>/{index.html,api-status.json,api-instance-status.json,api-instances.json}
//          testdata/reference/scd/status/runtime-state/{cases.json,<case>.json}
import { FROZEN_NOW } from "./lib/freeze-time.mjs";
import { repoRoot, writeJSON, writeText } from "./lib/io.mjs";
import { mkdtempSync, readdirSync, readFileSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, resolve } from "node:path";

process.env.TZ = "UTC";

const { loadConfig } = await import("../../scd/dist/config/load-config.js");
const { createSyncMemoryState, getOrCreateTargetState, replaceTargetTopology, buildTargetStateKey } = await import(
  "../../scd/dist/runtime/run-state.js"
);
const { createStatusAggregationState, collectRemoteStatus } = await import("../../scd/dist/runtime/status-aggregation.js");
const { handleStatusServerRequest } = await import("../../scd/dist/runtime/status-server.js");
const { buildCurrentRuntimeStateSnapshot } = await import("../../scd/dist/runtime/current-runtime-state.js");
const { buildManifest } = await import("../../scd/dist/manifest.js");
const { buildTargetTopology } = await import("../../scd/dist/topology/build-tunnel-topology.js");
const { buildOutboundGrpc } = await import("../../scd/dist/builders/build-outbound-grpc.js");
const { buildInboundGrpc } = await import("../../scd/dist/builders/build-inbound-grpc.js");
const { concatBytes, encodeRepeatedStringField, encodeStringField } = await import("../../scd/dist/api/protobuf.js");

const REF = "testdata/reference/scd/status";
const ENDPOINTS = [
  ["/", "index.html"],
  ["/api/status", "api-status.json"],
  ["/api/instance-status", "api-instance-status.json"],
  ["/api/instances", "api-instances.json"],
];
const silentLogger = { info() {}, warn() {} };

function fail(message) {
  throw new Error(message);
}

function byteCompare(a, b) {
  const x = Buffer.from(a, "utf8");
  const y = Buffer.from(b, "utf8");
  return Buffer.compare(x, y);
}

function assertSortedBytes(list, key, what) {
  for (let i = 1; i < list.length; i += 1) {
    if (key(list[i - 1], list[i]) > 0) fail(`${what}: ICU order differs from byte order at index ${i}`);
  }
}

// ---- config -------------------------------------------------------------
async function loadFixtureConfig(dir, fc) {
  if (!fc.instanceId) fail("config.instanceId is required (os.hostname() must not leak into references)");
  const yaml = {
    subscriptions: fc.subscriptions.map((s) => {
      const target = { address: s.address };
      if (s.observatorySubjectSelectorPrefix !== undefined) {
        target.observatorySubjectSelectorPrefix = s.observatorySubjectSelectorPrefix;
      }
      return { id: s.id, enabled: s.enabled, input: "./sub.txt", target };
    }),
    runtime: { mode: "run-once" },
    logging: {},
    resources: {},
    statusServer: {
      enabled: true,
      listen: "127.0.0.1:9090",
      instanceId: fc.instanceId,
      remoteInstances: fc.remoteInstances,
      runtimeState: fc.runtimeState,
    },
  };
  const path = join(dir, "config.yml");
  // JSON is valid YAML; loadConfig applies the same defaults as for a hand-written file.
  writeFileSync(path, JSON.stringify(yaml, null, 2));
  return loadConfig(path);
}

// ---- memory state -------------------------------------------------------
function buildMemoryState(fixture) {
  const mem = createSyncMemoryState();
  for (const t of fixture.targets) {
    const configured = fixture.config.subscriptions.find((s) => s.id === t.subscriptionId);
    if (!configured || configured.address !== t.targetAddress) {
      fail(`target ${t.subscriptionId}::${t.targetAddress} does not match a configured subscription`);
    }
    const key = buildTargetStateKey(t.subscriptionId, t.targetAddress);
    if (mem.targets[key]) fail(`duplicate target ${key}`);
    const state = getOrCreateTargetState(mem, key);
    const tunnels = t.tunnels.map((x) => ({
      baseTunnelId: x.baseTunnelId,
      displayName: x.displayName,
      countryIso2: x.countryIso2,
      baseOutboundTag: x.outboundWithoutPrefixTag,
      outboundTagInitial: x.outboundTagInitial,
      outboundTagCurrent: x.outboundTagCurrent,
      inboundTag: `in-${x.baseTunnelId}`,
      routeTag: `route-${x.baseTunnelId}`,
      listen: x.listen,
      port: x.port,
      outboundInitial: { tag: x.outboundTagInitial },
      outboundWithoutPrefix: { tag: x.outboundWithoutPrefixTag },
    }));
    replaceTargetTopology(state, { tunnels }, {});
    for (const x of t.tunnels) {
      if (typeof x.monitor?.state !== "string" || typeof x.monitor.consecutiveFailures !== "number") {
        fail(`tunnel ${x.baseTunnelId}: monitor.state and monitor.consecutiveFailures are required`);
      }
      state.tunnels[x.baseTunnelId].monitor = { ...x.monitor };
    }
    if (Object.keys(state.tunnels).length !== t.tunnels.length) fail(`${key}: duplicate baseTunnelId`);
    const b = t.balancerMonitor;
    if (typeof b?.state !== "string" || typeof b.remotePingState !== "string" || typeof b.consecutiveFailures !== "number") {
      fail(`${key}: balancerMonitor.state, remotePingState, consecutiveFailures are required`);
    }
    state.balancerMonitor = { ...b };
  }
  return mem;
}

// ---- aggregation state --------------------------------------------------
async function buildAggregationState(fixture, loaded) {
  const agg = createStatusAggregationState(loaded.config.statusServer.remoteInstances);
  for (const r of fixture.remotes) {
    const configured = fixture.config.remoteInstances.find((x) => x.id === r.id);
    if (!configured || configured.apiUrl !== r.apiUrl) fail(`remote ${r.id} does not match config.remoteInstances`);
    const cur = agg.remotes[r.id];
    // Same key order as collectRemoteStatus: id, source, apiUrl, lastAttemptAt, snapshot, lastSuccessAt, lastError.
    if (r.lastAttemptAt !== undefined) cur.lastAttemptAt = r.lastAttemptAt;
    if (r.snapshot !== undefined) {
      // Validate the snapshot with the real TS parser (collectRemoteStatus on a scratch state).
      const scratch = createStatusAggregationState([]);
      await collectRemoteStatus({ ...configured, timeoutMs: 5000 }, scratch, silentLogger, {
        fetchFn: async () => new Response(JSON.stringify(r.snapshot), { status: 200 }),
      });
      if (scratch.remotes[r.id].lastError) fail(`remote ${r.id}: snapshot rejected: ${scratch.remotes[r.id].lastError}`);
      cur.snapshot = r.snapshot;
    }
    if (r.lastSuccessAt !== undefined) cur.lastSuccessAt = r.lastSuccessAt;
    if (r.lastError !== undefined) cur.lastError = r.lastError;
  }
  return agg;
}

// ---- status fixtures ----------------------------------------------------
const fixtureDir = resolve(repoRoot, REF, "fixtures");
const names = readdirSync(fixtureDir)
  .filter((f) => f.endsWith(".json"))
  .map((f) => f.slice(0, -5))
  .sort();
if (names.length < 10) fail(`expected at least 10 fixtures, got ${names.length}`);

for (const name of names) {
  const fixture = JSON.parse(readFileSync(join(fixtureDir, `${name}.json`), "utf8"));
  if (fixture.now !== FROZEN_NOW) fail(`${name}: fixture.now ${fixture.now} != frozen ${FROZEN_NOW}`);
  const dir = mkdtempSync(join(tmpdir(), "scd-status-"));
  try {
    const loaded = await loadFixtureConfig(dir, fixture.config);
    const mem = buildMemoryState(fixture);
    const agg = await buildAggregationState(fixture, loaded);
    const out = {};
    for (const [path, file] of ENDPOINTS) {
      const res = await handleStatusServerRequest(path, loaded, mem, { aggregationState: agg });
      if (res.statusCode !== 200) fail(`${name} ${path}: status ${res.statusCode}`);
      const json = file.endsWith(".json");
      const expectedType = json ? "application/json; charset=utf-8" : "text/html; charset=utf-8";
      if (res.contentType !== expectedType) fail(`${name} ${path}: content-type ${res.contentType}`);
      out[file] = json ? JSON.parse(res.body) : res.body;
      if (json) writeJSON(`${REF}/${name}/${file}`, out[file]);
      else writeText(`${REF}/${name}/${file}`, res.body + "\n");
    }

    // Self-checks: every sorted list must be in byte order too (Go uses strings.Compare, D23).
    const flat = out["api-status.json"].tunnels;
    assertSortedBytes(
      flat,
      (a, b) => byteCompare(a.subscriptionId, b.subscriptionId) || byteCompare(a.targetAddress, b.targetAddress) ||
        byteCompare(a.displayName, b.displayName),
      `${name} /api/status`,
    );
    const local = out["api-instance-status.json"];
    if (local.instanceId !== fixture.config.instanceId) fail(`${name}: instanceId mismatch`);
    assertSortedBytes(local.subscriptions, (a, b) => byteCompare(a.subscriptionId, b.subscriptionId), `${name} subscriptions`);
    for (const s of local.subscriptions) {
      assertSortedBytes(s.targets, (a, b) => byteCompare(a.targetAddress, b.targetAddress), `${name} ${s.subscriptionId} targets`);
      for (const t of s.targets) {
        const lat = (x) => (x.state === "idle" || x.lastLatencyMs === undefined ? Infinity : x.lastLatencyMs);
        assertSortedBytes(
          t.tunnels,
          (a, b) => (lat(a) === lat(b) ? byteCompare(a.displayName, b.displayName) : lat(a) - lat(b)),
          `${name} ${s.subscriptionId}/${t.targetAddress} tunnels`,
        );
      }
    }
    const instances = out["api-instances.json"].instances;
    assertSortedBytes(instances.slice(1), (a, b) => byteCompare(a.id, b.id), `${name} remote instances`);
    const enabled = fixture.config.subscriptions.filter((s) => s.enabled).length;
    if (local.subscriptions.length !== enabled) fail(`${name}: ${local.subscriptions.length} subscriptions, want ${enabled}`);
    if (out["index.html"].includes("<script>alert")) fail(`${name}: unescaped hostile string in HTML`);
    console.log(`status ${name}: tunnels=${flat.length} subscriptions=${local.subscriptions.length} instances=${instances.length}`);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}

// ---- runtime-state ------------------------------------------------------
// Inputs mirror scd/test/runtime-state.test.ts (03 section 10.5 items 1-6) with fake hosts only.
// Runtime items: {fixedTag, rawHex} = opaque bytes; {tunnel, variant} = built from the topology of the
// subscription line (variant "initial" = outboundInitial, "withoutPrefix" = outboundWithoutPrefix).
const LINE_TLS = "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@vpn.example.test:443?type=tcp&security=tls#🇦🇹 Вена, Австрия, Extra";
const LINE_REALITY =
  "vless://7d1b6590-1069-4372-92be-8d0a0ae6eaf5@vpn.example.test:443?security=reality&encryption=none&fp=chrome&headerType=none&type=tcp&flow=xtls-rprx-vision&sni=io.example.test&pbk=OHWtGCfZc0FXULj2ZYeW_vlKIlurak79i6AXun9oJQg&sid=cbad7f159438fee4#🇩🇪 Германия, Extra";
const MONITORS = {
  monitor: {
    enabled: true,
    schedule: "*/2 * * * *",
    request: { url: "https://monitor.example.test/health?token=secret", method: "GET", expectedStatus: 204, timeoutMs: 5000 },
  },
  balancerMonitor: {
    enabled: true,
    schedule: "*/2 * * * *",
    socks5: { host: "127.0.0.1", port: 1080 },
    request: { url: "https://balancer.example.test/check?token=secret", method: "GET", expectedStatus: 204, timeoutMs: 5000 },
    remotePing: { enabled: true, url: "https://push.example.test/api/push/token?secret=abc", timeoutMs: 5000, viaSocks: true },
  },
};
const MANAGED = [{ tunnel: 0, variant: "initial" }];
const CASES = [
  {
    name: "redacted-default",
    line: LINE_TLS,
    target: {},
    options: {},
    runtime: { outbounds: [{ fixedTag: "direct", rawHex: "010203" }, ...MANAGED], inbounds: [{ tunnel: 0 }], rules: [{ tunnel: 0 }] },
  },
  {
    name: "vision-udp443",
    line: LINE_REALITY,
    target: { visionUdp443Override: true },
    options: {},
    runtime: { outbounds: MANAGED, inbounds: [], rules: [] },
  },
  {
    name: "repaired-fallback",
    line: LINE_TLS,
    target: {},
    options: {},
    runtime: { outbounds: [{ tunnel: 0, variant: "withoutPrefix" }], inbounds: [], rules: [] },
  },
  {
    name: "remote-input",
    input: "https://example.test/tokenized/path?secret=abc",
    target: {},
    options: {},
    balancerMonitor: { state: "healthy", remotePingState: "idle", consecutiveFailures: 0, lastStatusCode: 204 },
    runtime: { outbounds: [], inbounds: [], rules: [] },
  },
  {
    name: "redact-monitor-urls",
    input: "https://subscription.example.test/tokenized/path?secret=abc",
    target: MONITORS,
    options: {},
    runtime: { outbounds: [], inbounds: [], rules: [] },
  },
  {
    name: "redact-monitor-urls-include-secrets",
    input: "https://subscription.example.test/tokenized/path?secret=abc",
    target: MONITORS,
    options: { includeSecrets: true },
    runtime: { outbounds: [], inbounds: [], rules: [] },
  },
  {
    name: "raw-and-secrets",
    line: LINE_TLS,
    target: {},
    options: { includeRaw: true, includeSecrets: true },
    runtime: { outbounds: MANAGED, inbounds: [], rules: [] },
  },
];
const RS_SUB_ID = "source-1";
const RS_ADDRESS = "127.0.0.1:8080";

function hexBytes(hex) {
  return new Uint8Array(Buffer.from(hex, "hex"));
}

for (const c of CASES) {
  const dir = mkdtempSync(join(tmpdir(), "scd-runtime-state-"));
  try {
    let input = c.input;
    if (c.line !== undefined) {
      input = join(dir, "subscription.txt");
      writeFileSync(input, c.line, "utf8");
    }
    const yaml = {
      subscriptions: [{
        id: RS_SUB_ID,
        input,
        format: "plain",
        target: {
          address: RS_ADDRESS,
          fixedOutbounds: ["direct", "blocked"],
          inboundSocks: { listen: "127.0.0.1", portRange: { start: 20000, end: 20010 } },
          observatorySubjectSelectorPrefix: "x-observe-",
          ...c.target,
        },
      }],
      runtime: { mode: "run-once" },
      logging: {},
      resources: { outbounds: { enabled: true }, inbounds: { enabled: true }, routing: { enabled: true } },
      statusServer: { enabled: false, instanceId: "local-main" },
    };
    const cfgPath = join(dir, "config.yml");
    writeFileSync(cfgPath, JSON.stringify(yaml, null, 2));
    const loaded = await loadConfig(cfgPath);
    const subscription = loaded.config.subscriptions[0];
    const target = subscription.target;

    let tunnels = [];
    if (c.line !== undefined) {
      tunnels = buildTargetTopology(buildManifest(c.line, RS_SUB_ID), target).tunnels;
      if (tunnels.length !== 1) fail(`${c.name}: expected 1 tunnel, got ${tunnels.length}`);
    }
    const tun = (i) => tunnels[i] ?? fail(`${c.name}: no tunnel ${i}`);
    const outbounds = c.runtime.outbounds.map((o) => {
      if (o.fixedTag !== undefined) return { tag: o.fixedTag, raw: hexBytes(o.rawHex) };
      const p = o.variant === "initial" ? tun(o.tunnel).outboundInitial : tun(o.tunnel).outboundWithoutPrefix;
      return { tag: p.tag, raw: buildOutboundGrpc(p.normalized).raw };
    });
    const inbounds = c.runtime.inbounds.map((i) => ({ tag: tun(i.tunnel).inboundTag, raw: buildInboundGrpc(tun(i.tunnel)).raw }));
    const rules = c.runtime.rules.map((r) => {
      const t = tun(r.tunnel);
      return {
        tag: t.outboundTagCurrent,
        ruleTag: t.routeTag,
        inboundTags: [t.inboundTag],
        raw: concatBytes([
          encodeStringField(1, t.outboundTagCurrent),
          encodeRepeatedStringField(8, [t.inboundTag]),
          encodeStringField(19, t.routeTag),
        ]),
      };
    });

    const deps = {
      createClient: () => ({
        listOutbounds: async () => outbounds,
        listInbounds: async () => inbounds,
        listRules: async () => rules,
      }),
    };
    if (c.input !== undefined) {
      deps.loadLocalInputFn = async () => fail(`${c.name}: remote input must not be loaded`);
    }
    if (c.balancerMonitor) {
      const mem = createSyncMemoryState();
      getOrCreateTargetState(mem, buildTargetStateKey(RS_SUB_ID, RS_ADDRESS)).balancerMonitor = { ...c.balancerMonitor };
      deps.memoryState = mem;
    }
    const snapshot = await buildCurrentRuntimeStateSnapshot(loaded, RS_SUB_ID, RS_ADDRESS, deps, c.options);
    if (!snapshot) fail(`${c.name}: snapshot is undefined`);

    // Replace the temp input path (config.subscription.input, expected.source) with a placeholder.
    let text = JSON.stringify(snapshot);
    if (c.line !== undefined) text = text.split(JSON.stringify(input).slice(1, -1)).join("<input>");
    if (text.includes(dir)) fail(`${c.name}: temp path leaked`);
    const out = JSON.parse(text);

    // Assumptions from 03 section 10.5.
    const ob = out.runtime.outbounds;
    const check = (cond, what) => cond || fail(`${c.name}: ${what}`);
    switch (c.name) {
      case "redacted-default":
        check(ob[0].classification === "fixed" && ob[1].classification === "managed-initial", "classification");
        check(ob[1].parsed?.protocol === "vless" && ob[1].parsed.uuid === undefined && ob[1].rawBase64 === undefined, "redaction");
        check(out.runtime.inbounds[0].classification === "managed" && out.runtime.inbounds[0].parsed?.protocol === "socks", "inbound");
        check(out.runtime.routingRules[0].classification === "managed", "routing");
        check(out.serviceState.balancerMonitor === undefined, "serviceState");
        check(out.expected.source === "<input>" && out.config.subscription.input === "<input>", "input placeholder");
        break;
      case "vision-udp443":
        check(out.expected.outbounds[0].flow === "xtls-rprx-vision-udp443", "expected flow");
        check(ob[0].parsed?.flow === "xtls-rprx-vision-udp443", "runtime flow");
        break;
      case "repaired-fallback":
        check(out.diff.outbounds.matched.length === 0 && out.diff.outbounds.matchedFallback.length === 1, "fallback diff");
        check(out.diff.outbounds.missing.length === 0, "missing");
        break;
      case "remote-input":
        check(out.config.subscription.input === "https://example.test/<redacted>", "input redaction");
        check(/does not refetch subscription sources/.test(out.expected.error ?? ""), "expected.error");
        check(out.serviceState.balancerMonitor?.state === "healthy", "balancerMonitor");
        break;
      case "redact-monitor-urls":
        check(out.config.target.monitor.request.url === "https://monitor.example.test/<redacted>", "monitor url");
        check(out.config.target.balancerMonitor.request.url === "https://balancer.example.test/<redacted>", "balancer url");
        check(out.config.target.balancerMonitor.remotePing.url === "https://push.example.test/<redacted>", "push url");
        check(out.config.target.balancerMonitor.remotePing.viaSocks === true, "viaSocks");
        break;
      case "redact-monitor-urls-include-secrets":
        check(out.config.target.monitor.request.url === MONITORS.monitor.request.url, "monitor url exposed");
        check(out.config.subscription.input === c.input, "input exposed");
        break;
      case "raw-and-secrets":
        check(typeof ob[0].rawBase64 === "string" && ob[0].parsed?.uuid === "7d1b6590-1069-4372-92be-8d0a0ae6eaf5", "raw+uuid");
        break;
      default:
        fail(`unchecked case ${c.name}`);
    }
    writeJSON(`${REF}/runtime-state/${c.name}.json`, out);
    console.log(`runtime-state ${c.name}: outbounds=${ob.length}`);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
}
writeJSON(`${REF}/runtime-state/cases.json`, {
  subscriptionId: RS_SUB_ID,
  targetAddress: RS_ADDRESS,
  note: "Inputs of runtime-state/<name>.json; config built by tools/golden/status.mjs via loadConfig.",
  cases: CASES,
});

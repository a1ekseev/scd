import type {
  ResourceKind,
  StatusSnapshotTunnel,
  TargetBalancerMonitorState,
  TargetTopology,
  TunnelMonitorState,
  TunnelRuntimeState,
  TunnelSpeedtestState,
} from '../types.ts';

export interface ManagedResourceState {
  manifestHash: string;
}

export interface TargetApplyState {
  resources: Partial<Record<ResourceKind, ManagedResourceState>>;
  topologyGeneration: number;
  topology?: TargetTopology;
  tunnels: Record<string, TunnelRuntimeState>;
  balancerMonitor: TargetBalancerMonitorState;
}

export interface SyncMemoryState {
  syncInProgress: boolean;
  targets: Record<string, TargetApplyState>;
  targetLocks: Record<string, Promise<void>>;
}

function createIdleMonitorState(): TunnelMonitorState {
  return {
    state: 'idle',
    consecutiveFailures: 0,
  };
}

function createEmptySpeedtestState(): TunnelSpeedtestState {
  return {};
}

function createIdleBalancerMonitorState(): TargetBalancerMonitorState {
  return {
    state: 'idle',
    consecutiveFailures: 0,
  };
}

export function createSyncMemoryState(): SyncMemoryState {
  return {
    syncInProgress: false,
    targets: {},
    targetLocks: {},
  };
}

export function buildTargetStateKey(subscriptionId: string, targetAddress: string): string {
  return `${subscriptionId}::${targetAddress}`;
}

export function getOrCreateTargetState(memoryState: SyncMemoryState, targetKey: string): TargetApplyState {
  return (memoryState.targets[targetKey] ??= {
    resources: {},
    topologyGeneration: 0,
    tunnels: {},
    balancerMonitor: createIdleBalancerMonitorState(),
  });
}

export function replaceTargetTopology(
  targetState: TargetApplyState,
  topology: TargetTopology,
  resourceHashes: Partial<Record<ResourceKind, string>>,
): void {
  targetState.topologyGeneration += 1;
  targetState.topology = topology;
  targetState.resources = Object.fromEntries(
    Object.entries(resourceHashes).map(([kind, manifestHash]) => [kind, { manifestHash }]),
  ) as Partial<Record<ResourceKind, ManagedResourceState>>;
  targetState.tunnels = Object.fromEntries(
    topology.tunnels.map((tunnel) => [
      tunnel.baseTunnelId,
      {
        tunnel,
        monitor: createIdleMonitorState(),
        speedtest: createEmptySpeedtestState(),
      },
    ]),
  );
}

export function setTargetBalancerMonitorState(
  targetState: TargetApplyState,
  updater: (current: TargetBalancerMonitorState) => TargetBalancerMonitorState,
): void {
  targetState.balancerMonitor = updater(targetState.balancerMonitor);
}

export function setTunnelMonitorState(
  targetState: TargetApplyState,
  tunnelId: string,
  updater: (current: TunnelMonitorState) => TunnelMonitorState,
): void {
  const current = targetState.tunnels[tunnelId];
  if (!current) {
    return;
  }

  current.monitor = updater(current.monitor);
}

export function setTunnelSpeedtestState(
  targetState: TargetApplyState,
  tunnelId: string,
  updater: (current: TunnelSpeedtestState) => TunnelSpeedtestState,
): void {
  const current = targetState.tunnels[tunnelId];
  if (!current) {
    return;
  }

  current.speedtest = updater(current.speedtest);
}

export async function withTargetMutationLock<T>(
  memoryState: SyncMemoryState,
  targetKey: string,
  fn: () => Promise<T>,
): Promise<T> {
  const previous = memoryState.targetLocks[targetKey] ?? Promise.resolve();
  let release!: () => void;
  const current = new Promise<void>((resolve) => {
    release = resolve;
  });
  const queued = previous.catch(() => undefined).then(() => current);
  memoryState.targetLocks[targetKey] = queued;

  await previous.catch(() => undefined);
  try {
    return await fn();
  } finally {
    release();
    if (memoryState.targetLocks[targetKey] === queued) {
      delete memoryState.targetLocks[targetKey];
    }
  }
}

export function buildStatusSnapshot(memoryState: SyncMemoryState): StatusSnapshotTunnel[] {
  const tunnels: StatusSnapshotTunnel[] = [];

  for (const [targetKey, targetState] of Object.entries(memoryState.targets)) {
    const separator = targetKey.indexOf('::');
    const subscriptionId = separator >= 0 ? targetKey.slice(0, separator) : targetKey;
    const targetAddress = separator >= 0 ? targetKey.slice(separator + 2) : targetKey;

    for (const runtimeState of Object.values(targetState.tunnels)) {
      tunnels.push({
        subscriptionId,
        targetAddress,
        displayName: runtimeState.tunnel.displayName,
        countryIso2: runtimeState.tunnel.countryIso2,
        endpoint: `${runtimeState.tunnel.listen}:${runtimeState.tunnel.port}`,
        state: runtimeState.monitor.state,
        lastHttpStatus: runtimeState.monitor.lastStatusCode,
        lastLatencyMs: runtimeState.monitor.lastLatencyMs,
        lastBitsPerSecond: runtimeState.speedtest.lastBitsPerSecond,
        balancerMonitorState: targetState.balancerMonitor.state,
        balancerMonitorLastStatusCode: targetState.balancerMonitor.lastStatusCode,
        balancerMonitorLastLatencyMs: targetState.balancerMonitor.lastLatencyMs,
        balancerMonitorSuccessGetLastStatusCode: targetState.balancerMonitor.successGetLastStatusCode,
      });
    }
  }

  return tunnels.sort((left, right) => {
    return (
      left.subscriptionId.localeCompare(right.subscriptionId) ||
      left.targetAddress.localeCompare(right.targetAddress) ||
      left.displayName.localeCompare(right.displayName)
    );
  });
}

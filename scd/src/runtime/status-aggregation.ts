import type { Logger } from '../logging/create-logger.ts';
import type {
  AggregatedInstanceStatus,
  AggregatedStatusSnapshot,
  InstanceStatusSnapshot,
  RemoteStatusInstanceConfig,
} from '../types.ts';

export interface StatusAggregationState {
  local?: InstanceStatusSnapshot;
  remotes: Record<string, AggregatedInstanceStatus>;
}

interface RemoteStatusDependencies {
  fetchFn?: typeof fetch;
  nowFn?: () => Date;
}

export function createStatusAggregationState(
  remoteInstances: RemoteStatusInstanceConfig[] = [],
): StatusAggregationState {
  return {
    remotes: Object.fromEntries(remoteInstances.map((remote) => [
      remote.id,
      {
        id: remote.id,
        source: 'remote' as const,
        apiUrl: remote.apiUrl,
      },
    ])),
  };
}

export function storeLocalStatusSnapshot(
  state: StatusAggregationState,
  snapshot: InstanceStatusSnapshot,
): void {
  state.local = snapshot;
}

function parseRemoteSnapshot(value: unknown): InstanceStatusSnapshot {
  if (!value || typeof value !== 'object') {
    throw new Error('Remote status response must be a JSON object.');
  }

  const candidate = value as Partial<InstanceStatusSnapshot>;
  if (
    candidate.version !== 1 ||
    typeof candidate.instanceId !== 'string' ||
    typeof candidate.capturedAt !== 'string' ||
    !Array.isArray(candidate.subscriptions) ||
    !Array.isArray(candidate.tunnels)
  ) {
    throw new Error('Remote status response has an unsupported schema.');
  }

  const validStates = new Set(['healthy', 'degraded', 'repairing', 'idle']);
  const isValidTunnel = (tunnel: unknown): boolean => {
    if (!tunnel || typeof tunnel !== 'object') {
      return false;
    }
    const tunnelItem = tunnel as Record<string, unknown>;
    return (
      typeof tunnelItem.subscriptionId === 'string' &&
      typeof tunnelItem.targetAddress === 'string' &&
      typeof tunnelItem.displayName === 'string' &&
      typeof tunnelItem.endpoint === 'string' &&
      typeof tunnelItem.state === 'string' &&
      validStates.has(tunnelItem.state)
    );
  };
  const hasValidSubscriptions = candidate.subscriptions.every((subscription) => {
    if (!subscription || typeof subscription !== 'object') {
      return false;
    }
    const item = subscription as { subscriptionId?: unknown; targets?: unknown };
    if (typeof item.subscriptionId !== 'string' || !Array.isArray(item.targets)) {
      return false;
    }
    return item.targets.every((target) => {
      if (!target || typeof target !== 'object') {
        return false;
      }
      const targetItem = target as { targetAddress?: unknown; tunnels?: unknown };
      if (typeof targetItem.targetAddress !== 'string' || !Array.isArray(targetItem.tunnels)) {
        return false;
      }
      return targetItem.tunnels.every(isValidTunnel);
    });
  });

  if (!hasValidSubscriptions || !candidate.tunnels.every(isValidTunnel)) {
    throw new Error('Remote status response has an unsupported schema.');
  }

  return candidate as InstanceStatusSnapshot;
}

export async function collectRemoteStatus(
  remote: RemoteStatusInstanceConfig,
  state: StatusAggregationState,
  logger: Pick<Logger, 'info' | 'warn'>,
  dependencies: RemoteStatusDependencies = {},
): Promise<void> {
  const fetchFn = dependencies.fetchFn ?? fetch;
  const nowFn = dependencies.nowFn ?? (() => new Date());
  const attemptedAt = nowFn().toISOString();
  const current = state.remotes[remote.id] ?? {
    id: remote.id,
    source: 'remote' as const,
    apiUrl: remote.apiUrl,
  };
  current.lastAttemptAt = attemptedAt;
  state.remotes[remote.id] = current;

  try {
    const response = await fetchFn(remote.apiUrl, {
      headers: { accept: 'application/json' },
      signal: AbortSignal.timeout(remote.timeoutMs),
    });
    if (!response.ok) {
      throw new Error(`Remote status API returned HTTP ${response.status}.`);
    }

    current.snapshot = parseRemoteSnapshot(await response.json());
    current.lastSuccessAt = nowFn().toISOString();
    delete current.lastError;
    logger.info(
      { event: 'remote_status_collected', remoteInstanceId: remote.id, apiUrl: remote.apiUrl },
      'Remote instance status collected.',
    );
  } catch (error) {
    current.lastError = error instanceof Error ? error.message : String(error);
    logger.warn(
      {
        event: 'remote_status_collection_failed',
        remoteInstanceId: remote.id,
        apiUrl: remote.apiUrl,
        error: current.lastError,
      },
      'Remote instance status collection failed; keeping the last successful snapshot.',
    );
  }
}

export function buildAggregatedStatusSnapshot(state: StatusAggregationState): AggregatedStatusSnapshot {
  const instances: AggregatedInstanceStatus[] = [];
  if (state.local) {
    instances.push({
      id: state.local.instanceId,
      source: 'local',
      lastSuccessAt: state.local.capturedAt,
      snapshot: state.local,
    });
  }
  instances.push(...Object.values(state.remotes).sort((left, right) => left.id.localeCompare(right.id)));

  return {
    capturedAt: new Date().toISOString(),
    instances,
  };
}

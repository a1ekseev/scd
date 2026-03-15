import type { ResourceKind } from '../types.ts';

export interface ManagedResourceState {
  manifestHash: string;
}

export interface TargetApplyState {
  resources: Partial<Record<ResourceKind, ManagedResourceState>>;
}

export interface SyncMemoryState {
  syncInProgress: boolean;
  targets: Record<string, TargetApplyState>;
}

export function createSyncMemoryState(): SyncMemoryState {
  return {
    syncInProgress: false,
    targets: {},
  };
}

export function buildTargetStateKey(subscriptionId: string, targetAddress: string): string {
  return `${subscriptionId}::${targetAddress}`;
}

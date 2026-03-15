import type { ApplyReport, AppConfig, ResourceKind, SkippedEntry, SubscriptionTargetConfig } from '../types.ts';
import type { LoadedSubscription } from '../runtime/generate-manifest-from-source.ts';

export interface ResourcePlan {
  kind: ResourceKind;
  sourceId: string;
  skipped: SkippedEntry[];
  manifestHash: string;
  managedIds: string[];
}

export interface ResourceApplyContext {
  subscriptionId: string;
  target: SubscriptionTargetConfig;
}

export interface ResourceApplicator<TPlan extends ResourcePlan = ResourcePlan> {
  kind: ResourceKind;
  isEnabled(config: AppConfig): boolean;
  buildPlan(subscription: LoadedSubscription): TPlan;
  preparePlanForTarget(plan: TPlan, target: SubscriptionTargetConfig): TPlan;
  applyPlan(plan: TPlan, context: ResourceApplyContext): Promise<ApplyReport>;
}

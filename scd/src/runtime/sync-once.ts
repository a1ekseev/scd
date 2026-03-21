import { outboundApplicator } from '../apply/outbound-applicator.ts';
import { inboundApplicator } from '../apply/inbound-applicator.ts';
import { routingApplicator } from '../apply/routing-applicator.ts';
import type { ResourceApplicator, ResourcePlan } from '../apply/resource-applicator.ts';
import { loadConfig } from '../config/load-config.ts';
import { createLogger, type Logger } from '../logging/create-logger.ts';
import type {
  ApplyReport,
  LoadedConfig,
  ResourceKind,
  TargetTopology,
  SubscriptionTargetConfig,
  SyncReport,
  TargetSyncReport,
} from '../types.ts';
import { loadSubscriptions, type FailedSubscriptionLoad, type LoadedSubscription, type LoadSubscriptionsResult } from './generate-manifest-from-source.ts';
import {
  buildTargetStateKey,
  createSyncMemoryState,
  getOrCreateTargetState,
  replaceTargetTopology,
  withTargetMutationLock,
  type SyncMemoryState,
} from './run-state.ts';

interface SyncServices {
  loadSubscriptionsFn: (sources: LoadedConfig['config']['subscriptions']) => Promise<LoadSubscriptionsResult>;
  applicators: ResourceApplicator[];
}

interface BuiltResourcePlan {
  applicator: ResourceApplicator;
  plan: ResourcePlan;
}

function hasFilteredManifestSummary(
  plan: ResourcePlan,
): plan is ResourcePlan & { manifest: { summary: { filtered: number; filteredByCountry: number; filteredByLabelRegex: number } } } {
  if (!('manifest' in plan) || typeof plan.manifest !== 'object' || plan.manifest === null) {
    return false;
  }

  const summary = (plan.manifest as { summary?: unknown }).summary;
  if (!summary || typeof summary !== 'object') {
    return false;
  }

  return (
    'filtered' in summary &&
    'filteredByCountry' in summary &&
    'filteredByLabelRegex' in summary
  );
}

const defaultSyncServices: SyncServices = {
  loadSubscriptionsFn: loadSubscriptions,
  applicators: [outboundApplicator, inboundApplicator, routingApplicator],
};

function buildReportTimestamp(): string {
  return new Date().toISOString();
}

function createEmptySyncReport(): SyncReport {
  return {
    appliedAt: buildReportTimestamp(),
    durationMs: 0,
    added: 0,
    replaced: 0,
    removed: 0,
    unchanged: 0,
    failed: 0,
    targets: [],
    skipped: [],
  };
}

function createFailedApplyReport(
  kind: ResourceKind,
  subscriptionId: string,
  targetAddress: string,
  sourceId: string,
  skipped: ApplyReport['skipped'],
  message: string,
): ApplyReport {
  return {
    kind,
    sourceId,
    subscriptionId,
    targetAddress,
    appliedAt: buildReportTimestamp(),
    durationMs: 0,
    added: 0,
    replaced: 0,
    removed: 0,
    failed: 1,
    deletedIds: [],
    appliedIds: [],
    items: [
      {
        id: kind,
        status: 'failed',
        message,
      },
    ],
    skipped,
  };
}

function buildTargetSyncReport(
  subscriptionId: string,
  targetAddress: string,
  sourceId: string,
  unchangedKinds: ResourceKind[],
  resources: ApplyReport[],
  skipped: ApplyReport['skipped'],
): TargetSyncReport {
  return {
    subscriptionId,
    targetAddress,
    sourceId,
    unchangedKinds,
    failed: resources.reduce((total, report) => total + report.failed, 0),
    resources,
    skipped,
  };
}

function buildFailedTargetSyncReport(
  subscriptionId: string,
  targetAddress: string,
  sourceId: string,
  resources: ApplyReport[],
): TargetSyncReport {
  return {
    subscriptionId,
    targetAddress,
    sourceId,
    unchangedKinds: [],
    failed: resources.reduce((total, report) => total + report.failed, 0),
    resources,
    skipped: [],
  };
}

function aggregateReports(reports: TargetSyncReport[], skipped: ApplyReport['skipped'], startedAt: number): SyncReport {
  const allResourceReports = reports.flatMap((target) => target.resources);
  return {
    appliedAt: buildReportTimestamp(),
    durationMs: Date.now() - startedAt,
    added: allResourceReports.reduce((total, report) => total + report.added, 0),
    replaced: allResourceReports.reduce((total, report) => total + report.replaced, 0),
    removed: allResourceReports.reduce((total, report) => total + report.removed, 0),
    unchanged: reports.reduce((total, target) => total + target.unchangedKinds.length, 0),
    failed: allResourceReports.reduce((total, report) => total + report.failed, 0),
    targets: reports,
    skipped,
  };
}

function buildPlans(subscription: LoadedSubscription, services: SyncServices, loadedConfig: LoadedConfig): BuiltResourcePlan[] {
  return services.applicators
    .filter((applicator) => applicator.isEnabled(loadedConfig.config))
    .map((applicator) => ({
      applicator,
      plan: applicator.buildPlan(subscription),
    }));
}

function logApplyResult(logger: Logger, report: ApplyReport): void {
  const payload = {
    event: report.failed > 0 ? 'apply_failed' : 'apply_finished',
    kind: report.kind,
    subscriptionId: report.subscriptionId,
    targetAddress: report.targetAddress,
    sourceId: report.sourceId,
    added: report.added,
    replaced: report.replaced,
    removed: report.removed,
    failed: report.failed,
    deletedIds: report.deletedIds,
    appliedIds: report.appliedIds,
    durationMs: report.durationMs,
  };

  if (report.failed > 0) {
    logger.error(payload, 'Apply failed.');
    return;
  }

  logger.info(payload, 'Apply finished.');
}

async function syncSubscriptionTarget(
  subscription: LoadedSubscription,
  target: SubscriptionTargetConfig,
  builtPlans: BuiltResourcePlan[],
  memoryState: SyncMemoryState,
  logger: Logger,
): Promise<TargetSyncReport> {
  const targetKey = buildTargetStateKey(subscription.id, target.address);
  const targetState = getOrCreateTargetState(memoryState, targetKey);
  const targetPlans = builtPlans.map(({ applicator, plan }) => ({
    applicator,
    plan: applicator.preparePlanForTarget(plan, target),
  }));
  const changedPlans = targetPlans.filter(({ plan }) => {
    const previous = targetState.resources[plan.kind];
    return !previous || previous.manifestHash !== plan.manifestHash;
  });

  if (changedPlans.length === 0) {
    const unchangedKinds = targetPlans.map(({ plan }) => plan.kind);
    for (const kind of unchangedKinds) {
      logger.info(
        {
          event: 'skipped_no_changes',
          kind,
          subscriptionId: subscription.id,
          targetAddress: target.address,
          observatorySubjectSelectorPrefix: target.observatorySubjectSelectorPrefix,
        },
        'Manifest unchanged for resource; skipping Xray API.',
      );
    }

    return buildTargetSyncReport(
      subscription.id,
      target.address,
      builtPlans[0]?.plan.sourceId ?? subscription.id,
      unchangedKinds,
      [],
      builtPlans.flatMap((item) => item.plan.skipped),
    );
  }

  return withTargetMutationLock(memoryState, targetKey, async () => {
    const resourceReports: ApplyReport[] = [];
    const resourceHashes: Partial<Record<ResourceKind, string>> = {};
    let nextTopology: TargetTopology | undefined;
    let failed = false;

    for (const { applicator, plan } of targetPlans) {
      try {
        logger.info(
          {
            event: 'resource_apply_started',
            kind: plan.kind,
            subscriptionId: subscription.id,
            targetAddress: target.address,
            observatorySubjectSelectorPrefix: target.observatorySubjectSelectorPrefix,
          },
          'Applying resource to target.',
        );

        const report = await applicator.applyPlan(plan, {
          subscriptionId: subscription.id,
          target,
        });
        resourceReports.push(report);
        logApplyResult(logger, report);

        if (report.failed > 0) {
          failed = true;
        } else {
          resourceHashes[plan.kind] = plan.manifestHash;
          if ('topology' in plan && typeof plan === 'object' && plan.topology) {
            nextTopology = plan.topology as TargetTopology;
          }
        }
      } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        const report = createFailedApplyReport(
          plan.kind,
          subscription.id,
          target.address,
          plan.sourceId,
          plan.skipped,
          message,
        );
        resourceReports.push(report);
        logApplyResult(logger, report);
        failed = true;
      }
    }

    if (!failed && nextTopology) {
      replaceTargetTopology(targetState, nextTopology, resourceHashes);
    }

    return buildTargetSyncReport(
      subscription.id,
      target.address,
      builtPlans[0]?.plan.sourceId ?? subscription.id,
      [],
      resourceReports,
      builtPlans.flatMap((item) => item.plan.skipped),
    );
  });
}

function buildManifestFailureTargetReports(
  subscriptionId: string,
  sourceId: string,
  targets: SubscriptionTargetConfig[],
  services: SyncServices,
  loadedConfig: LoadedConfig,
  logger: Logger,
  errorMessage: string,
): TargetSyncReport[] {
  const applicators = services.applicators.filter((applicator) => applicator.isEnabled(loadedConfig.config));
  const reports: TargetSyncReport[] = [];

  logger.error(
    {
      event: 'manifest_validation_failed',
      subscriptionId,
      sourceId,
      error: errorMessage,
      targets: targets.map((target) => target.address),
    },
    'Manifest validation failed.',
  );

  for (const target of targets) {
    const resourceReports = applicators.map((applicator) =>
      createFailedApplyReport(applicator.kind, subscriptionId, target.address, sourceId, [], errorMessage),
    );

    for (const report of resourceReports) {
      logApplyResult(logger, report);
    }

    reports.push(buildFailedTargetSyncReport(subscriptionId, target.address, sourceId, resourceReports));
  }

  return reports;
}

function buildFailedSubscriptionTargetReports(
  failedSubscription: FailedSubscriptionLoad,
  services: SyncServices,
  loadedConfig: LoadedConfig,
  logger: Logger,
): TargetSyncReport[] {
  const applicators = services.applicators.filter((applicator) => applicator.isEnabled(loadedConfig.config));
  const reports: TargetSyncReport[] = [];

  logger.error(
    {
      event: 'subscription_fetch_failed',
      subscriptionId: failedSubscription.id,
      source: failedSubscription.source,
      error: failedSubscription.error,
      targets: failedSubscription.targets.map((target) => target.address),
    },
    'Subscription fetch failed.',
  );

  for (const target of failedSubscription.targets) {
    const resourceReports = applicators.map((applicator) =>
      createFailedApplyReport(
        applicator.kind,
        failedSubscription.id,
        target.address,
        failedSubscription.id,
        [],
        failedSubscription.error,
      ),
    );

    for (const report of resourceReports) {
      logApplyResult(logger, report);
    }

    reports.push(buildFailedTargetSyncReport(failedSubscription.id, target.address, failedSubscription.id, resourceReports));
  }

  return reports;
}

async function performSync(
  loadedConfig: LoadedConfig,
  logger: Logger,
  memoryState: SyncMemoryState,
  services: SyncServices,
): Promise<SyncReport> {
  const startedAt = Date.now();
  logger.info({ event: 'sync_started', sourceCount: loadedConfig.config.subscriptions.length }, 'Sync started.');

  const subscriptionResult = await services.loadSubscriptionsFn(loadedConfig.config.subscriptions);
  logger.info(
    {
      event: 'subscription_fetched',
      subscriptions: subscriptionResult.loaded.map((subscription) => ({
        id: subscription.id,
        source: subscription.source,
        encoding: subscription.encoding,
        targets: subscription.targets.map((target) => target.address),
      })),
    },
    'Subscriptions loaded.',
  );

  const targetReports: TargetSyncReport[] = [];
  const skippedEntries: ApplyReport['skipped'] = [];

  for (const failedSubscription of subscriptionResult.failed) {
    targetReports.push(...buildFailedSubscriptionTargetReports(failedSubscription, services, loadedConfig, logger));
  }

  for (const subscription of subscriptionResult.loaded) {
    let builtPlans: BuiltResourcePlan[];
    try {
      builtPlans = buildPlans(subscription, services, loadedConfig);
    } catch (error) {
      targetReports.push(
        ...buildManifestFailureTargetReports(
          subscription.id,
          subscription.id,
          subscription.targets,
          services,
          loadedConfig,
          logger,
          error instanceof Error ? error.message : String(error),
        ),
      );
      continue;
    }

    skippedEntries.push(...builtPlans.flatMap((item) => item.plan.skipped));

    for (const { plan } of builtPlans) {
      const filteredSummary =
        hasFilteredManifestSummary(plan)
          ? {
              filtered: plan.manifest.summary.filtered,
              filteredByCountry: plan.manifest.summary.filteredByCountry,
              filteredByLabelRegex: plan.manifest.summary.filteredByLabelRegex,
            }
          : {};

      logger.info(
        {
          event: 'manifest_built',
          kind: plan.kind,
          subscriptionId: subscription.id,
          sourceId: plan.sourceId,
          managedCount: plan.managedIds.length,
          skipped: plan.skipped.length,
          ...filteredSummary,
          targetCount: subscription.targets.length,
        },
        'Manifest built.',
      );
    }

    for (const target of subscription.targets) {
      targetReports.push(await syncSubscriptionTarget(subscription, target, builtPlans, memoryState, logger));
    }
  }

  return aggregateReports(targetReports, skippedEntries, startedAt);
}

export async function syncWithConfig(
  loadedConfig: LoadedConfig,
  logger: Logger,
  memoryState: SyncMemoryState = createSyncMemoryState(),
  services: SyncServices = defaultSyncServices,
): Promise<SyncReport> {
  if (memoryState.syncInProgress) {
    logger.warn({ event: 'sync_skipped_overlap' }, 'Sync is already running in this process; skipping overlap run.');
    return createEmptySyncReport();
  }

  memoryState.syncInProgress = true;
  try {
    return await performSync(loadedConfig, logger, memoryState, services);
  } catch (error) {
    const message = error instanceof Error ? error.message : String(error);
    logger.error({ event: 'sync_failed', error: message }, 'Sync failed.');
    throw error;
  } finally {
    memoryState.syncInProgress = false;
  }
}

export async function syncOnce(configPath: string): Promise<SyncReport> {
  const loadedConfig = await loadConfig(configPath);
  const logger = createLogger(loadedConfig.config.logging);
  logger.info({ event: 'config_loaded', configPath: loadedConfig.configPath }, 'Config loaded.');
  return syncWithConfig(loadedConfig, logger, createSyncMemoryState());
}

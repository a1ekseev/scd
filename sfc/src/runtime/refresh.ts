import { Buffer } from 'node:buffer';

import { parseRegexLiteral } from '../config/load-config.ts';
import { loadInputSource } from '../input/load-input.ts';
import type {
  AppState,
  LoadedConfig,
  OutputConfig,
  OutputRuntimeState,
  ParsedSubscriptionEntry,
  RefreshOutputReport,
  RefreshReport,
  SubscriptionConfig,
} from '../types.ts';
import { createLogger, type Logger } from '../logging/create-logger.ts';
import { parseSubscriptionLine } from '../subscription/parse-subscription-line.ts';
import { scanLines } from '../subscription/scan-lines.ts';
import { buildOutputPath } from './output-path.ts';

function now(): string {
  return new Date().toISOString();
}

function createOutputState(subscription: SubscriptionConfig, output: OutputConfig): OutputRuntimeState {
  return {
    id: output.id,
    subscriptionId: subscription.id,
    pathRoute: subscription.pathRoute,
    name: output.name,
    regex: output.labelIncludeRegex,
    userAgent: output.userAgent,
    profileTitle: output.profileTitle,
    profileUpdateInterval: output.profileUpdateInterval,
  };
}

function setOutputState(state: AppState, outputState: OutputRuntimeState): void {
  state.outputs[outputState.id] = outputState;
  state.outputsByPath[buildOutputPath(outputState.pathRoute, outputState.id)] = outputState;
}

export function createAppState(config: LoadedConfig['config']): AppState {
  const outputs: Record<string, OutputRuntimeState> = {};
  const outputsByPath: Record<string, OutputRuntimeState> = {};

  for (const subscription of config.subscriptions) {
    for (const output of subscription.outputs) {
      const outputState = createOutputState(subscription, output);
      outputs[output.id] = outputState;
      outputsByPath[buildOutputPath(subscription.pathRoute, output.id)] = outputState;
    }
  }

  return {
    outputs,
    outputsByPath,
    refreshInProgress: false,
  };
}

function parseSupportedEntries(content: string): ParsedSubscriptionEntry[] {
  const entries: ParsedSubscriptionEntry[] = [];
  for (const line of scanLines(content)) {
    const parsed = parseSubscriptionLine(line);
    if (parsed) {
      entries.push(parsed);
    }
  }
  return entries;
}

function updateOutputFailure(
  state: AppState,
  subscription: SubscriptionConfig,
  output: OutputConfig,
  error: string,
): RefreshOutputReport {
  const current = state.outputs[output.id] ?? createOutputState(subscription, output);
  const nextState = {
    ...current,
    pathRoute: subscription.pathRoute,
    regex: output.labelIncludeRegex,
    name: output.name,
    userAgent: output.userAgent,
    profileTitle: output.profileTitle,
    profileUpdateInterval: output.profileUpdateInterval,
    lastRefreshAt: now(),
    lastFailureAt: now(),
    lastError: error,
  };
  setOutputState(state, nextState);

  return {
    id: output.id,
    subscriptionId: subscription.id,
    name: output.name,
    matchedLines: 0,
    ok: false,
    usedCachedValue: Boolean(current.lastGoodBase64),
    error,
  };
}

function updateOutputSuccess(
  state: AppState,
  subscription: SubscriptionConfig,
  output: OutputConfig,
  matched: ParsedSubscriptionEntry[],
): RefreshOutputReport {
  const plain = matched.map((entry) => entry.raw).join('\n');
  const base64 = Buffer.from(plain, 'utf8').toString('base64');
  const timestamp = now();
  const nextState = {
    ...(state.outputs[output.id] ?? createOutputState(subscription, output)),
    pathRoute: subscription.pathRoute,
    name: output.name,
    regex: output.labelIncludeRegex,
    userAgent: output.userAgent,
    profileTitle: output.profileTitle,
    profileUpdateInterval: output.profileUpdateInterval,
    lastGoodPlain: plain,
    lastGoodBase64: base64,
    lastGoodLineCount: matched.length,
    lastRefreshAt: timestamp,
    lastSuccessAt: timestamp,
    lastError: undefined,
  };
  setOutputState(state, nextState);

  return {
    id: output.id,
    subscriptionId: subscription.id,
    name: output.name,
    matchedLines: matched.length,
    ok: true,
    usedCachedValue: false,
  };
}

function buildOutputReports(
  state: AppState,
  subscription: SubscriptionConfig,
  parsedEntries: ParsedSubscriptionEntry[],
): RefreshOutputReport[] {
  const reports: RefreshOutputReport[] = [];

  for (const output of subscription.outputs.filter((item) => item.enabled)) {
    const regex = parseRegexLiteral(output.labelIncludeRegex);
    const matched = parsedEntries.filter((entry) => {
      regex.lastIndex = 0;
      return regex.test(entry.label);
    });

    if (matched.length === 0) {
      reports.push(updateOutputFailure(state, subscription, output, 'Filtered output is empty.'));
      continue;
    }

    reports.push(updateOutputSuccess(state, subscription, output, matched));
  }

  return reports;
}

export async function refreshWithConfig(
  loadedConfig: LoadedConfig,
  state: AppState,
  logger: Logger = createLogger(loadedConfig.config.logging),
): Promise<RefreshReport> {
  const reports: RefreshOutputReport[] = [];
  state.refreshInProgress = true;
  state.lastRefreshStartedAt = now();

  try {
    for (const subscription of loadedConfig.config.subscriptions.filter((item) => item.enabled)) {
      try {
        const input = await loadInputSource(subscription.input, {
          format: subscription.format,
          fetchTimeoutMs: subscription.fetchTimeoutMs,
        });
        const parsedEntries = parseSupportedEntries(input.content.trim());
        reports.push(...buildOutputReports(state, subscription, parsedEntries));
        logger.info(
          {
            event: 'subscription_refreshed',
            subscriptionId: subscription.id,
            source: input.source,
            outputs: subscription.outputs.filter((item) => item.enabled).map((item) => item.id),
            parsedEntries: parsedEntries.length,
          },
          'Subscription refreshed.',
        );
      } catch (error) {
        const message = error instanceof Error ? error.message : String(error);
        logger.error(
          {
            event: 'subscription_refresh_failed',
            subscriptionId: subscription.id,
            error: message,
          },
          'Subscription refresh failed.',
        );
        for (const output of subscription.outputs.filter((item) => item.enabled)) {
          reports.push(updateOutputFailure(state, subscription, output, message));
        }
      }
    }
  } finally {
    state.refreshInProgress = false;
    state.lastRefreshFinishedAt = now();
  }

  return {
    refreshedAt: state.lastRefreshFinishedAt!,
    sourceCount: loadedConfig.config.subscriptions.filter((item) => item.enabled).length,
    outputCount: reports.length,
    successful: reports.filter((item) => item.ok).length,
    failed: reports.filter((item) => !item.ok).length,
    outputs: reports.sort((left, right) => left.id.localeCompare(right.id)),
  };
}

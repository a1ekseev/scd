import assert from 'node:assert/strict';
import test from 'node:test';

import { getErrorLogDetails, runCronLoop, runLoggedCronLoop } from '../src/runtime/run-server.ts';

type CapturedLog = {
  object: Record<string, unknown>;
  message: string;
};

function captureLog(logs: CapturedLog[], ...args: unknown[]): void {
  const [object, message] = args;
  logs.push({
    object: (object ?? {}) as Record<string, unknown>,
    message: String(message ?? ''),
  });
}

test('runCronLoop skips overrunning refresh ticks without drifting future cron slots', async () => {
  let nowMs = 0;
  let stopping = false;
  const startedAt: number[] = [];
  let releaseFirstRun!: () => void;
  const firstRunDone = new Promise<void>((resolve) => {
    releaseFirstRun = resolve;
  });
  const overruns: string[] = [];

  await runCronLoop(
    'unused',
    () => stopping,
    async () => {
      startedAt.push(nowMs);
      if (startedAt.length === 1) {
        await firstRunDone;
      }
    },
    {
      nowFn: () => new Date(nowMs),
      waitFn: async (ms) => {
        nowMs += ms;
        if (nowMs >= 300) {
          stopping = true;
        }
        await Promise.resolve();
      },
      getNextDateFn: (_schedule, currentDate) => new Date(currentDate.getTime() + 100),
      onOverrun: (scheduledAt) => {
        overruns.push(scheduledAt.toISOString());
        if (overruns.length === 1) {
          releaseFirstRun();
        }
      },
    },
  );

  assert.deepEqual(startedAt, [0, 200]);
  assert.deepEqual(overruns, [new Date(100).toISOString()]);
});

test('getErrorLogDetails preserves aggregate causes', () => {
  const details = getErrorLogDetails(
    new AggregateError([new Error('first cause'), new Error('second cause')], 'top-level'),
  );

  assert.equal(details.message, 'top-level');
  assert.deepEqual(details.causes, ['first cause', 'second cause']);
});

test('runLoggedCronLoop logs refresh overrun through logger path', async () => {
  let nowMs = 0;
  let stopping = false;
  let runCount = 0;
  let releaseFirstRun!: () => void;
  const firstRunDone = new Promise<void>((resolve) => {
    releaseFirstRun = resolve;
  });
  const warnings: CapturedLog[] = [];
  const errors: CapturedLog[] = [];

  await runLoggedCronLoop(
    'unused',
    () => stopping,
    async () => {
      runCount += 1;
      if (runCount === 1) {
        await firstRunDone;
      }
    },
    {
      warn(...args: unknown[]) {
        captureLog(warnings, ...args);
      },
      error(...args: unknown[]) {
        captureLog(errors, ...args);
      },
    },
    {
      overrunEvent: 'refresh_tick_skipped_overrun',
      overrunMessage: 'Refresh tick skipped because previous run is still in progress.',
      failureEvent: 'refresh_failed',
      failureMessage: 'Refresh tick failed.',
      runCronLoopOptions: {
        nowFn: () => new Date(nowMs),
        waitFn: async (ms) => {
          nowMs += ms;
          if (nowMs >= 300) {
            stopping = true;
          }
          await Promise.resolve();
        },
        getNextDateFn: (_schedule, currentDate) => new Date(currentDate.getTime() + 100),
        onOverrun: () => {
          releaseFirstRun();
        },
      },
    },
  );

  assert.equal(errors.length, 0);
  assert.ok(warnings.length >= 1);
  assert.equal(warnings[0]?.object.event, 'refresh_tick_skipped_overrun');
  assert.equal(warnings[0]?.message, 'Refresh tick skipped because previous run is still in progress.');
});

test('runLoggedCronLoop logs aggregate causes for refresh failure', async () => {
  let stopping = false;
  const warnings: CapturedLog[] = [];
  const errors: CapturedLog[] = [];

  await runLoggedCronLoop(
    'unused',
    () => stopping,
    async () => {
      stopping = true;
      throw new AggregateError([new Error('fetch failed'), new Error('decode failed')], 'refresh exploded');
    },
    {
      warn(...args: unknown[]) {
        captureLog(warnings, ...args);
      },
      error(...args: unknown[]) {
        captureLog(errors, ...args);
      },
    },
    {
      overrunEvent: 'refresh_tick_skipped_overrun',
      overrunMessage: 'Refresh tick skipped because previous run is still in progress.',
      failureEvent: 'refresh_failed',
      failureMessage: 'Refresh tick failed.',
      runCronLoopOptions: {
        getNextDateFn: (_schedule, currentDate) => new Date(currentDate.getTime() + 100),
      },
    },
  );

  assert.equal(warnings.length, 0);
  assert.equal(errors.length, 1);
  assert.equal(errors[0]?.object.event, 'refresh_failed');
  assert.deepEqual(errors[0]?.object.causes, ['fetch failed', 'decode failed']);
  assert.equal(errors[0]?.message, 'Refresh tick failed.');
});

import assert from "node:assert/strict";
import test from "node:test";

import { createLatestRequest, createPoller } from "./async.js";

test("latest request invalidates older generations", () => {
  const requests = createLatestRequest();
  const first = requests.next();
  const second = requests.next();

  assert.equal(first(), false);
  assert.equal(second(), true);

  requests.invalidate();
  assert.equal(second(), false);
});

test("abortable latest requests abort and ignore superseded responses", async () => {
  const requests = createLatestRequest();
  const applied = [];
  let resolveFirst;
  let resolveSecond;
  let firstSignal;
  let secondSignal;

  async function run(response, captureSignal) {
    const { isLatest, signal } = requests.nextAbortable();
    captureSignal(signal);
    const value = await response;
    if (isLatest()) applied.push(value);
  }

  const firstResponse = new Promise((resolve) => (resolveFirst = resolve));
  const firstRun = run(firstResponse, (signal) => (firstSignal = signal));
  const secondResponse = new Promise((resolve) => (resolveSecond = resolve));
  const secondRun = run(secondResponse, (signal) => (secondSignal = signal));

  assert.equal(firstSignal.aborted, true);
  assert.equal(secondSignal.aborted, false);
  resolveSecond("B");
  await secondRun;
  resolveFirst("A");
  await firstRun;

  assert.deepEqual(applied, ["B"]);
  requests.invalidate();
  assert.equal(secondSignal.aborted, true);
});

test("poller runs a task single-flight", async () => {
  let calls = 0;
  let finish;
  const poller = createPoller(
    () =>
      new Promise((resolve) => {
        calls += 1;
        finish = resolve;
      }),
  );

  const first = poller.run();
  const second = poller.run();
  await Promise.resolve();

  assert.equal(first, second);
  assert.equal(calls, 1);
  finish();
  await first;
});

test("stopping a poller clears its scheduled task", async () => {
  let calls = 0;
  const poller = createPoller(() => {
    calls += 1;
  });

  poller.start(5);
  poller.stop();
  await new Promise((resolve) => setTimeout(resolve, 15));

  assert.equal(calls, 0);
});

test("stopping an in-flight poll prevents a late reschedule", async () => {
  let calls = 0;
  let finish;
  const poller = createPoller(
    () =>
      new Promise((resolve) => {
        calls += 1;
        finish = resolve;
      }),
  );

  poller.start(5, { immediate: true });
  await Promise.resolve();
  poller.stop();
  finish();
  await new Promise((resolve) => setTimeout(resolve, 15));

  assert.equal(calls, 1);
});

test("poller handles task errors", async () => {
  const failure = new Error("failed");
  let handled;
  const poller = createPoller(
    () => {
      throw failure;
    },
    { onError: (error) => (handled = error) },
  );

  await poller.run();
  assert.equal(handled, failure);
});

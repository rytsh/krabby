export function createLatestRequest() {
  let generation = 0;
  let controller;

  return {
    next() {
      const requestGeneration = ++generation;
      return () => requestGeneration === generation;
    },
    nextAbortable() {
      controller?.abort();
      controller = new AbortController();
      const requestGeneration = ++generation;
      return {
        signal: controller.signal,
        isLatest: () => requestGeneration === generation,
      };
    },
    invalidate() {
      generation += 1;
      controller?.abort();
      controller = undefined;
    },
  };
}

// Schedule the next poll only after the current task settles. run() is also
// single-flight, so manual and scheduled calls through the poller can share it.
export function createPoller(task, { onError = () => {} } = {}) {
  let active = false;
  let delay = 0;
  let timer;
  let inFlight = null;

  function schedule() {
    if (!active || delay <= 0 || timer !== undefined || inFlight) return;
    timer = setTimeout(() => {
      timer = undefined;
      void run();
    }, delay);
  }

  function run() {
    if (inFlight) return inFlight;
    if (timer !== undefined) {
      clearTimeout(timer);
      timer = undefined;
    }

    inFlight = Promise.resolve()
      .then(task)
      .catch((error) => {
        try {
          onError(error);
        } catch {
          // Polling errors must never become unhandled rejections.
        }
      })
      .finally(() => {
        inFlight = null;
        schedule();
      });
    return inFlight;
  }

  function stop() {
    active = false;
    if (timer !== undefined) {
      clearTimeout(timer);
      timer = undefined;
    }
  }

  function start(nextDelay, { immediate = false } = {}) {
    stop();
    delay = Number(nextDelay);
    if (!(delay > 0)) return;
    active = true;
    if (immediate) void run();
    else schedule();
  }

  return {
    run,
    start,
    stop,
    get running() {
      return inFlight !== null;
    },
  };
}

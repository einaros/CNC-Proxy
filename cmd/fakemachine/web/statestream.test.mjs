import test from "node:test";
import assert from "node:assert/strict";
import { readFile } from "node:fs/promises";

// app.js cannot be imported under node (it touches document/three.js at module
// scope), so extract the self-contained createStateStream factory from its
// source text and evaluate it. This tests the exact shipped code.
const appSource = await readFile(new URL("./app.js", import.meta.url), "utf8");

function extractFunction(src, name) {
  const marker = `function ${name}(`;
  const start = src.indexOf(marker);
  assert.notEqual(start, -1, `${name} not found in app.js`);
  const bodyStart = src.indexOf("{", start);
  let depth = 0;
  for (let i = bodyStart; i < src.length; i++) {
    if (src[i] === "{") depth++;
    else if (src[i] === "}") {
      depth--;
      if (depth === 0) return src.slice(start, i + 1);
    }
  }
  assert.fail(`unbalanced braces extracting ${name}`);
}

const createStateStream = new Function(
  `return (${extractFunction(appSource, "createStateStream")});`,
)();

function makeTimers() {
  let nextId = 1;
  const timeouts = new Map();
  const intervals = new Map();
  return {
    fns: {
      setTimeoutFn: (fn) => {
        const id = nextId++;
        timeouts.set(id, fn);
        return id;
      },
      clearTimeoutFn: (id) => {
        timeouts.delete(id);
      },
      setIntervalFn: (fn) => {
        const id = nextId++;
        intervals.set(id, fn);
        return id;
      },
      clearIntervalFn: (id) => {
        intervals.delete(id);
      },
    },
    // Fire every live interval once (one watchdog tick).
    tickIntervals: () => {
      for (const fn of [...intervals.values()]) fn();
    },
    // Fire timeouts that are currently due; callbacks may schedule new ones.
    runDueTimeouts: () => {
      const due = [...timeouts.entries()];
      for (const [id] of due) timeouts.delete(id);
      for (const [, fn] of due) fn();
    },
    pendingTimeouts: () => timeouts.size,
    activeIntervals: () => intervals.size,
  };
}

function makeEventSourceClass() {
  class FakeEventSource {
    static CONNECTING = 0;
    static OPEN = 1;
    static CLOSED = 2;
    static instances = [];
    constructor(url) {
      this.url = url;
      this.readyState = FakeEventSource.CONNECTING;
      this.listeners = new Map();
      this.onopen = null;
      this.onerror = null;
      FakeEventSource.instances.push(this);
    }
    addEventListener(type, fn) {
      const arr = this.listeners.get(type) ?? [];
      arr.push(fn);
      this.listeners.set(type, arr);
    }
    emit(type, event) {
      for (const fn of this.listeners.get(type) ?? []) fn(event);
    }
    open() {
      this.readyState = FakeEventSource.OPEN;
      if (this.onopen) this.onopen();
    }
  }
  return FakeEventSource;
}

const settle = () => new Promise((resolve) => setImmediate(resolve));

function makeHarness({ eventSource = true } = {}) {
  const timers = makeTimers();
  const ES = eventSource ? makeEventSourceClass() : null;
  const snapshots = [];
  let fetchCalls = 0;
  const stream = createStateStream({
    eventSourceCtor: ES,
    fetchFn: async () => {
      fetchCalls++;
      return { ok: true, json: async () => ({ state: "Idle" }) };
    },
    ...timers.fns,
    onSnapshot: (s) => snapshots.push(s),
    onConnectionError: () => {},
    eventsURL: "/api/events",
    stateURL: "/api/state",
    pollDelayMs: 250,
    fallbackCheckMs: 1200,
  });
  return {
    timers,
    ES,
    snapshots,
    stream,
    fetchCalls: () => fetchCalls,
  };
}

test("closed SSE with repeated fallback ticks starts exactly one poll chain", async () => {
  const h = makeHarness();
  h.stream.connect();
  assert.equal(h.ES.instances.length, 1);
  h.ES.instances[0].readyState = h.ES.CLOSED;

  // Three watchdog ticks while the EventSource is CLOSED. Pre-fix, each tick
  // spawned a fresh self-rescheduling poll chain.
  for (let i = 0; i < 3; i++) {
    h.timers.tickIntervals();
    await settle();
  }

  assert.equal(h.fetchCalls(), 1, "only one poll may start across ticks");
  assert.equal(h.timers.pendingTimeouts(), 1, "exactly one poll chain scheduled");
  assert.equal(h.timers.activeIntervals(), 1, "exactly one live watchdog interval");

  // Each subsequent poll round issues exactly one request.
  for (let round = 2; round <= 4; round++) {
    h.timers.runDueTimeouts();
    await settle();
    assert.equal(h.fetchCalls(), round, "poll cadence must stay single-flight");
    assert.equal(h.timers.pendingTimeouts(), 1);
  }
});

test("restored SSE stops the poll chain", async () => {
  const h = makeHarness();
  h.stream.connect();
  h.ES.instances[0].readyState = h.ES.CLOSED;
  h.timers.tickIntervals();
  await settle();
  assert.equal(h.fetchCalls(), 1);
  assert.equal(h.stream.isPolling(), true);

  // The watchdog opened a fresh EventSource when it fell back to polling.
  assert.equal(h.ES.instances.length, 2);
  const es = h.ES.instances[1];
  es.open();

  assert.equal(h.stream.isPolling(), false, "polling must stop when SSE restores");
  assert.equal(h.timers.pendingTimeouts(), 0, "no poll reschedule may remain");
  h.timers.runDueTimeouts();
  await settle();
  assert.equal(h.fetchCalls(), 1, "no further /api/state requests after SSE restore");

  es.emit("state", { data: JSON.stringify({ state: "Run" }) });
  assert.deepEqual(h.snapshots.at(-1), { state: "Run" });
});

test("state event stops an active poll chain even without onopen", async () => {
  const h = makeHarness();
  h.stream.connect();
  h.ES.instances[0].readyState = h.ES.CLOSED;
  h.timers.tickIntervals();
  await settle();
  assert.equal(h.stream.isPolling(), true);

  h.ES.instances[1].emit("state", { data: JSON.stringify({ state: "Idle" }) });
  assert.equal(h.stream.isPolling(), false);
  assert.equal(h.timers.pendingTimeouts(), 0);
});

test("no EventSource support falls back to a single poll chain", async () => {
  const h = makeHarness({ eventSource: false });
  h.stream.connect();
  await settle();
  assert.equal(h.fetchCalls(), 1);
  assert.equal(h.timers.pendingTimeouts(), 1);
  h.timers.runDueTimeouts();
  await settle();
  assert.equal(h.fetchCalls(), 2);
  assert.equal(h.timers.pendingTimeouts(), 1);
});

test("SSE events deliver snapshots without any polling", async () => {
  const h = makeHarness();
  h.stream.connect();
  const es = h.ES.instances[0];
  es.open();
  es.emit("state", { data: JSON.stringify({ state: "Idle" }) });
  await settle();
  assert.equal(h.fetchCalls(), 0);
  assert.equal(h.timers.pendingTimeouts(), 0);
  assert.deepEqual(h.snapshots.at(-1), { state: "Idle" });
});

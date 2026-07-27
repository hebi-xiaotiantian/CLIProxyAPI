import assert from "node:assert/strict";
import fs from "node:fs";
import test from "node:test";
import vm from "node:vm";

const html = fs.readFileSync(new URL("./index.html", import.meta.url), "utf8");
const scriptMatch = html.match(/<script>([\s\S]*?)<\/script>/);
assert.ok(scriptMatch, "index.html must contain an inline script");
const pageScript = scriptMatch[1];

class FakeClassList {
  constructor() {
    this.values = new Set();
  }

  add(value) {
    this.values.add(value);
  }

  contains(value) {
    return this.values.has(value);
  }

  toggle(value, force) {
    const enabled = force === undefined ? !this.values.has(value) : Boolean(force);
    enabled ? this.values.add(value) : this.values.delete(value);
    return enabled;
  }
}

function createElement(id = "") {
  let htmlValue = "";
  const element = {
    id,
    value: "",
    disabled: false,
    checked: false,
    textContent: "",
    className: "",
    dataset: {},
    classList: new FakeClassList(),
    innerHTMLWrites: 0,
    showModal() {},
    close() {},
    querySelector() {
      return createElement();
    },
    querySelectorAll() {
      return [];
    },
    matches() {
      return false;
    }
  };
  Object.defineProperty(element, "innerHTML", {
    get() {
      return htmlValue;
    },
    set(value) {
      htmlValue = String(value);
      element.innerHTMLWrites++;
    }
  });
  return element;
}

function account(id, overrides = {}) {
  return {
    id,
    label: id,
    email: `${id}@example.com`,
    plan: "paid",
    plan_type: "plus",
    quota_fresh: true,
    five_hour_remaining: 80,
    five_hour_window_present: true,
    weekly_remaining: 70,
    weekly_window_present: true,
    policy: {
      enabled: true,
      priority: 100,
      weight: 1,
      backup: false,
      five_hour_reserve: 10,
      weekly_reserve: 15
    },
    refresh: { state: "idle" },
    ...overrides
  };
}

function profile(name) {
  return {
    effective: name,
    persistent: name,
    temporary: null,
    custom_plan_order: ["paid", "free", "unknown"]
  };
}

function createHarness({ editableRow = false } = {}) {
  const elements = new Map();
  const handlers = new Map();
  const pendingFetches = [];
  const timers = [];
  const rows = [];
  let nextTimerID = 0;

  const accountRows = createElement("accountRows");
  elements.set("accountRows", accountRows);
  for (const id of ["searchInput", "planFilter", "stateFilter"]) {
    elements.set(id, createElement(id));
  }

  let editable = null;
  if (editableRow) {
    const row = createElement("row-auth-a");
    row.dataset.id = "auth-a";
    const selected = createElement("selected-auth-a");
    const toggles = ["backup", "enabled"].map(field => {
      const button = createElement(`toggle-${field}`);
      button.dataset.toggle = field;
      return button;
    });
    const fields = ["priority", "weight", "five_hour_reserve", "weekly_reserve"].map(field => {
      const input = createElement(`field-${field}`);
      input.dataset.field = field;
      input.matches = selector => selector === "#accountRows input[data-field]";
      return input;
    });
    row.querySelector = selector => selector === "[data-select]" ? selected : createElement();
    row.querySelectorAll = selector => {
      if (selector === "[data-field]") return fields;
      if (selector === "[data-toggle]") return toggles;
      return [];
    };
    rows.push(row);
    editable = {
      row,
      fields: Object.fromEntries(fields.map(input => [input.dataset.field, input]))
    };
  }

  const documentStub = {
    hidden: false,
    activeElement: null,
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, createElement(id));
      return elements.get(id);
    },
    querySelectorAll(selector) {
      if (selector === "#accountRows tr[data-id]") return rows;
      return [];
    },
    addEventListener(name, handler) {
      handlers.set(name, handler);
    }
  };

  const context = vm.createContext({
    console,
    Intl,
    Map,
    Set,
    Promise,
    Date,
    Math,
    Number,
    String,
    JSON,
    Error,
    sessionStorage: {
      getItem() {
        return "";
      },
      setItem() {}
    },
    document: documentStub,
    fetch(url, options = {}) {
      return new Promise((resolve, reject) => {
        pendingFetches.push({ url, options, resolve, reject });
      });
    },
    setTimeout(callback, delay) {
      const timer = { id: ++nextTimerID, callback, delay, cleared: false };
      timers.push(timer);
      return timer.id;
    },
    clearTimeout(id) {
      const timer = timers.find(item => item.id === id);
      if (timer) timer.cleared = true;
    },
    alert() {}
  });

  vm.runInContext(`${pageScript}
    ;globalThis.__test = {
      load,
      refresh,
      refreshAccounts,
      pollRefreshStatus,
      renderAccounts,
      setKey(value) { managementKey = value; },
      setAccounts(value) { accounts = value; },
      setCurrentProfile(value) { currentProfile = value; },
      activateRefresh(generation) {
        refreshPollGeneration = generation;
        clearTimeout(accountPollTimer);
        accountPollTimer = 0;
        activeRefreshPollGeneration = generation;
      },
      state() {
        return {
          accounts,
          decisions,
          currentProfile,
          pending: Array.from(pending.entries()),
          activeRefreshPollGeneration,
          accountRequestSequence,
          connectionText: $("connectionStatus").textContent,
          connectionClass: $("connectionStatus").className,
          applyDisabled: $("applyButton").disabled
        };
      }
    };`, context);

  const response = (data, status = 200) => ({
    ok: status >= 200 && status < 300,
    status,
    json: async () => data
  });

  return {
    api: context.__test,
    accountRows,
    document: documentStub,
    editable,
    handlers,
    pendingFetches,
    timers,
    activeTimers(delay) {
      return timers.filter(timer => !timer.cleared && timer.delay === delay);
    },
    fireTimer(timer) {
      timer.cleared = true;
      return timer.callback();
    },
    resolve(request, data, status = 200) {
      request.resolve(response(data, status));
    },
    async flush() {
      for (let step = 0; step < 8; step++) {
        await Promise.resolve();
      }
    }
  };
}

test("concurrent loads are whole-operation last-started-wins", async () => {
  const harness = createHarness();
  harness.api.setKey("test-key");

  const oldLoad = harness.api.load();
  const oldRequests = harness.pendingFetches.splice(0, 3);
  const newLoad = harness.api.load();
  const newRequests = harness.pendingFetches.splice(0, 3);

  harness.resolve(oldRequests[0], { accounts: [account("old-account")] });
  harness.resolve(oldRequests[1], profile("old-profile"));
  harness.resolve(oldRequests[2], { decisions: [{ auth_id: "old-decision" }] });
  assert.equal(await oldLoad, false);
  assert.equal(harness.api.state().currentProfile, "");
  assert.equal(harness.api.state().decisions.length, 0);
  assert.equal(harness.api.state().accounts.length, 0);

  harness.resolve(newRequests[0], { accounts: [account("new-account")] });
  harness.resolve(newRequests[1], profile("new-profile"));
  harness.resolve(newRequests[2], { decisions: [{ auth_id: "new-decision" }] });
  assert.equal(await newLoad, true);

  const state = harness.api.state();
  assert.equal(state.currentProfile, "new-profile");
  assert.equal(state.decisions[0].auth_id, "new-decision");
  assert.equal(state.accounts[0].id, "new-account");
});

test("superseded load error cannot replace a newer accounts success status", async () => {
  const harness = createHarness();
  harness.api.setKey("test-key");

  const loading = harness.api.load();
  const loadRequests = harness.pendingFetches.splice(0, 3);
  const refreshing = harness.api.refreshAccounts();
  const refreshRequest = harness.pendingFetches.shift();

  harness.resolve(refreshRequest, { accounts: [account("new-account")] });
  assert.equal(await refreshing, true);
  assert.equal(harness.api.state().connectionText, "已连接");

  harness.resolve(loadRequests[0], { accounts: [account("old-account")] });
  harness.resolve(loadRequests[1], { error: "old profile failed" }, 500);
  harness.resolve(loadRequests[2], { decisions: [] });
  assert.equal(await loading, false);

  const state = harness.api.state();
  assert.equal(state.connectionText, "已连接");
  assert.equal(state.connectionClass, "status success");
  assert.equal(state.accounts[0].id, "new-account");
});

test("latest load still applies profile and decisions when accounts are superseded", async () => {
  const harness = createHarness();
  harness.api.setKey("test-key");

  const loading = harness.api.load();
  const loadRequests = harness.pendingFetches.splice(0, 3);
  const refreshing = harness.api.refreshAccounts();
  const refreshRequest = harness.pendingFetches.shift();

  harness.resolve(refreshRequest, { accounts: [account("new-account")] });
  assert.equal(await refreshing, true);
  harness.resolve(loadRequests[0], { accounts: [account("old-account")] });
  harness.resolve(loadRequests[1], profile("latest-profile"));
  harness.resolve(loadRequests[2], { decisions: [{ auth_id: "latest-decision" }] });
  assert.equal(await loading, true);

  const state = harness.api.state();
  assert.equal(state.accounts[0].id, "new-account");
  assert.equal(state.currentProfile, "latest-profile");
  assert.equal(state.decisions[0].auth_id, "latest-decision");
  assert.equal(state.connectionText, "已连接");
});

test("out-of-order accounts responses keep the latest-started result", async () => {
  const harness = createHarness();
  harness.api.setKey("test-key");

  const oldRefresh = harness.api.refreshAccounts();
  const oldRequest = harness.pendingFetches.shift();
  const newRefresh = harness.api.refreshAccounts();
  const newRequest = harness.pendingFetches.shift();

  harness.resolve(newRequest, { accounts: [account("new-account")] });
  assert.equal(await newRefresh, true);
  harness.resolve(oldRequest, { accounts: [account("old-account")] });
  assert.equal(await oldRefresh, false);
  assert.equal(harness.api.state().accounts[0].id, "new-account");
});

test("rapid hide and show preserves the manual refresh chain", async () => {
  const harness = createHarness();
  harness.api.setKey("test-key");
  harness.api.activateRefresh(1);

  const initialPoll = harness.api.pollRefreshStatus(1);
  harness.resolve(harness.pendingFetches.shift(), {
    accounts: [account("initial", { refresh: { state: "queued" } })]
  });
  await initialPoll;
  const originalFollowUp = harness.activeTimers(2000)[0];
  assert.ok(originalFollowUp);

  harness.document.hidden = true;
  harness.handlers.get("visibilitychange")();
  harness.document.hidden = false;
  const requestsBeforeShow = harness.pendingFetches.length;
  const immediateRefresh = harness.handlers.get("visibilitychange")();
  assert.equal(harness.pendingFetches.length - requestsBeforeShow, 1);
  assert.equal(harness.api.state().activeRefreshPollGeneration, 1);
  assert.equal(harness.activeTimers(5000).length, 0);
  assert.equal(originalFollowUp.cleared, false);

  harness.resolve(harness.pendingFetches.shift(), {
    accounts: [account("show-immediate", { refresh: { state: "queued" } })]
  });
  await immediateRefresh;

  const inFlightFollowUp = harness.fireTimer(originalFollowUp);
  const oldInFlightRequest = harness.pendingFetches.shift();
  harness.document.hidden = true;
  harness.handlers.get("visibilitychange")();
  harness.document.hidden = false;
  const newerVisibilityRefresh = harness.handlers.get("visibilitychange")();
  const newerVisibilityRequest = harness.pendingFetches.shift();

  harness.resolve(newerVisibilityRequest, {
    accounts: [account("visibility-new", { refresh: { state: "queued" } })]
  });
  await newerVisibilityRefresh;
  harness.resolve(oldInFlightRequest, {
    accounts: [account("inflight-old", { refresh: { state: "queued" } })]
  });
  await inFlightFollowUp;

  const state = harness.api.state();
  assert.equal(state.accounts[0].id, "visibility-new");
  assert.equal(state.activeRefreshPollGeneration, 1);
  assert.equal(harness.activeTimers(5000).length, 0);
  assert.equal(harness.activeTimers(2000).length, 1);
});

test("superseded manual refresh chain cannot clear its replacement", async () => {
  const harness = createHarness();
  harness.api.setKey("test-key");

  const firstRefresh = harness.api.refresh();
  harness.resolve(harness.pendingFetches.shift(), {});
  await harness.flush();
  harness.resolve(harness.pendingFetches.shift(), {
    accounts: [account("first", { refresh: { state: "queued" } })]
  });
  await firstRefresh;
  const oldFollowUp = harness.activeTimers(2000)[0];
  assert.ok(oldFollowUp);

  const secondRefresh = harness.api.refresh();
  harness.resolve(harness.pendingFetches.shift(), {});
  await harness.flush();
  assert.equal(oldFollowUp.cleared, true);
  assert.equal(harness.api.state().activeRefreshPollGeneration, 2);

  await harness.fireTimer(oldFollowUp);
  assert.equal(harness.api.state().activeRefreshPollGeneration, 2);

  harness.resolve(harness.pendingFetches.shift(), {
    accounts: [account("second", { refresh: { state: "queued" } })]
  });
  await secondRefresh;
  const newFollowUp = harness.activeTimers(2000)[0];
  assert.ok(newFollowUp);
  assert.notEqual(newFollowUp.id, oldFollowUp.id);
  assert.equal(harness.activeTimers(5000).length, 0);

  const finishingPoll = harness.fireTimer(newFollowUp);
  harness.resolve(harness.pendingFetches.shift(), {
    accounts: [account("second", { refresh: { state: "idle" } })]
  });
  await finishingPoll;
  assert.equal(harness.api.state().activeRefreshPollGeneration, 0);
  assert.equal(harness.activeTimers(5000).length, 1);
});

test("focused numeric edit survives account polling and stages all numeric fields", async () => {
  const harness = createHarness({ editableRow: true });
  harness.api.setKey("test-key");
  harness.api.setCurrentProfile("paid-first");
  harness.api.setAccounts([account("auth-a")]);
  harness.api.renderAccounts();

  const values = {
    priority: 777,
    weight: 3,
    five_hour_reserve: 21,
    weekly_reserve: 34
  };
  for (const [field, value] of Object.entries(values)) {
    const input = harness.editable.fields[field];
    input.value = String(value);
    assert.equal(typeof input.oninput, "function");
    input.oninput();
  }

  const priorityInput = harness.editable.fields.priority;
  harness.document.activeElement = priorityInput;
  const writesBeforePoll = harness.accountRows.innerHTMLWrites;
  const refreshing = harness.api.refreshAccounts();
  harness.resolve(harness.pendingFetches.shift(), {
    accounts: [account("auth-a", { policy: { ...account("auth-a").policy, priority: 100 } })]
  });
  assert.equal(await refreshing, true);

  const state = harness.api.state();
  const pending = Object.fromEntries(state.pending);
  assert.deepEqual(JSON.parse(JSON.stringify(pending["auth-a"])), values);
  assert.equal(state.applyDisabled, false);
  assert.equal(harness.editable.row.classList.contains("changed"), true);
  assert.equal(priorityInput.value, "777");
  assert.equal(harness.accountRows.innerHTMLWrites, writesBeforePoll);

  assert.equal(typeof priorityInput.onblur, "function");
  priorityInput.onblur();
  assert.equal(harness.accountRows.innerHTMLWrites, writesBeforePoll + 1);
  assert.match(harness.accountRows.innerHTML, /data-field="priority" value="777"/);
});

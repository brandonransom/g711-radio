const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");

function setup() {
  const elements = [];
  let active = [];
  let scheduledPoll;
  function element() {
    const el = {
      dataset: {},
      textContent: "",
      classList: { toggle() {} },
      setAttribute(name, value) { this[name] = value; },
      getAttribute(name) { return this[name]; },
    };
    elements.push(el);
    return el;
  }
  const context = vm.createContext({
    document: {
      head: { append() {} },
      createElement: element,
      querySelector: () => null,
      querySelectorAll: (selector) => elements.filter((el) =>
        selector === "[data-audio-state]" ? el.dataset.audioState : el.dataset.activityIds),
      addEventListener() {},
      visibilityState: "visible",
    },
    fetch: async () => ({ ok: true, json: async () => ({ active }) }),
    setTimeout(fn) { scheduledPoll = fn; },
    clearTimeout() {},
  });
  context.window = context;
  vm.runInContext(fs.readFileSync(path.join(__dirname, "web", "activity.js"), "utf8"), context);
  return {
    context,
    element,
    async poll(ids) {
      active = ids;
      await scheduledPoll();
    },
  };
}

function loadFunctions(context, page, names) {
  const html = fs.readFileSync(path.join(__dirname, "web", page), "utf8");
  for (const name of names) {
    const match = html.match(new RegExp(`      function ${name}\\([^]*?\\n      \\}`));
    assert.ok(match, `Missing ${name} in ${page}`);
    vm.runInContext(match[0], context);
  }
}

test("activity updates disconnected and connected copy independently for each stream", async () => {
  const { context, element, poll } = setup();
  const first = element();
  const second = element();
  context.setLiveAudioStatus(first, "a", false);
  context.setLiveAudioStatus(second, "b", false);
  assert.equal(first.textContent, "Not connected to live audio. Connect to listen for transmissions.");

  await poll(["a"]);
  assert.equal(first.textContent, "Transmission in progress. Connect to hear live audio.");
  assert.equal(second.textContent, "Not connected to live audio. Connect to listen for transmissions.");

  context.setLiveAudioStatus(first, "a", true);
  assert.equal(first.textContent, "Live audio connected. Transmission in progress.");
  await poll([]);
  assert.equal(first.textContent, "Live audio connected. Waiting for transmissions.");
  await poll(["b"]);
  assert.equal(first.textContent, "Live audio connected. Waiting for transmissions.");
  assert.equal(second.textContent, "Transmission in progress. Connect to hear live audio.");

  context.setLiveAudioStatus(first, "a", false);
  assert.equal(first.textContent, "Not connected to live audio. Connect to listen for transmissions.");
});

for (const page of ["index.html", "section.html"]) {
  test(`${page}: disconnect restores the activity-aware invitation`, async () => {
    const { context, element, poll } = setup();
    const status = element();
    const button = { classList: { remove() {} } };
    const card = { stream: { id: "a" }, statusEl: status, button };
    Object.assign(context, {
      statusText: status,
      sharePageStream: { id: "a" },
      reconnectTimeout: null,
      resetConnectionUI() {},
      setConnectionState(state) { context.currentConnectionState = state; },
      renderLaunchView() {},
      renderStreams() {},
      showShareLanding: () => true,
      closeCard() {},
      updateAllButtons() {},
    });
    const disconnectName = page === "index.html" ? "disconnect" : "disconnectCard";
    loadFunctions(context, page, ["setStatus", disconnectName]);
    context.setLiveAudioStatus(status, "a", true);
    await poll(["a"]);
    if (page === "index.html") {
      context.disconnect(true, true);
      assert.equal(context.currentConnectionState, "idle");
    } else {
      context.disconnectCard(card);
      assert.equal(button.textContent, "Connect to live audio");
      assert.equal(button.disabled, false);
    }
    assert.equal(status.textContent, "Transmission in progress. Connect to hear live audio.");
    await poll([]);
    assert.equal(status.textContent, "Not connected to live audio. Connect to listen for transmissions.");
  });

  test(`${page}: activity preserves progress, failure, and paused playback messages`, async () => {
    const { context, element, poll } = setup();
    const status = element();
    const audio = { paused: false };
    const card = {
      pc: { connectionState: "connected" },
      stream: { id: "a" },
      statusEl: status,
      audioEl: audio,
    };
    Object.assign(context, {
      statusText: status,
      player: audio,
      currentConnectionState: "connected",
      currentStreamId: "a",
    });
    loadFunctions(context, page, ["setStatus", "updateLiveAudioStatus"]);
    const update = () => context.updateLiveAudioStatus(card);
    const setStatus = (text) => page === "index.html"
      ? context.setStatus(text) : context.setStatus(card, text);

    update();
    await poll(["a"]);
    assert.equal(status.textContent, "Live audio connected. Transmission in progress.");

    for (const message of ["Connecting to live audio...", "Live audio interrupted. Attempting to reconnect...", "Audio could not start. Press Play in the audio controls to retry: blocked"]) {
      setStatus(message);
      await poll([]);
      await poll(["a"]);
      assert.equal(status.textContent, message);
    }

    audio.paused = true;
    update();
    await poll([]);
    assert.equal(status.textContent, "Live audio paused. Press Play in the audio controls to listen.");

    audio.paused = false;
    update();
    assert.equal(status.textContent, "Live audio connected. Waiting for transmissions.");

    context.currentConnectionState = "reconnecting";
    card.pc.connectionState = "disconnected";
    setStatus("Live audio interrupted. Attempting to reconnect...");
    update();
    await poll(["a"]);
    assert.equal(status.textContent, "Live audio interrupted. Attempting to reconnect...");
  });

  test(`${page}: inline scripts parse`, () => {
    const html = fs.readFileSync(path.join(__dirname, "web", page), "utf8");
    for (const [, script] of html.matchAll(/<script>([^]*?)<\/script>/g)) {
      assert.doesNotThrow(() => new vm.Script(script));
    }
  });
}

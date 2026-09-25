const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");

for (const [page, idField] of [["index.html", "clipId"], ["section.html", "key"]]) {
  function setup() {
    const html = fs.readFileSync(path.join(__dirname, "web", page), "utf8");
    const functions = ["updateClipPlayButtons", "playNextQueuedClip", "enqueueClipPlayback"].map((name) => {
      const match = html.match(new RegExp(`      function ${name}\\([^]*?\\n      \\}`));
      assert.ok(match, `Missing ${name} in ${page}`);
      return match[0];
    });
    const listeners = new Set();
    const requests = [];
    const audio = {
      paused: true,
      currentTime: 0,
      addEventListener(event, listener) {
        assert.equal(event, "ended");
        listeners.add(listener);
      },
      removeEventListener(event, listener) {
        assert.equal(event, "ended");
        listeners.delete(listener);
      },
      play() {
        this.paused = false;
        return new Promise((resolve, reject) => requests.push({ resolve, reject }));
      },
      pause() { this.paused = true; },
      removeAttribute(name) { delete this[name]; },
      load() { this.currentTime = 0; },
    };
    const datasetField = idField === "key" ? "clipKey" : "clipId";
    const buttons = ["a", "b"].map((id) => ({ dataset: { [datasetField]: id } }));
    const context = vm.createContext({
      clipAudio: audio,
      clipPlaybackActive: null,
      clipPlaybackQueue: [],
      clipPlaybackSerial: 0,
      transcriptLog: { querySelectorAll: () => buttons },
      attachClipScrubBar(id) { context.scrub = id; },
      detachClipScrubBar() { context.scrub = null; },
    });
    vm.runInContext(functions.join("\n"), context);
    return { context, audio, buttons, requests, listeners };
  }

  test(`${page}: stop resets playback and permits replay`, () => {
    const { context, audio, buttons, listeners } = setup();
    context.enqueueClipPlayback("a", "/audio/a.wav");
    assert.equal(audio.paused, false);
    assert.equal(buttons[0].title, "Stop recording");
    audio.currentTime = 12;
    context.enqueueClipPlayback("a", "/audio/a.wav");
    assert.equal(audio.paused, true);
    assert.equal(audio.currentTime, 0);
    assert.equal(audio.src, undefined);
    assert.equal(context.clipPlaybackActive, null);
    assert.equal(context.scrub, null);
    assert.equal(buttons[0].title, "Play recording");
    assert.equal(listeners.size, 0);
    context.enqueueClipPlayback("a", "/audio/a.wav");
    assert.equal(audio.paused, false);
    assert.equal(context.scrub, "a");
    assert.equal(listeners.size, 1);
  });

  test(`${page}: stopping advances the queue; ending completes playback`, () => {
    const { context, audio, listeners } = setup();
    context.enqueueClipPlayback("a", "/audio/a.wav");
    context.enqueueClipPlayback("b", "/audio/b.wav");
    context.enqueueClipPlayback("b", "/audio/b.wav");
    assert.equal(context.clipPlaybackQueue.length, 1);
    context.enqueueClipPlayback("a", "/audio/a.wav");
    assert.equal(context.clipPlaybackActive[idField], "b");
    assert.equal(audio.src, "/audio/b.wav");
    assert.equal(listeners.size, 1);
    for (const listener of [...listeners]) listener();
    assert.equal(context.clipPlaybackActive, null);
    assert.equal(audio.paused, true);
    assert.equal(listeners.size, 0);
  });

  test(`${page}: stale play rejection does not stop a replay`, async () => {
    const { context, audio, requests, listeners } = setup();
    context.enqueueClipPlayback("a", "/audio/a.wav");
    context.enqueueClipPlayback("a", "/audio/a.wav");
    context.enqueueClipPlayback("a", "/audio/a.wav");
    requests[0].reject(new Error("Playback aborted by stop"));
    await Promise.resolve();
    assert.equal(context.clipPlaybackActive[idField], "a");
    assert.equal(audio.paused, false);
    assert.equal(listeners.size, 1);
    requests[1].reject(new Error("Playback failed"));
    await Promise.resolve();
    assert.equal(context.clipPlaybackActive, null);
    assert.equal(listeners.size, 0);
  });
}

const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");

function extractFunction(html, name) {
  const match = html.match(new RegExp(`      function ${name}\\([^]*?\\n      \\}`));
  assert.ok(match, `Missing ${name}`);
  return match[0];
}

function setupFilter(useFullNames = false) {
  const html = fs.readFileSync(path.join(__dirname, "web", "section.html"), "utf8");
  const select = {
    options: [],
    set innerHTML(value) { this.options = []; },
    append(...options) { this.options.push(...options); },
    get selectedOptions() { return this.options.filter((o) => o.selected); },
  };
  const context = vm.createContext({
    historyStreamSelect: select,
    historyAllStreamsSelected: true,
    untranscribedAudioOnly: false,
    document: { createElement: () => ({ selected: false }) },
    historyMinDurationInput: { value: "2" },
    historyEntries: [
      { streamId: "a", durationMs: 3000, audioUrl: "/a.wav" },
      { streamId: "z", durationMs: 4000, audioUrl: "/z.wav" },
      { streamId: "a", durationMs: 1000, audioUrl: "/short.wav" },
    ],
  });
  const names = [
    "formatStreamDisplayName", "getSelectedStreamIds", "normalizeStreamFilterSelection",
    "populateStreamFilterOptions", "historyDurationFilter", "passesDurationFilter",
    "filteredHistoryEntries", "filteredAudioURLs",
  ];
  vm.runInContext(names.map((name) => extractFunction(html, name)).join("\n"), context);
  context.populateStreamFilterOptions([
    { stream: { id: "z", streamName: "Zulu", stateName: "Z State" } },
    { stream: { id: "a", streamName: "Aardvark", stateName: "A State" } },
  ], useFullNames);
  return { context, select };
}

test("All Streams stays first and selected above alphabetically sorted names", () => {
  for (const fullNames of [false, true]) {
    const { context, select } = setupFilter(fullNames);
    assert.equal(select.options[0].textContent, "All Streams");
    assert.deepEqual(select.options.map((o) => o.value), ["", "a", "z"]);
    assert.equal(select.options[0].selected, true);
    assert.equal(context.getSelectedStreamIds().size, 0);
    assert.equal(context.filteredHistoryEntries().length, 2);
  }
});

test("individual selection replaces All Streams, including additive selection", () => {
  const { context, select } = setupFilter();
  select.options[1].selected = true;
  context.normalizeStreamFilterSelection();
  assert.equal(select.options[0].selected, false);
  assert.deepEqual(Array.from(context.getSelectedStreamIds()), ["a"]);
  assert.deepEqual(Array.from(context.filteredAudioURLs()), ["/a.wav"]);
  select.options[2].selected = true;
  context.normalizeStreamFilterSelection();
  assert.equal(context.getSelectedStreamIds().size, 2);
});

test("selecting All Streams clears individual streams", () => {
  const { context, select } = setupFilter();
  select.options[0].selected = false;
  select.options[1].selected = true;
  select.options[2].selected = true;
  context.normalizeStreamFilterSelection();
  select.options[0].selected = true;
  context.normalizeStreamFilterSelection();
  assert.deepEqual(select.options.map((o) => o.selected), [true, false, false]);
  assert.equal(context.getSelectedStreamIds().size, 0);
});

test("deselecting the last stream restores All Streams", () => {
  const { context, select } = setupFilter();
  select.options[0].selected = false;
  select.options[1].selected = true;
  context.normalizeStreamFilterSelection();
  select.options[1].selected = false;
  context.normalizeStreamFilterSelection();
  assert.equal(select.options[0].selected, true);
  assert.equal(context.getSelectedStreamIds().size, 0);
});

for (const page of ["index.html", "section.html"]) {
  test(`${page}: transcript arrival does not duplicate a filtered-out recording`, () => {
    const html = fs.readFileSync(path.join(__dirname, "web", page), "utf8");
    const row = { clipId: "a", key: "a", wavFilename: "a.wav", wav: "a.wav", text: "" };
    const context = vm.createContext({
      untranscribedAudioOnly: true,
      historyEntries: [row],
      findHistoryEntry: () => row,
      setTranscriptionPending: () => {},
      findTranscriptEntry: () => { throw new Error("Filtered view must not append a duplicate"); },
    });
    vm.runInContext(extractFunction(html, "updateTranscript"), context);
    if (page === "index.html") context.updateTranscript("a", "Arrived", "a.wav");
    else context.updateTranscript("a", "Stream", "", "/a.wav", 3000, "Arrived", "a.wav");
    assert.equal(context.historyEntries.length, 1);
    assert.equal(row.text, "Arrived");
  });

  test(`${page}: untranscribed filter toggles and retains other filters`, () => {
    const html = fs.readFileSync(path.join(__dirname, "web", page), "utf8");
    const button = { setAttribute(name, value) { this[name] = value; } };
    let renders = 0;
    const context = vm.createContext({
      untranscribedAudioOnly: false,
      TRANSCRIPTION_FAILED: "[transcription failed]",
      filterUntranscribedAudioBtn: button,
      historyMinDurationInput: { value: "2" },
      getSelectedStreamIds: () => new Set(["a"]),
      isTranscriptionPending: (ids) => ids.includes("queued"),
      applyHistoryFiltersAndRender: () => { renders++; },
      historyEntries: [
        { streamId: "a", key: "empty", clipId: "empty", durationMs: 3000, audioUrl: "/empty.wav", text: "" },
        { streamId: "a", key: "failed", clipId: "failed", durationMs: 3000, audioUrl: "/failed.wav", text: "[transcription failed]" },
        { streamId: "a", key: "done", clipId: "done", durationMs: 3000, audioUrl: "/done.wav", text: "Done" },
        { streamId: "a", key: "queued", clipId: "queued", durationMs: 3000, audioUrl: "/queued.wav", text: "" },
        { streamId: "a", durationMs: 1000, audioUrl: "/short.wav", text: "" },
        { streamId: "a", durationMs: 3000, text: "" },
        { streamId: "b", durationMs: 3000, audioUrl: "/b.wav", text: "" },
      ],
    });
    const names = [
      "historyDurationFilter", "passesDurationFilter", "filteredHistoryEntries",
      "filteredAudioURLs", "isUntranscribedAudio", "toggleUntranscribedAudioFilter",
      "untranscribedFilteredEntries",
    ];
    vm.runInContext(names.map((name) => extractFunction(html, name)).join("\n"), context);
    assert.match(html, /const MAX_BULK_TRANSCRIBE = 10000;/);
    context.toggleUntranscribedAudioFilter();
    assert.equal(button["aria-pressed"], "true");
    const expected = ["/empty.wav", "/failed.wav", "/queued.wav"];
    if (page === "index.html") expected.push("/b.wav");
    assert.deepEqual(Array.from(context.filteredAudioURLs()), expected);
    assert.equal(context.untranscribedFilteredEntries().some((ev) => ev.audioUrl === "/queued.wav"), false);
    context.historyEntries[0].text = "Arrived";
    assert.equal(context.filteredAudioURLs().includes("/empty.wav"), false);
    context.toggleUntranscribedAudioFilter();
    assert.equal(button["aria-pressed"], "false");
    assert.equal(context.filteredAudioURLs().includes("/done.wav"), true);
    assert.equal(renders, 2);
  });

  test(`${page}: bulk controls are hidden unless user=super`, () => {
    const html = fs.readFileSync(path.join(__dirname, "web", page), "utf8");
    assert.match(html, /<div id="bulkAudioActions"[^>]* hidden>/);
    assert.doesNotMatch(html, /historyStreamClearBtn|Streams \(none = all\)/);
    for (const query of ["", "?all=1", "?user=normal", "?user=Super", "?all=1&user=super"]) {
      const actions = { hidden: true };
      const context = vm.createContext({
        URL,
        window: { location: { href: `http://localhost/${page}${query}` } },
        document: { getElementById: (id) => {
          assert.equal(id, "bulkAudioActions");
          return actions;
        } },
      });
      vm.runInContext(extractFunction(html, "configureBulkAudioControls"), context);
      context.configureBulkAudioControls();
      assert.equal(actions.hidden, !query.includes("user=super"));
    }
    const scripts = [...html.matchAll(/<script(?:\s[^>]*)?>([^]*?)<\/script>/g)];
    for (const [, script] of scripts) new vm.Script(script);
  });
}

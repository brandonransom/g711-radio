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

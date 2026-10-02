// Keeps listening from being cut off by navigation. Leaving a page closes its
// live streams and stops any recording that is playing, so while something is
// playing in this tab (or in a same-site frame within it):
//  - a plain click on a link opens the destination in a new tab instead, and
//  - Back, reload, or close asks for confirmation while live streams are
//    connected (the browser shows its own "Leave site?" dialog).
// When nothing is playing, links and navigation behave normally. Pages report
// their state through window.g711AudioState() => { live, playing }.
(() => {
  function sameSiteWindows() {
    const found = [];
    const visit = (win) => {
      try {
        void win.document; // throws for cross-origin frames
      } catch (_) {
        return;
      }
      found.push(win);
      for (let i = 0; i < win.frames.length; i++) visit(win.frames[i]);
    };
    let root = window;
    try {
      void window.top.document;
      root = window.top;
    } catch (_) { /* framed by another site; only this window counts */ }
    visit(root);
    return found;
  }

  function audioState() {
    const state = { live: false, playing: false };
    for (const win of sameSiteWindows()) {
      let s;
      try {
        s = typeof win.g711AudioState === "function" ? win.g711AudioState() : null;
      } catch (_) {
        s = null;
      }
      if (s) {
        state.live = state.live || !!s.live;
        state.playing = state.playing || !!s.playing;
      }
    }
    return state;
  }

  function shouldOpenInNewTab(event, link) {
    if (event.defaultPrevented || event.button !== 0) return false;
    // Modified clicks already have their own meaning (new tab/window, save).
    if (event.ctrlKey || event.metaKey || event.shiftKey || event.altKey) return false;
    if (link.hasAttribute("download")) return false;
    const target = (link.getAttribute("target") || link.ownerDocument.querySelector("base[target]")?.getAttribute("target") || "").toLowerCase();
    if (target && target !== "_self" && target !== "_top" && target !== "_parent") return false;
    let url;
    try {
      url = new URL(link.href, link.ownerDocument.baseURI);
    } catch (_) {
      return false;
    }
    if (url.protocol !== "http:" && url.protocol !== "https:") return false;
    // In-page anchors don't leave the page.
    const here = window.location;
    if (url.hash && url.origin === here.origin && url.pathname === here.pathname && url.search === here.search) return false;
    const state = audioState();
    return state.live || state.playing;
  }

  document.addEventListener("click", (event) => {
    const link = event.target instanceof Element ? event.target.closest("a[href]") : null;
    if (!link || !shouldOpenInNewTab(event, link)) return;
    const opened = window.open(link.href, "_blank");
    if (!opened) return; // popup blocked: navigate normally (the leave prompt still applies)
    try { opened.opener = null; } catch (_) {}
    event.preventDefault();
  });

  // Only the top-level page prompts; it covers its frames too.
  if (window === window.top) {
    window.addEventListener("beforeunload", (event) => {
      if (!audioState().live) return;
      event.preventDefault();
      event.returnValue = "";
    });
  }
})();

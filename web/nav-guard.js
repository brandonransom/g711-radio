// Keeps listening from being cut off by navigation, and avoids opening a page
// that is already open in another tab.
//
// Leaving a page closes its live streams and stops any recording that is
// playing, so while something is playing in this tab (or in a same-site frame
// within it):
//  - a plain click on a link opens the destination in a new tab instead, and
//  - Back, reload, or close asks for confirmation while live streams are
//    connected (the browser shows its own "Leave site?" dialog).
// When nothing is playing, links and navigation behave normally.
//
// Before following a link to one of this site's pages:
//  - if this tab opened a tab that still shows that page, switch to it
//    without reloading it (browsers decide whether the switch is allowed;
//    Chrome and Edge allow it after a click), otherwise
//  - if any other tab of this site in this browser shows that page, say so
//    and make that tab's title flash so it's easy to find.
// Web pages can't switch to tabs they didn't open, so that's as far as the
// second case can go.
//
// Pages report their state through window.g711AudioState() => { live, playing }.
(() => {
  const CHANNEL = "g711-tabs";
  const HEARTBEAT_MS = 30000;
  // Background tabs may run timers only once a minute; allow for that.
  const PEER_EXPIRY_MS = 150000;

  function topWindow() {
    try {
      void window.top.document; // throws when framed by another site
      return window.top;
    } catch (_) {
      return window;
    }
  }

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
    visit(topWindow());
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

  // pageKey identifies one of this site's pages regardless of parameter order
  // or "/" vs "/index.html". Non-page URLs and other sites return "".
  function pageKey(href) {
    let url;
    try {
      url = new URL(href, window.location.href);
    } catch (_) {
      return "";
    }
    if (url.origin !== window.location.origin) return "";
    let path = url.pathname;
    if (path === "/index.html") path = "/";
    if (path !== "/" && !path.endsWith(".html")) return "";
    const params = Array.from(url.searchParams.entries())
      .filter(([k]) => k !== "embed")
      .sort(([a, av], [b, bv]) => (a === b ? (av < bv ? -1 : av > bv ? 1 : 0) : a < b ? -1 : 1));
    const search = new URLSearchParams(params).toString();
    return search ? `${path}?${search}` : path;
  }

  // The tab registry lives in the top-level page; same-site frames use it.
  function createRegistry() {
    const id = Math.random().toString(36).slice(2) + Date.now().toString(36);
    const peers = new Map(); // id -> { key, seen }
    const opened = new Map(); // window name -> WindowProxy this tab opened
    let channel = null;
    try {
      channel = typeof BroadcastChannel === "function" ? new BroadcastChannel(CHANNEL) : null;
    } catch (_) {
      channel = null;
    }

    const myKey = () => pageKey(window.location.href);
    const post = (msg) => {
      try {
        if (channel) channel.postMessage(msg);
      } catch (_) { /* channel closed */ }
    };
    const announce = () => post({ type: "hello", id, key: myKey() });

    let flashTimer = null;
    let savedTitle = "";
    const FLASH_PREFIX = "🔔 Already open here — ";
    function stopFlash() {
      if (!flashTimer) return;
      clearInterval(flashTimer);
      flashTimer = null;
      if (document.title.startsWith(FLASH_PREFIX)) document.title = savedTitle;
    }
    function flashTitle() {
      if (document.visibilityState === "visible") return;
      stopFlash();
      let ticks = 0;
      flashTimer = setInterval(() => {
        if (document.visibilityState === "visible" || ++ticks > 30) {
          stopFlash();
          return;
        }
        if (document.title.startsWith(FLASH_PREFIX)) {
          document.title = savedTitle;
        } else {
          savedTitle = document.title;
          document.title = FLASH_PREFIX + savedTitle;
        }
      }, 1000);
    }
    document.addEventListener("visibilitychange", () => {
      if (document.visibilityState === "visible") stopFlash();
    });

    if (channel) {
      channel.onmessage = (event) => {
        const msg = event.data || {};
        if (!msg.id || msg.id === id) return;
        switch (msg.type) {
          case "who":
            announce();
            break;
          case "hello":
            peers.set(msg.id, { key: msg.key, seen: Date.now() });
            break;
          case "bye":
            peers.delete(msg.id);
            break;
          case "attention":
            if (msg.target === id) {
              flashTitle();
              try { window.focus(); } catch (_) {}
            }
            break;
        }
      };
      announce();
      post({ type: "who", id });
      setInterval(announce, HEARTBEAT_MS);
      window.addEventListener("pagehide", () => post({ type: "bye", id }));
      window.addEventListener("pageshow", (event) => {
        if (event.persisted) {
          announce();
          post({ type: "who", id });
        }
      });
      // index.html rewrites its URL in place when a stream is chosen.
      for (const method of ["pushState", "replaceState"]) {
        const original = history[method];
        history[method] = function (...args) {
          const result = original.apply(this, args);
          announce();
          return result;
        };
      }
      window.addEventListener("popstate", announce);
    }

    return {
      // Returns a tab this tab opened that still shows key, or null.
      openedTabFor(key) {
        for (const [name, win] of opened) {
          let winKey = "";
          try {
            winKey = win.closed ? "" : pageKey(win.location.href);
          } catch (_) {
            winKey = "";
          }
          if (!winKey) {
            opened.delete(name);
          } else if (winKey === key) {
            return { name, win };
          }
        }
        return null;
      },
      // Returns the id of another tab that shows key, or "".
      peerFor(key) {
        const now = Date.now();
        for (const [peerId, peer] of peers) {
          if (now - peer.seen > PEER_EXPIRY_MS) {
            peers.delete(peerId);
          } else if (peer.key === key) {
            return peerId;
          }
        }
        return "";
      },
      ask(peerId) {
        post({ type: "attention", id, target: peerId });
      },
      remember(name, win) {
        opened.set(name, win);
      },
    };
  }

  const top = topWindow();
  if (top === window) {
    window.g711Tabs = createRegistry();
  }
  const registry = top.g711Tabs || null;

  function openNewTab(href) {
    const name = "g711-" + Math.random().toString(36).slice(2);
    const win = window.open(href, name);
    if (!win) return false;
    if (registry) registry.remember(name, win);
    return true;
  }

  function focusOpenedTab(entry) {
    // Opening an existing named tab with no URL switches to it without
    // reloading it.
    const win = window.open("", entry.name);
    try { (win || entry.win).focus(); } catch (_) {}
  }

  let noticeEl = null;
  let noticeTimer = null;
  function showNotice(href) {
    const doc = top.document;
    if (!noticeEl || !doc.contains(noticeEl)) {
      noticeEl = doc.createElement("div");
      noticeEl.setAttribute("role", "status");
      noticeEl.style.cssText = [
        "position:fixed", "left:50%", "bottom:20px", "transform:translateX(-50%)", "z-index:2147483647",
        "max-width:min(92vw,520px)", "padding:12px 14px", "border-radius:10px",
        "background:#1e241f", "color:#fff", "font:14px/1.4 system-ui,sans-serif",
        "box-shadow:0 6px 24px rgba(0,0,0,.25)", "display:flex", "gap:12px", "align-items:center", "flex-wrap:wrap",
      ].join(";");
      doc.body.append(noticeEl);
    }
    noticeEl.textContent = "";
    const text = doc.createElement("span");
    text.textContent = "That page is already open in another tab — look for the flashing tab title.";
    text.style.flex = "1 1 220px";
    const button = (label, onClick) => {
      const b = doc.createElement("button");
      b.type = "button";
      b.textContent = label;
      b.style.cssText = "border:1px solid rgba(255,255,255,.5);background:transparent;color:#fff;border-radius:6px;padding:4px 10px;font:inherit;cursor:pointer";
      b.addEventListener("click", onClick);
      return b;
    };
    const hide = () => {
      clearTimeout(noticeTimer);
      if (noticeEl) noticeEl.remove();
      noticeEl = null;
    };
    noticeEl.append(
      text,
      button("Open anyway", () => {
        hide();
        followLink(href, true);
      }),
      button("Dismiss", hide),
    );
    clearTimeout(noticeTimer);
    noticeTimer = setTimeout(hide, 10000);
  }

  // followLink navigates the way a plain click would, except that it opens a
  // new tab while audio is playing.
  function followLink(href, skipDuplicateCheck) {
    const key = pageKey(href);
    if (key && registry && !skipDuplicateCheck) {
      const tab = registry.openedTabFor(key);
      if (tab) {
        focusOpenedTab(tab);
        return;
      }
      const peerId = registry.peerFor(key);
      if (peerId) {
        registry.ask(peerId);
        showNotice(href);
        return;
      }
    }
    const state = audioState();
    if ((state.live || state.playing) && openNewTab(href)) return;
    top.location.href = href;
  }

  function isPlainNavigation(event, link) {
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
    const here = top.location;
    if (url.hash && url.origin === here.origin && url.pathname === here.pathname && url.search === here.search) return false;
    return true;
  }

  document.addEventListener("click", (event) => {
    const link = event.target instanceof Element ? event.target.closest("a[href]") : null;
    if (!link || !isPlainNavigation(event, link)) return;
    const key = pageKey(link.href);
    const duplicate = key && registry && key !== pageKey(top.location.href) &&
      (registry.openedTabFor(key) || registry.peerFor(key));
    const state = audioState();
    if (!duplicate && !state.live && !state.playing) return; // ordinary navigation
    event.preventDefault();
    followLink(link.href, !duplicate);
  });

  // Only the top-level page prompts; it covers its frames too.
  if (top === window) {
    window.addEventListener("beforeunload", (event) => {
      if (!audioState().live) return;
      event.preventDefault();
      event.returnValue = "";
    });
  }
})();

// Subtle "on the air" lights. Any element with a data-activity-ids attribute
// (comma-separated stream IDs) gets the "is-active" class while at least one
// of those streams is receiving audio on the server. This is independent of
// whether this browser is connected to the stream.
(() => {
  const VISIBLE_POLL_MS = 2500;
  const HIDDEN_POLL_MS = 5000;
  let timer = null;
  let active = new Set();

  const style = document.createElement("style");
  style.textContent = `
    .activity-dot {
      display: inline-block;
      flex: none;
      width: 8px;
      height: 8px;
      margin: 0 8px 0 0;
      border-radius: 50%;
      vertical-align: middle;
      background: rgba(30, 36, 31, 0.14);
      transition: background-color 1.2s ease, box-shadow 1.2s ease;
    }
    .activity-dot.is-active {
      background: #16A720;
      box-shadow: 0 0 0 3px rgba(22, 167, 32, 0.18);
      transition-duration: 0.25s;
      animation: activity-breathe 2.4s ease-in-out infinite;
    }
    @keyframes activity-breathe {
      50% { box-shadow: 0 0 0 5px rgba(22, 167, 32, 0.08); }
    }
    @media (prefers-reduced-motion: reduce) {
      .activity-dot.is-active { animation: none; }
    }
  `;
  document.head.append(style);

  function iconHref(color, ring) {
    const svg = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 32 32">${ring ? `<circle cx="16" cy="16" r="15" fill="${ring}"/>` : ""}<circle cx="16" cy="16" r="10" fill="${color}"/></svg>`;
    return `data:image/svg+xml,${encodeURIComponent(svg)}`;
  }
  const IDLE_ICON = iconHref("#1C508A");
  const ACTIVE_ICON = iconHref("#16A720", "rgba(22,167,32,0.35)");

  // The tab icon mirrors the page: green while any stream shown on it is live.
  function setTabIcon(on) {
    let link = document.querySelector('link[rel="icon"]');
    if (!link) {
      link = document.createElement("link");
      link.rel = "icon";
      link.type = "image/svg+xml";
      document.head.append(link);
    }
    const href = on ? ACTIVE_ICON : IDLE_ICON;
    if (link.getAttribute("href") !== href) link.setAttribute("href", href);
  }

  function apply() {
    let anyOnPage = false;
    for (const el of document.querySelectorAll("[data-activity-ids]")) {
      const ids = el.dataset.activityIds ? el.dataset.activityIds.split(",") : [];
      const on = ids.some((id) => active.has(id));
      el.classList.toggle("is-active", on);
      if (on) anyOnPage = true;
    }
    setTabIcon(anyOnPage);
  }

  async function poll() {
    try {
      const resp = await fetch(`/stream-activity?t=${Date.now()}`, { cache: "no-store" });
      if (resp.ok) {
        const body = await resp.json();
        active = new Set(body.active || []);
        apply();
      }
    } catch (_) { /* ignore transient network errors */ }
  }

  function schedule() {
    clearTimeout(timer);
    // Keep polling (more slowly) in hidden tabs so the tab icon stays useful.
    const delay = document.visibilityState === "hidden" ? HIDDEN_POLL_MS : VISIBLE_POLL_MS;
    timer = setTimeout(async () => {
      await poll();
      schedule();
    }, delay);
  }

  // Builds a small dot that lights for any of the given stream IDs.
  window.buildActivityDot = (streamIds) => {
    const dot = document.createElement("span");
    dot.className = "activity-dot";
    dot.dataset.activityIds = streamIds.join(",");
    dot.title = "Lights while audio is being received";
    dot.setAttribute("aria-hidden", "true");
    const on = streamIds.some((id) => active.has(id));
    dot.classList.toggle("is-active", on);
    if (on) setTabIcon(true);
    return dot;
  };

  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") void poll();
    schedule();
  });

  setTabIcon(false);
  void poll();
  schedule();
})();

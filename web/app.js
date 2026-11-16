// Anchor front end. Plain JavaScript, no build step.
(() => {
  "use strict";

  const $ = (id) => document.getElementById(id);
  const REFRESH_MS = 60_000;
  const STATUS_TEXT = { up: "Working", down: "Not responding", unknown: "" };
  const LETTER_COLORS = ["#0f5f7a", "#7a4f0f", "#2f6b3a", "#6a3d7a", "#8a3a3a", "#3d4f8a"];

  let data = null;
  let previewAs = "";
  let reqApp = null, ebookApp = null;
  let current = null; // app shown in the sheet
  let lastSig = "";
  let accessScope = "";
  let launchTimer = null;
  let launchVisibility = null;
  const dialogTriggers = new WeakMap();

  function showDialog(id, trigger) {
    const dialog = $(id);
    dialogTriggers.set(dialog, trigger || document.activeElement);
    dialog.showModal();
    dialog.querySelector("button:not([disabled]), input:not([disabled]), [href]")?.focus();
  }

  function resetPrivateViews() {
    cancelMobileLaunch();
    cancelRequests();
    cancelEbooks();
    cancelReport();
    clearInterval(reqTimer);
    reqTimer = null;
    reqData = ebookData = current = reqApp = ebookApp = null;
    ebookIndexSource = null;
    ebookIndex = [];
    ebookPage = 0;
    reqAt = ebookAt = 0;
    reqNote = ebookNote = "";
    lastSig = "";
    for (const id of ["app-dialog", "guide-dialog", "report-dialog", "req-dialog", "ebooks-dialog"]) {
      if ($(id).open) $(id).close();
    }
    for (const id of ["req-list", "ebooks-list", "req-admin", "req-banner", "req-updated", "req-empty", "ebooks-empty", "ebooks-banner", "ebooks-updated", "ebooks-page",
      "server-addr", "sheet-title", "sheet-note", "store-links", "guide-title", "guide-intro", "guide-steps",
      "sheet-alt-note", "open-app-link", "guide-action", "guide-device-title", "guide-device-steps", "guide-note", "report-title", "report-body", "report-send"]) $(id).textContent = "";
    $("ebooks-paging").hidden = true;
    for (const id of ["guide-action", "open-app-link", "sheet-add-home"]) {
      $(id).removeAttribute("href");
      $(id).onclick = null;
    }
    for (const id of ["sheet-icon", "guide-icon"]) $(id).removeAttribute("src");
    $("req-search").value = $("ebooks-search").value = "";
    clearTimeout(toastTimer);
    $("toast").hidden = true;
    $("toast").textContent = "";
  }

  function acceptData(next) {
    const scope = JSON.stringify([next.scope, next.login, next.known, next.admin, next.viewingAs, next.reports,
      (next.sections || []).flatMap(s => s.apps.map(a => [a.id, a.url, a.locked, a.adminOpen, a.requestStatus, a.ebookLibrary]))]);
    if (scope !== accessScope) resetPrivateViews();
    accessScope = scope;
    data = next;
    if (!next.admin && previewAs) {
      previewAs = "";
      try { sessionStorage.removeItem("anchor.previewAs"); } catch { /* private mode */ }
    }
  }

  // Include response-body decoding in the deadline, not only response headers.
  async function timedJSON(url, options, timeout, operation) {
    const timer = setTimeout(() => operation.controller.abort(), timeout);
    try {
      const res = await fetch(url, { ...options, signal: operation.controller.signal });
      const body = await res.json().catch((err) => {
        if (operation.controller.signal.aborted) throw err;
        return {};
      });
      return { res, body };
    } finally {
      clearTimeout(timer);
    }
  }

  function cancelReport() {
    const operation = reportOperation;
    reportOperation = null;
    reporting = null;
    operation?.controller.abort();
    $("report-send").disabled = false;
  }

  function cancelRequests() {
    const operation = reqOperation;
    reqOperation = null;
    reqBusy = false;
    operation?.controller.abort();
  }

  function cancelEbooks() {
    const operation = ebookOperation;
    ebookOperation = null;
    ebookBusy = false;
    operation?.controller.abort();
  }

  try { previewAs = sessionStorage.getItem("anchor.previewAs") || ""; } catch { /* private mode */ }

  function greeting(name) {
    const h = new Date().getHours();
    const part = h < 5 ? "Hello" : h < 12 ? "Good morning" : h < 18 ? "Good afternoon" : "Good evening";
    return name ? `${part}, ${name}` : part;
  }

  function firstName(name) {
    return (name || "").trim().split(/\s+/)[0] || "";
  }

  // ---------- Loading, and the "can't connect" screen ----------
  const RETRY_MS = 5_000;
  let offline = false;
  let retryTimer = null;
  let loading = false;
  let again = false; // asked to load while a load was running

  function savedContact() {
    if (data && data.contact) return data.contact;
    try { return localStorage.getItem("anchor.contact") || ""; } catch { return ""; }
  }

  async function load() {
    if (loading) {
      // The running request may have been answered just before a change,
      // so fetch once more when it finishes.
      again = true;
      return;
    }
    loading = true;
    const requestedPreview = previewAs;
    const url = previewAs ? `/api/me?as=${encodeURIComponent(previewAs)}` : "/api/me";
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), 8_000);
    try {
      let res;
      try {
        res = await fetch(url, { cache: "no-store", credentials: "same-origin", signal: ctrl.signal });
      } catch (err) {
        // No answer at all: almost always Tailscale is off on this device.
        showOffline("network");
        console.warn("Anchor: can't connect", err);
        return;
      }
      if (!res.ok) {
        // Reached the server, but Anchor itself didn't answer properly.
        showOffline("server");
        console.warn("Anchor: server answered", res.status);
        return;
      }
      const next = await res.json();
      if (requestedPreview !== previewAs) { again = true; return; }
      acceptData(next);
      try { localStorage.setItem("anchor.contact", data.contact || ""); } catch { /* ignore */ }
      const wasOffline = offline;
      hideOffline();
      if (wasOffline && (!window.anchorCSRF || location.pathname.startsWith("/open/"))) {
        // An offline shell may have missed session setup. Reload online to
        // restore authenticated actions/navigation, or continue a shortcut.
        location.reload();
        return;
      }
      render();
    } catch (err) {
      showOffline("server");
      console.warn("Anchor: load failed", err);
    } finally {
      clearTimeout(timer);
      loading = false;
      if (again) {
        again = false;
        load();
      }
    }
  }

  // ---------- Live updates ----------
  // Anchor sends a short "changed" message the moment an app goes down or
  // comes back (or anchor.yaml is edited), and the page fetches its list
  // again. The regular refresh stays on as a backup.
  let events = null;
  let listened = false;

  function listen() {
    if (!("EventSource" in window) || events) return;
    const es = new EventSource("/api/events");
    events = es;
    es.onmessage = () => load();
    es.onopen = () => {
      // Back after a dropped connection: catch up on anything missed.
      if (listened) load();
      listened = true;
    };
    es.onerror = () => {
      // The browser reconnects on its own after a network blip, but gives
      // up for good if Anchor answers with an error page. Start over later.
      if (es.readyState === EventSource.CLOSED) {
        es.close();
        events = null;
        setTimeout(listen, RETRY_MS);
      }
    };
  }

  function showOffline(kind) {
    offline = true;
    const who = savedContact();
    const ask = who ? `ask ${who}` : "ask the person who set up Anchor";
    $("offline").hidden = false;
    $("offline").dataset.kind = kind;
    $("main-content").hidden = true;
    if (kind === "network") {
      $("offline-title").textContent = "Anchor can't connect";
      $("offline-steps").hidden = window.anchorMode !== "tailscale";
      $("offline-lead").textContent = window.anchorMode === "tailscale" ? "Anchor could not be reached. Check your internet connection and server address. If you use Tailscale for this address, also check that it is connected; the server may be unavailable." : "Check your network connection and the Anchor server address.";
      $("offline-help").textContent = `Still not working after that? Please ${ask}.`;
    } else {
      $("offline-title").textContent = "Anchor isn't responding";
      $("offline-lead").textContent = "You're connected, but Anchor itself is having a problem. It will keep trying on its own.";
      $("offline-steps").hidden = true;
      $("offline-help").textContent = `If this lasts more than a few minutes, please ${ask}.`;
    }
    const t = new Date().toLocaleTimeString([], { hour: "numeric", minute: "2-digit" });
    $("retry-status").textContent = `Trying again automatically. Last try: ${t}.`;
    const btn = $("retry-btn");
    btn.disabled = false;
    btn.textContent = "Try again";
    clearTimeout(retryTimer);
    retryTimer = setTimeout(() => { if (document.visibilityState === "visible") load(); }, RETRY_MS);
  }

  function hideOffline() {
    if (!offline) return;
    offline = false;
    clearTimeout(retryTimer);
    $("offline").hidden = true;
    $("main-content").hidden = false;
  }

  function render() {
    const administration = document.querySelector('.auth-nav a[href="/manage.html"]');
    if (administration) administration.hidden = !data.admin;
    const account = document.querySelector('.auth-nav a[href="/account.html"]');
    if (account) account.hidden = !!data.admin;
    const logout = document.getElementById("sign-out");
    if (logout) logout.hidden = !data.known || window.anchorAuthMethod !== "local";
    document.title = data.title;
    $("brand-name").textContent = data.title;
    $("help-line").textContent = `Something not working? Ask ${data.contact}.`;

    // The shared avatar represents the actual signed-in account, including during previews.
    $("who").hidden = !(data.known || data.login);
    window.anchorAvatar.render($("who"), data.known || data.login ? data : null, "who-pic");

    renderAdmin();

    const notice = $("notice");
    if (data.known && data.notice && data.notice.message) {
      notice.hidden = false;
      notice.classList.toggle("warning", data.notice.style === "warning");
      $("notice-text").textContent = data.notice.message;
    } else {
      notice.hidden = true;
    }

    const cp = $("check-problem");
    cp.hidden = !(data.admin && data.checkProblem);
    cp.textContent = data.checkProblem || "";

    const err = $("config-error");
    err.hidden = !data.configError;
    err.textContent = data.configError ? `Settings problem: ${data.configError}` : "";

    if (!data.known) {
      renderStranger();
      return;
    }
    $("stranger").hidden = true;
    $("greeting").textContent = greeting(firstName(data.viewingAs || data.name));
    $("subtitle").textContent = data.subtitle || "";

    const total = data.sections.reduce((n, s) => n + s.apps.length, 0);
    $("search-wrap").hidden = total < 2;
    // On the regular refresh only the dots change; update them in place so
    // nothing jumps around under the visitor's finger.
    const sig = String(data.reports) + String(data.admin) + String(!!data.viewingAs) + JSON.stringify(data.sections.map((s) => [s.name, s.apps.map((a) => [a.id, a.name, a.url, a.description, a.icon, a.mobile || null, a.ebookLibrary, a.guide || null, a.homeScreen, !!a.requestStatus, a.locked, a.adminOpen, a.alerts, a.maintenance || "", a.status === "down"])]));
    if (sig === lastSig) {
      updateStatuses();
    } else {
      lastSig = sig;
      renderSections();
    }
    applySearch();

    if (total === 0) {
      $("sections").innerHTML = "";
      const p = document.createElement("p");
      p.className = "empty";
      p.textContent = `There's nothing here for you yet. Ask ${data.contact} to add some apps.`;
      $("sections").append(p);
    }
  }

  function renderAdmin() {
    const bar = $("admin-bar");
    if (!data.admin) { bar.hidden = true; return; }
    bar.hidden = false;
    const sel = $("preview-as");
    const me = data.login;
    sel.innerHTML = "";
    for (const p of data.people) {
      const o = document.createElement("option");
      o.value = p.login === me ? "" : p.login;
      o.textContent = p.login === me ? `${p.name || p.login} (you)` : (p.name || p.login);
      sel.append(o);
    }
    sel.value = previewAs;
    $("preview-note").textContent = data.viewingAs ? `You're seeing exactly what ${data.viewingAs} sees.` : "";
  }

  function renderStranger() {
    lastSig = "";
    $("greeting").textContent = "Welcome to Anchor";
    $("subtitle").textContent = "";
    $("sections").innerHTML = "";
    $("search-wrap").hidden = true;
    $("stranger").hidden = false;
    if (data.login) {
      $("stranger-title").textContent = "You're connected, but not set up yet";
      $("stranger-body").textContent = `Tailscale supplied your identity, but Anchor has not matched it to an enabled account. Ask ${data.contact} to link this email to your Anchor account and check that it is enabled:`;
      $("stranger-login").textContent = data.login;
      $("stranger-login").hidden = false;
    } else {
      $("stranger-title").textContent = "Anchor can't tell who you are";
      $("stranger-body").textContent =
        window.anchorMode === "tailscale"
          ? `Sign in through My account with your Anchor password, or connect Tailscale on this device and open Anchor's HTTPS Serve address. Your Tailscale email must be linked to an enabled Anchor account. Ask ${data.contact} if you need the address or an email link.`
          : `Open My account to sign in, then return to the dashboard. If you still need help, ask ${data.contact}.`;
      $("stranger-login").hidden = true;
    }
  }

  function letterColor(name) {
    let h = 0;
    for (const ch of name) h = (h * 31 + ch.charCodeAt(0)) >>> 0;
    return LETTER_COLORS[h % LETTER_COLORS.length];
  }

  function renderSections() {
    const root = $("sections");
    const tpl = $("tile-tpl");
    root.innerHTML = "";
    for (const section of data.sections) {
      const sec = document.createElement("section");
      sec.className = "section";
      const h2 = document.createElement("h2");
      h2.textContent = section.name;
      const grid = document.createElement("div");
      grid.className = "grid";
      sec.append(h2, grid);

      for (const app of section.apps) {
        const node = tpl.content.firstElementChild.cloneNode(true);
        node.dataset.search = `${app.name} ${app.description} ${section.name}`.toLowerCase();
        const link = node.querySelector(".tile-link");
        link.href = safeUrl(app.url);
        link.setAttribute("aria-label", `Open ${app.name}${app.description ? ": " + app.description : ""}`);
        const action = mobileAction(app.mobile);
        if (app.ebookLibrary) {
          link.href = "#";
          link.setAttribute("aria-label", `${app.name}: browse and download ebooks`);
          link.addEventListener("click", (event) => {
            if (app.locked && !app.adminOpen) return;
            event.preventDefault();
            openEbooks(app, event.currentTarget);
          });
        } else if (app.guide) {
          link.href = "#";
          link.setAttribute("aria-label", `${app.name}: ${app.description || "Instructions"}`);
          link.addEventListener("click", (event) => {
            if (app.locked && !app.adminOpen) return;
            event.preventDefault();
            openGuide(app, event.currentTarget);
          });
        } else if (action === "launch") {
          // This is an app/store action, not a link to the server used for
          // health checks and Absorb setup. Keep that address out of the tile
          // link itself, including its screen-reader destination.
          link.href = "#";
          link.setAttribute("aria-label", `Open ${app.name} on this phone`);
          link.addEventListener("click", (event) => {
            if (app.locked && !app.adminOpen) return;
            event.preventDefault();
            launchMobileApp(app.mobile);
          });
        } else if (action === "store") {
          // If this phone has no app deep link, take the tile straight to its
          // store page. The separate App instructions button opens the setup sheet.
          const storeURL = isAndroid ? app.mobile.android : app.mobile.ios;
          if (storeURL) {
            link.href = safeUrl(storeURL);
            link.setAttribute("aria-label", `Get ${app.name} from ${isAndroid ? "Google Play" : "the App Store"}`);
          }
        } else if (action === "setup") {
          link.href = "#";
          link.setAttribute("aria-label", `${app.name}: phone setup instructions`);
          link.addEventListener("click", (event) => {
            if (app.locked && !app.adminOpen) return;
            event.preventDefault();
            openSheet(app, event.currentTarget);
          });
        }
        node.querySelector(".tile-name").textContent = app.name;
        node.querySelector(".tile-desc").textContent = app.description || "";

        const iconBox = node.querySelector(".tile-icon");
        const img = iconBox.querySelector("img");
        iconBox.querySelector(".tile-letter").textContent = app.name.charAt(0).toUpperCase();
        iconBox.style.setProperty("--letter-bg", letterColor(app.name));
        img.addEventListener("error", () => iconBox.classList.add("letter"), { once: true });
        img.src = app.icon;

        node.dataset.id = app.id;
        setStatus(node, app);

        if (app.locked) {
          const note = node.querySelector(".tile-note");
          node.classList.add("locked", app.maintenance ? "maintenance" : "down");
          note.hidden = false;
          note.textContent = app.maintenance
            ? app.maintenance
            : app.alerts
              ? `Not working right now. Alerts to ${data.contact} are enabled.`
              : "Not working right now.";
          if (app.adminOpen) {
            // Admins can still open it, e.g. to fix it (but not while
            // previewing, so the preview matches what that person sees).
            node.classList.add("admin-open");
            note.textContent += " (Locked for others; you can still open it.)";
          } else {
            link.removeAttribute("href");
            link.setAttribute("aria-disabled", "true");
            link.setAttribute("role", "link");
            link.tabIndex = 0;
            link.setAttribute("aria-label", `${app.name} is unavailable: ${note.textContent}`);
            link.addEventListener("click", (e) => e.preventDefault());
          }
        }

        if (data.reports && !app.maintenance) {
          const rep = node.querySelector(".report");
          rep.hidden = false;
          rep.setAttribute("aria-label", `Tell ${data.contact} ${app.name} isn't working`);
          rep.addEventListener("click", (event) => openReport(app, event.currentTarget));
        }

        if (app.mobile) {
          const btn = node.querySelector("button.get-app");
          btn.hidden = false;
          btn.setAttribute("aria-label", `App instructions for ${app.name}`);
          btn.addEventListener("click", (event) => openSheet(app, event.currentTarget));
        }
        if (homeScreenOffered(app) && !app.mobile) {
          // Apps with a phone app offer this inside their app-instructions sheet instead.
          const add = node.querySelector(".add-home");
          add.hidden = false;
          add.href = homeGuideURL(app);
          add.setAttribute("aria-label", `Add ${app.name} to your home screen`);
        }
        if (app.requestStatus) {
          // A separate section at the bottom of the tile; nothing above it moves.
          const extra = node.querySelector(".tile-extra");
          extra.hidden = false;
          extra.querySelector(".req-open").addEventListener("click", (event) => openRequests(app, event.currentTarget));
        }
        grid.append(node);
      }
      root.append(sec);
    }
  }

  function setStatus(tile, app) {
    const st = tile.querySelector(".status");
    st.classList.remove("up", "down", "unknown");
    const state = app.maintenance ? "unknown" : app.status;
    st.classList.add(state);
    tile.querySelector(".status-text").textContent = app.maintenance ? "Paused" : (STATUS_TEXT[app.status] || "");
    if (app.statusDetail) st.title = app.statusDetail; else st.removeAttribute("title");
    if (state === "unknown" && !app.maintenance) st.setAttribute("aria-hidden", "true"); else st.removeAttribute("aria-hidden");
  }

  function updateStatuses() {
    for (const section of data.sections) {
      for (const app of section.apps) {
        const tile = document.querySelector(`.tile[data-id="${CSS.escape(app.id)}"]`);
        if (tile) setStatus(tile, app);
      }
    }
  }

  function safeUrl(u) {
    try {
      const url = new URL(u, location.href);
      return url.protocol === "https:" || url.protocol === "http:" ? url.href : "#";
    } catch { return "#"; }
  }

  // Preview the same shortcut options the person sees when signed in.
  function homeScreenOffered(app) {
    return app.homeScreen;
  }

  function homeGuideURL(app) {
    return `/open/${encodeURIComponent(app.id)}?add=1`;
  }

  function applySearch() {
    const q = $("search").value.trim().toLowerCase();
    let shown = 0;
    for (const sec of document.querySelectorAll("#sections .section")) {
      let secShown = 0;
      for (const tile of sec.querySelectorAll(".tile")) {
        const match = !q || q.split(/\s+/).every((w) => tile.dataset.search.includes(w));
        tile.hidden = !match;
        if (match) secShown++;
      }
      sec.hidden = secShown === 0;
      shown += secShown;
    }
    $("no-results").hidden = !(q && shown === 0);
  }

  // ---------- App instructions sheet ----------
  const isIOS = /iPad|iPhone|iPod/.test(navigator.userAgent) || (navigator.platform === "MacIntel" && navigator.maxTouchPoints > 1);
  const isAndroid = /Android/.test(navigator.userAgent);

  function mobileLaunchURL(mobile) {
    if (!mobile) return "";
    return isAndroid ? mobile.androidApp : isIOS ? mobile.iosApp : "";
  }

  function mobileAction(mobile) {
    if (!mobile?.iosApp && !mobile?.androidApp) return "web";
    if (mobileLaunchURL(mobile)) return "launch";
    if ((isIOS && mobile.ios) || (isAndroid && mobile.android)) return "store";
    return "setup";
  }

  function launchMobileApp(mobile) {
    const launchURL = mobileLaunchURL(mobile);
    if (!launchURL) return;
    cancelMobileLaunch();
    const store = isAndroid ? mobile.android : mobile.ios;
    let switched = false;
    launchVisibility = () => { if (document.hidden) switched = true; };
    document.addEventListener("visibilitychange", launchVisibility);
    window.location.href = launchURL;
    launchTimer = setTimeout(() => {
      cancelMobileLaunch();
      if (store && !switched && !document.hidden) window.location.href = safeUrl(store);
    }, 1600);
  }

  function cancelMobileLaunch() {
    clearTimeout(launchTimer);
    launchTimer = null;
    if (launchVisibility) document.removeEventListener("visibilitychange", launchVisibility);
    launchVisibility = null;
  }

  function storeLink(href, label, primary) {
    const a = document.createElement("a");
    a.href = safeUrl(href);
    a.target = "_blank";
    a.rel = "noopener";
    a.textContent = label;
    if (!primary) a.className = "secondary";
    return a;
  }

  function openSheet(app, trigger) {
    current = app;
    const m = app.mobile;
    $("sheet-title").textContent = `${app.name} on your phone`;
    $("sheet-icon").src = app.icon;
    const links = $("store-links");
    links.innerHTML = "";
    const launchURL = mobileLaunchURL(m);
    const launchWrap = $("open-app-wrap");
    const launchLink = $("open-app-link");
    launchWrap.hidden = !launchURL;
    if (launchURL) {
      launchLink.textContent = m.openLabel || "Open the app";
      launchLink.href = "#";
      launchLink.onclick = (event) => {
        event.preventDefault();
        launchMobileApp(m);
      };
    }
    const ios = m.ios && storeLink(m.ios, "App Store (iPhone)", !isAndroid);
    const android = m.android && storeLink(m.android, "Google Play (Android)", !isIOS);
    const ordered = isAndroid ? [android, ios] : [ios, android];
    for (const l of ordered) if (l) links.append(l);
    $("step-install").hidden = links.childElementCount === 0;
    $("server-addr").textContent = m.server;
    $("sheet-note").textContent = m.note || "";
    $("sheet-note").hidden = !m.note;
    const btn = $("copy-btn");
    btn.textContent = "Copy";
    btn.classList.remove("copied");
    const alt = $("sheet-alt");
    alt.hidden = !homeScreenOffered(app);
    if (!alt.hidden) {
      $("sheet-alt-note").textContent = `Puts ${app.name} on your home screen as an icon. It opens ${app.name} in your browser.`;
      $("sheet-add-home").href = homeGuideURL(app);
    }
    showDialog("app-dialog", trigger);
  }

  function appendGuideSteps(list, steps) {
    list.replaceChildren();
    for (const text of steps || []) {
      const li = document.createElement("li");
      li.textContent = text;
      list.append(li);
    }
  }

  function openGuide(app, trigger) {
    const guide = app.guide;
    $("guide-title").textContent = app.name;
    $("guide-icon").src = app.icon;
    $("guide-intro").textContent = guide.intro || "";
    $("guide-intro").hidden = !guide.intro;
    appendGuideSteps($("guide-steps"), guide.steps);
    $("guide-steps").hidden = !guide.steps?.length;

    const deviceSteps = isAndroid ? guide.android : isIOS ? guide.ios : null;
    const device = $("guide-device");
    device.hidden = !deviceSteps?.length;
    if (!device.hidden) {
      $("guide-device-title").textContent = isAndroid ? "On Android" : "On iPhone or iPad";
      appendGuideSteps($("guide-device-steps"), deviceSteps);
    }
    const note = $("guide-note");
    note.textContent = guide.note || "";
    note.hidden = !guide.note;
    const action = $("guide-action");
    action.textContent = guide.actionLabel || `Open ${app.name}`;
    action.href = safeUrl(app.url);
    showDialog("guide-dialog", trigger);
  }

  async function copyServer() {
    const text = current?.mobile?.server || "";
    const btn = $("copy-btn");
    try {
      await navigator.clipboard.writeText(text);
    } catch {
      // Older browsers: select the text so a long-press copies it.
      const r = document.createRange();
      r.selectNodeContents($("server-addr"));
      const s = getSelection();
      s.removeAllRanges();
      s.addRange(r);
      document.execCommand && document.execCommand("copy");
    }
    btn.textContent = "Copied!";
    btn.classList.add("copied");
    setTimeout(() => { btn.textContent = "Copy"; btn.classList.remove("copied"); }, 2000);
  }

  // ---------- "Not working?" ----------
  let reporting = null;
  let reportOperation = null;

  function openReport(app, trigger) {
    reporting = app;
    $("report-title").textContent = `Tell ${data.contact} that ${app.name} isn't working?`;
    $("report-body").textContent = `Anchor will try to send ${data.contact} a message. If it doesn't go through, please contact them directly.`;
    const send = $("report-send");
    send.style.removeProperty("min-width");
    send.style.removeProperty("min-height");
    send.textContent = `Yes, tell ${data.contact}`;
    send.disabled = false;
    showDialog("report-dialog", trigger);
  }

  async function sendReport() {
    if (!reporting || reportOperation) return;
    const operation = { controller: new AbortController() };
    reportOperation = operation;
    const send = $("report-send");
    const size = send.getBoundingClientRect();
    send.style.setProperty("min-width", `min(100%, ${size.width}px)`);
    send.style.setProperty("min-height", `${size.height}px`);
    send.disabled = true;
    send.textContent = "Sending…";
    let message = `Sorry, that didn't go through. Please text ${data.contact} instead.`;
    try {
      const { body } = await timedJSON("/api/report", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        credentials: "same-origin",
        body: JSON.stringify({ app: reporting.id }),
      }, 20_000, operation);
      if (reportOperation !== operation) return;
      if (body && body.message) message = body.message;
    } catch { /* keep the fallback message */ }
    if (reportOperation !== operation) return;
    reportOperation = null;
    send.disabled = false;
    $("report-dialog").close();
    reporting = null;
    toast(message);
  }

  let toastTimer = null;
  function toast(text) {
    const t = $("toast");
    t.textContent = text;
    t.hidden = false;
    clearTimeout(toastTimer);
    toastTimer = setTimeout(() => { t.hidden = true; }, 5000);
  }

  // ---------- Request status ----------
  // What the family asked for, and whether it's ready. Anchor sends a fixed
  // set of states. The words and colours for them are chosen here, and what
  // came from outside (the titles) is only ever shown as plain text.

  let reqData = null; // the last good answer
  let reqAt = 0;      // when it arrived (this phone's clock, only to count the time since)
  let reqNote = "";
  let reqBusy = false;
  let reqTimer = null;
  let reqOperation = null;

  const { formatDate, requestWording, comingWording, plain, ageText } = window.anchorRequestView;

  // Anchor says how old its list is; this phone only counts the time since
  // it arrived, so a wrong clock on the phone can't make it look fresh.
  function reqAge() {
    return reqData ? (Number(reqData.age) || 0) + (Date.now() - reqAt) / 1000 : 0;
  }

  // One row of either tab: the poster, the title, a small line saying what it
  // is and when it came out ("Movie · 2024"), then what to say about it (see
  // requestWording and comingWording) next to a coloured dot.
  function reqItem(row, say, tone) {
    return window.anchorRequestView.reqItem(row, say, tone, name => featureURL(`/api/requests/poster/${name}`, reqApp));
  }

  function reqRow(row, admin) {
    return window.anchorRequestView.reqRow(row, admin, name => featureURL(`/api/requests/poster/${name}`, reqApp));
  }

  // Which tab is showing. The sheet always opens on Requests.
  let reqTab = "requests";

  function comingMissingText(missing) {
    const shows = missing.includes("shows"), movies = missing.includes("movies");
    if (shows && movies) return "Couldn't check for new episodes or movies right now.";
    if (shows) return "Couldn't check for new episodes right now, so some may be missing.";
    if (movies) return "Couldn't check for new movies right now, so some may be missing.";
    return "";
  }

  // The banner and the status line at the top change as time passes; the list
  // itself doesn't, so it isn't redrawn (and loses its scroll place) for them.
  function renderReqFooter() {
    const parts = [];
    const coming = reqTab === "coming" && reqData && reqData.coming;
    if (coming) {
      const missing = comingMissingText(Array.isArray(coming.missing) ? coming.missing : []);
      if (missing) parts.push(missing);
    } else if (reqData && !reqData.plexOk) {
      parts.push("Can't check Plex right now, so nothing can be marked ready to watch.");
    }
    if (reqData && reqData.stale) parts.push(`Couldn't update just now. This is from ${ageText(reqAge())}.`);
    const banner = $("req-banner");
    banner.hidden = parts.length === 0;
    banner.textContent = parts.join(" ");
    const status = $("req-updated");
    status.textContent = reqBusy ? "Checking…" : reqNote || (reqData ? `Updated ${ageText(reqAge())}` : "");
    status.hidden = !status.textContent;
    $("req-refresh").disabled = reqBusy;
  }

  // Search only appears once there are enough titles to need it, filters as
  // you type, and never asks Anchor for anything.
  const REQ_SEARCH_FROM = 6;

  function applyReqSearch() {
    const rows = [...$("req-list").children];
    const offered = rows.length >= REQ_SEARCH_FROM;
    $("req-search-wrap").hidden = !offered;
    const words = offered ? plain($("req-search").value).split(/\s+/).filter(Boolean) : [];
    let shown = 0;
    for (const li of rows) {
      const match = words.every((w) => li.dataset.search.includes(w));
      li.hidden = !match;
      li.classList.toggle("req-first", match && shown === 0); // no divider above the first one showing
      if (match) shown++;
    }
    $("req-nomatch").hidden = !(rows.length && shown === 0);
  }

  function renderRequests() {
    const admin = !!(data && data.admin && !data.viewingAs);
    const list = $("req-list");
    list.innerHTML = "";
    const empty = $("req-empty");
    // The switch is only there when Anchor sent a Coming up list; if it
    // stops (anchor.yaml changed), fall back to Requests.
    const hasComing = !!(reqData && reqData.coming);
    $("req-setup-note").hidden = !admin || !reqData || hasComing;
    if (!hasComing) reqTab = "requests";
    const onComing = reqTab === "coming";
    $("req-tabs").hidden = !hasComing;
    $("req-tab-requests").setAttribute("aria-pressed", String(!onComing));
    $("req-tab-coming").setAttribute("aria-pressed", String(onComing));
    const label = onComing ? "Search coming up" : "Search your requests";
    $("req-search").placeholder = label;
    $("req-search").setAttribute("aria-label", label);
    if (reqData && onComing) {
      const rows = reqData.coming.rows || [];
      for (const row of rows) list.append(reqItem(row, comingWording(row), "info"));
      empty.hidden = rows.length > 0;
      const unsure = (reqData.coming.missing || []).length > 0; // "nothing due" would be a guess
      empty.textContent = unsure ? "Nothing to show right now." : "Nothing new is due in the next 6 months.";
    } else if (reqData) {
      for (const row of reqData.rows || []) list.append(reqRow(row, admin));
      empty.hidden = (reqData.rows || []).length > 0;
      empty.textContent = "Nothing has been requested yet.";
    } else {
      empty.hidden = reqBusy;
      empty.textContent = reqNote || "Nothing to show yet.";
    }
    applyReqSearch();
    const problems = $("req-admin");
    const adminProblems = admin && reqData && reqData.problems ? reqData.problems : [];
    problems.hidden = adminProblems.length === 0;
    problems.textContent = adminProblems.length ? `For you: ${adminProblems.join(" ")}` : "";
    renderReqFooter();
  }

  async function loadRequests(force) {
    if (reqBusy) return;
    const operation = { controller: new AbortController() };
    reqOperation = operation;
    reqBusy = true;
    reqNote = "";
    renderReqFooter();
    if (!reqData) renderRequests();
    try {
      const { res, body } = await timedJSON(featureURL(force ? "/api/requests/refresh" : "/api/requests", reqApp), {
        method: force ? "POST" : "GET",
        headers: force ? { "Content-Type": "application/json" } : {},
        body: force ? "{}" : undefined,
        cache: "no-store",
        credentials: "same-origin",
      }, 35_000, operation);
      if (reqOperation !== operation) return;
      if (res.status === 404) {
        $("req-dialog").close();
        toast("Request status isn't available right now.");
      } else if (res.ok && Array.isArray(body.rows)) {
        reqData = body;
        reqAt = Date.now();
        reqNote = typeof body.message === "string" ? body.message : "";
      } else {
        reqNote = (body && body.message) || "Couldn't load that right now. Please try again in a minute.";
      }
    } catch {
      if (reqOperation === operation) reqNote = "Couldn't reach Anchor in time. Check that Tailscale is on.";
    } finally {
      if (reqOperation === operation) {
        reqOperation = null;
        reqBusy = false;
        renderRequests();
      }
    }
  }

  function showReqTab(tab) {
    if (reqTab === tab) return;
    reqTab = tab;
    $("req-search").value = ""; // a different list: start the search again
    // A native PageDown animation can continue after scrollTop is reset.
    // Give the new tab its own scroll container so it starts at the first row.
    const previous = $("req-list");
    previous.replaceWith(previous.cloneNode(false));
    renderRequests();
  }

  function featureURL(path, app) {
    return app?.id ? `${path}?app=${encodeURIComponent(app.id)}` : path;
  }

  function openRequests(app, trigger) {
    if (reqApp?.id !== app.id) {
      cancelRequests(); reqData = null; reqAt = 0;
    }
    reqApp = app;
    $("req-title").textContent = "Request status";
    reqNote = "";
    reqTab = "requests";
    $("req-search").value = "";
    renderRequests();
    showDialog("req-dialog", trigger);
    loadRequests(false);
    clearInterval(reqTimer);
    reqTimer = setInterval(() => { reqNote = ""; renderReqFooter(); }, 20_000);
  }

  // ---------- Ebooks ----------
  let ebookData = null;
  let ebookAt = 0;
  let ebookBusy = false;
  let ebookNote = "";
  let ebookOperation = null;
  const EBOOK_PAGE_SIZE = 100;
  let ebookPage = 0;
  let ebookIndexSource = null;
  let ebookIndex = [];

  function ebookRow(book) {
    const li = document.createElement("li");
    li.className = "ebook-row";
    const line = document.createElement("div");
    line.className = "req-line";
    const poster = document.createElement("div");
    poster.className = "req-poster ebook-poster";
    poster.dataset.kind = "book";
    poster.setAttribute("aria-hidden", "true");
    poster.textContent = "▤";
    const img = document.createElement("img");
    img.alt = "";
    img.loading = "lazy";
    img.src = featureURL(`/api/ebooks/${encodeURIComponent(book.id)}/cover`, ebookApp);
    img.addEventListener("load", () => poster.classList.add("has-poster"), { once: true });
    img.addEventListener("error", () => img.remove(), { once: true });
    poster.append(img);

    const body = document.createElement("div");
    body.className = "req-body";
    const name = document.createElement("div");
    name.className = "req-name";
    name.textContent = String(book.title || "Untitled book");
    const details = document.createElement("div");
    details.className = "req-type";
    details.textContent = [book.author, book.year].filter(Boolean).join(" · ");
    const download = document.createElement("a");
    download.className = "btn ebook-download";
    download.href = featureURL(`/api/ebooks/${encodeURIComponent(book.id)}/download`, ebookApp);
    download.textContent = "Download";
    download.setAttribute("aria-label", `Download ${book.title || "book"}`);
    body.append(name);
    if (details.textContent) body.append(details);
    body.append(download);
    line.append(poster, body);
    li.append(line);
    li.dataset.search = plain(`${book.title || ""} ${book.author || ""}`);
    return li;
  }

  function renderEbooks() {
    const list = $("ebooks-list");
    list.innerHTML = "";
    const books = ebookData && Array.isArray(ebookData.rows) ? ebookData.rows : [];
    if (books !== ebookIndexSource) {
      ebookIndexSource = books;
      ebookIndex = books.map(book => ({ book, text: plain(`${book.title || ""} ${book.author || ""}`) }));
      ebookPage = 0;
    }
    const words = plain($("ebooks-search").value).split(/\s+/).filter(Boolean);
    const matches = ebookIndex.filter(row => words.every(word => row.text.includes(word)));
    const pages = Math.ceil(matches.length / EBOOK_PAGE_SIZE);
    ebookPage = Math.max(0, Math.min(ebookPage, pages - 1));
    const start = ebookPage * EBOOK_PAGE_SIZE;
    for (const { book } of matches.slice(start, start + EBOOK_PAGE_SIZE)) list.append(ebookRow(book));
    if (list.firstElementChild) list.firstElementChild.classList.add("req-first");
    $("ebooks-nomatch").hidden = !(books.length > 0 && matches.length === 0);
    $("ebooks-paging").hidden = pages <= 1;
    $("ebooks-page").textContent = matches.length ? `${start + 1}–${Math.min(start + EBOOK_PAGE_SIZE, matches.length)} of ${matches.length} books` : "";
    $("ebooks-prev").disabled = ebookPage === 0;
    $("ebooks-next").disabled = ebookPage + 1 >= pages;
    const empty = $("ebooks-empty");
    empty.hidden = books.length > 0;
    empty.textContent = ebookBusy ? "Loading your books…" : ebookNote || "No ebooks are available right now.";
    const banner = $("ebooks-banner");
    banner.hidden = !(ebookData && ebookData.stale);
    banner.textContent = ebookData && ebookData.stale
      ? "We couldn't update the library just now. Showing the last book list we received."
      : "";
    $("ebooks-updated").textContent = ebookBusy ? "Loading…" : ebookNote || (ebookData ? `Updated ${ageText(Math.floor((Date.now() - ebookAt) / 1000))}` : "");
    $("ebooks-refresh").disabled = ebookBusy;
  }

  function applyEbookSearch() {
    ebookPage = 0;
    renderEbooks();
  }

  function changeEbookPage(delta) {
    ebookPage += delta;
    renderEbooks();
  }

  async function loadEbooks(force) {
    if (ebookBusy) return;
    const operation = { controller: new AbortController() };
    ebookOperation = operation;
    ebookBusy = true;
    ebookNote = "";
    renderEbooks();
    try {
      const { res, body } = await timedJSON(featureURL(force ? "/api/ebooks/refresh" : "/api/ebooks", ebookApp), {
        method: force ? "POST" : "GET",
        headers: force ? { "Content-Type": "application/json" } : {},
        body: force ? "{}" : undefined,
        cache: "no-store",
        credentials: "same-origin",
      }, 100_000, operation);
      if (ebookOperation !== operation) return;
      if (res.status === 404) {
        $("ebooks-dialog").close();
        toast("The ebook library isn't available right now.");
      } else if (res.ok && Array.isArray(body.rows)) {
        ebookData = body;
        ebookAt = Date.now() - Math.max(0, Number(body.age) || 0) * 1000;
      } else {
        ebookNote = body.message || "Couldn't load your books. Please try again in a minute.";
      }
    } catch {
      if (ebookOperation === operation) ebookNote = "Couldn't reach Anchor in time. Check that Tailscale is on.";
    } finally {
      if (ebookOperation === operation) {
        ebookOperation = null;
        ebookBusy = false;
        renderEbooks();
      }
    }
  }

  function openEbooks(app, trigger) {
    if (ebookApp?.id !== app.id) {
      cancelEbooks(); ebookData = null; ebookAt = 0; ebookNote = "";
    }
    ebookApp = app;
    $("ebooks-title").textContent = app.name;
    $("ebooks-search").value = "";
    renderEbooks();
    showDialog("ebooks-dialog", trigger);
    loadEbooks(false);
  }

  // ---------- Install to home screen ----------
  let installPrompt = null;
  window.addEventListener("beforeinstallprompt", (e) => {
    e.preventDefault();
    installPrompt = e;
    $("install-btn").hidden = false;
    const tip = $("samsung-tip");
    if (tip) tip.hidden = true; // the button is enough
  });
  const standalone = matchMedia("(display-mode: standalone)").matches || navigator.standalone;
  if (isIOS && !standalone) $("ios-tip").hidden = false;
  // Samsung Internet never announces that Anchor can be installed (it has its
  // own button in the address bar), so the button above never shows there.
  const samsungTip = $("samsung-tip");
  if (samsungTip && /SamsungBrowser/.test(navigator.userAgent) && !standalone) samsungTip.hidden = false;

  // ---------- Wire up ----------
  document.addEventListener("DOMContentLoaded", () => {
    // Session navigation may arrive after the first dashboard response.
    window.anchorReady?.then(() => { if (data) render(); });
    for (const dialog of document.querySelectorAll("dialog")) {
      dialog.addEventListener("close", () => {
        if (dialog.open) return;
        const trigger = dialogTriggers.get(dialog);
        if (trigger?.isConnected && trigger.getClientRects().length) trigger.focus();
        dialogTriggers.delete(dialog);
      });
      dialog.addEventListener("keydown", (event) => {
        if (event.key !== "Tab") return;
        const controls = [...dialog.querySelectorAll("button, [href], input, select, textarea, [tabindex]")]
          .filter(control => !control.disabled && control.tabIndex >= 0 && control.getClientRects().length && getComputedStyle(control).visibility !== "hidden");
        if (!controls.length) return;
        const index = controls.indexOf(document.activeElement);
        event.preventDefault();
        const next = index < 0 ? (event.shiftKey ? controls.length - 1 : 0) : (index + (event.shiftKey ? -1 : 1) + controls.length) % controls.length;
        controls[next].focus();
        controls[next].scrollIntoView({ block: "nearest", inline: "nearest", behavior: "auto" });
      });
    }
    $("search").addEventListener("input", applySearch);
    $("preview-as").addEventListener("change", (e) => {
      previewAs = e.target.value;
      resetPrivateViews();
      try { sessionStorage.setItem("anchor.previewAs", previewAs); } catch { /* ignore */ }
      $("search").value = "";
      load();
    });
    $("copy-btn").addEventListener("click", copyServer);
    $("retry-btn").addEventListener("click", () => {
      const btn = $("retry-btn");
      btn.disabled = true;
      btn.textContent = "Checking…";
      load();
    });
    window.addEventListener("online", () => load());
    $("req-refresh").addEventListener("click", () => loadRequests(true));
    $("req-search").addEventListener("input", applyReqSearch);
    $("req-tab-requests").addEventListener("click", () => showReqTab("requests"));
    $("req-tab-coming").addEventListener("click", () => showReqTab("coming"));
    $("req-dialog").addEventListener("click", (e) => {
      if (e.target === e.currentTarget) e.currentTarget.close(); // tap outside
    });
    $("req-dialog").addEventListener("close", () => { clearInterval(reqTimer); cancelRequests(); });
    $("ebooks-search").addEventListener("input", applyEbookSearch);
    $("ebooks-prev").addEventListener("click", () => changeEbookPage(-1));
    $("ebooks-next").addEventListener("click", () => changeEbookPage(1));
    $("ebooks-refresh").addEventListener("click", () => loadEbooks(true));
    $("ebooks-dialog").addEventListener("click", (e) => {
      if (e.target === e.currentTarget) e.currentTarget.close();
    });
    $("ebooks-dialog").addEventListener("close", cancelEbooks);
    $("report-dialog").addEventListener("close", cancelReport);
    $("report-send").addEventListener("click", sendReport);
    $("report-cancel").addEventListener("click", () => $("report-dialog").close());
    $("report-dialog").addEventListener("click", (e) => {
      if (e.target === e.currentTarget) e.currentTarget.close();
    });
    $("app-dialog").addEventListener("click", (e) => {
      if (e.target === e.currentTarget) e.currentTarget.close(); // tap outside
    });
    $("install-btn").addEventListener("click", async () => {
      if (!installPrompt) return;
      installPrompt.prompt();
      await installPrompt.userChoice;
      installPrompt = null;
      $("install-btn").hidden = true;
    });
    document.addEventListener("keydown", (e) => {
      if (e.key === "/" && !e.ctrlKey && !e.metaKey && !e.altKey && !e.isComposing &&
          !e.target?.closest?.("input, textarea, select, [contenteditable], dialog[open]") &&
          document.activeElement !== $("search") && !$("search-wrap").hidden) {
        e.preventDefault();
        $("search").focus();
      }
    });

    load();
    listen();
    setInterval(() => { if (document.visibilityState === "visible") load(); }, REFRESH_MS);
    document.addEventListener("visibilitychange", () => { if (document.visibilityState === "visible") load(); });

    if ("serviceWorker" in navigator) {
      // Keep an existing installed shell together across a release. The new
      // worker activates only after its complete shell has been cached.
      if (navigator.serviceWorker.controller) {
        let reloading = false;
        navigator.serviceWorker.addEventListener("controllerchange", () => {
          if (!reloading) { reloading = true; location.reload(); }
        });
      }
      navigator.serviceWorker.register("/sw.js").catch(() => {});
    }
  });
})();

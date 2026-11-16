const { test } = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const vm = require("node:vm");
const path = require("node:path");
function session(status, fetchImpl, storage = new Map(), navigation = false) {
  const requests = [], timers = new Map();
  const redirects = [], nav = { children: [], append(...children) { this.children.push(...children); } };
  const location = {
    href: "http://anchor.test/account.html",
    origin: "http://anchor.test",
    replace(path) { redirects.push(path); },
  };
  const window = {
    fetch: async (input, options) => {
      requests.push({ input, options });
      if (fetchImpl) return fetchImpl(input, options);
      return { ok: true, json: async () => status };
    },
  };
  const elements = new Map();
  const document = {
    body: { dataset: { page: "account" }, prepend(el) { elements.set(el.id, el); } },
    getElementById: id => elements.get(id),
    querySelector: selector => navigation && selector === '.auth-nav' ? nav : null,
    createElement() {
      return { hidden: true, children: [], listeners: {}, setAttribute() {}, append(...children) { this.children.push(...children); },
        replaceChildren(...children) { this.children = children; },
        addEventListener(name, fn) { this.listeners[name] = fn; } };
    },
  };
  const context = {
    window,
    document,
    localStorage: { getItem: key => storage.get(key), setItem: (key, value) => storage.set(key, value) },
    location,
    URL,
    Headers,
    AbortController,
    setTimeout(fn, delay) { timers.set(1, { fn, delay }); return 1; },
    clearTimeout(id) { timers.delete(id); },
    fetch: (...args) => window.fetch(...args),
  };
  vm.runInNewContext(
    fs.readFileSync(path.join(__dirname, "../web/session.js"), "utf8"),
    context,
  );
  return { window, requests, timers, document, nav, redirects };
}

test("shared header sign-out follows the actual method and respects the draft guard", async () => {
  const status = { authenticated: true, method: 'local', csrf: 'private-csrf', mode: 'lan' };
  const h = session(status, undefined, new Map(), true);
  await h.window.anchorReady;
  const logout = h.nav.children.find(child => child.id === 'sign-out');
  assert.equal(logout.hidden, false);
  h.window.anchorRefreshSessionControls({ ...status, method: 'tailscale' });
  assert.equal(logout.hidden, true);
  h.window.anchorRefreshSessionControls(status);
  h.window.anchorBeforeSignOut = () => false;
  await logout.listeners.click();
  assert.equal(h.requests.length, 1, 'cancelling the draft prompt sends no logout');
  h.window.anchorBeforeSignOut = () => true;
  await logout.listeners.click();
  const request = h.requests.at(-1);
  assert.equal(request.input, '/api/auth/logout');
  assert.equal(request.options.headers.get('X-CSRF-Token'), 'private-csrf');
  assert.deepEqual(h.redirects, ['/account.html']);
  assert.equal(logout.disabled, false);
  assert.equal(h.timers.size, 0);
});

test("connection notice dismissal persists across pages without changing authentication", async () => {
  const storage = new Map();
  const status = { insecure: true, csrf: "csrf", authenticated: true, mode: "lan" };
  const first = session(status, undefined, storage);
  assert.equal(await first.window.anchorReady, status);
  const notice = first.document.getElementById("http-note");
  assert.equal(notice.hidden, false);
  notice.children[1].listeners.click();
  assert.equal(notice.hidden, true);
  const next = session(status, undefined, storage);
  assert.equal(await next.window.anchorReady, status);
  assert.equal(next.document.getElementById("http-note"), undefined);
  assert.equal(next.window.anchorCSRF, "csrf");
});
test("session wrapper adds CSRF only to same-origin state changes", async () => {
  const h = session({ csrf: "private-csrf", mode: "lan", authenticated: true });
  await h.window.anchorReady;
  await h.window.fetch("/api/admin/config", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: "{}",
  });
  const request = h.requests.at(-1);
  assert.equal(request.options.headers.get("X-CSRF-Token"), "private-csrf");
  assert.equal(request.options.headers.get("Content-Type"), "application/json");
  await h.window.fetch("https://other.example/api", { method: "POST" });
  assert.equal(h.requests.at(-1).options.headers, undefined);
  await h.window.fetch("/api/me");
  assert.equal(h.requests.at(-1).options.headers, undefined);
});

for (const bodyStalls of [false, true]) {
  test(`session initialization recovers from stalled ${bodyStalls ? 'JSON body' : 'headers'}`, async () => {
    let decoding;
    const bodyStarted = new Promise(resolve => { decoding = resolve; });
    const h = session(null, async (_input, options) => {
      const stalled = () => new Promise((_, reject) => {
        options.signal.addEventListener('abort', () => reject(new Error('aborted')), { once: true });
      });
      if (!bodyStalls) return stalled();
      return { ok: true, json() { decoding(); return stalled(); } };
    });
    if (bodyStalls) await bodyStarted;
    const deadline = [...h.timers.values()].find(timer => timer.delay === 20000);
    assert.ok(deadline, 'session initialization must have a deadline');
    deadline.fn();
    assert.equal(await h.window.anchorReady, null);
    assert.equal(h.window.anchorCSRF, undefined);
    assert.equal(h.timers.size, 0);
  });
}
test("anonymous state does not invent a CSRF token", async () => {
  const h = session({ authenticated: false, csrf: "", mode: "lan" });
  await h.window.anchorReady;
  await h.window.fetch("/api/auth/login", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
  });
  assert.equal(h.requests.at(-1).options.headers.has("X-CSRF-Token"), false);
});

test("URL objects cannot leak CSRF tokens to another origin", async () => {
  const h = session({ csrf: "private-csrf", mode: "lan", authenticated: true });
  await h.window.anchorReady;
  await h.window.fetch(new URL("https://other.example/api"), {
    method: "POST",
  });
  assert.equal(h.requests.at(-1).options.headers, undefined);
  const request = new Request("http://anchor.test/api/admin/config", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
  });
  await h.window.fetch(request);
  assert.equal(
    h.requests.at(-1).options.headers.get("X-CSRF-Token"),
    "private-csrf",
  );
  assert.equal(
    h.requests.at(-1).options.headers.get("Content-Type"),
    "application/json",
  );
});

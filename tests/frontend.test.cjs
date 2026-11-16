const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

// Run actual app functions in a small DOM harness; no production test exports
// or browser libraries are needed. This tests operation lifetimes, not layout.
function harness(fetch, device = {}) {
  const elements = new Map(), timers = new Map(), listeners = new Map();
  let nextTimer = 1;
  function element() {
    return {
      hidden: true, disabled: false, _text: '', value: '', children: [], dataset: {}, scrollTop: 0,
      classList: { add() {}, remove() {}, toggle() {} },
      style: { setProperty(name, value) { this[name] = value; }, removeProperty(name) { delete this[name]; } },
      getBoundingClientRect() { return { width: 160, height: 44 }; },
      setAttribute(name, value) { this[name] = value; }, removeAttribute(name) { delete this[name]; },
      addEventListener() {}, append(...nodes) { this.children.push(...nodes); },
      set textContent(value) { this._text = value; this.children = []; }, get textContent() { return this._text; },
      get firstElementChild() { return this.children[0]; },
      showModal() { this.open = true; }, close() { this.open = false; },
      querySelector() { return null; },
      cloneNode() { return element(); },
      replaceWith(replacement) { for (const [id, node] of elements) if (node === this) elements.set(id, replacement); },
      focus() { context.document.activeElement = this; },
      set innerHTML(_) { this.children = []; },
    };
  }
  function get(id) { if (!elements.has(id)) elements.set(id, element()); return elements.get(id); }
  const context = {
    console, navigator: { userAgent: 'test desktop', platform: '', maxTouchPoints: 0, ...device },
    matchMedia: () => ({ matches: false }),
    document: { getElementById: get, addEventListener(name, listener) { listeners.set(name, listener); }, removeEventListener() {}, querySelector: () => null, querySelectorAll: () => [], createElement: element, createElementNS: element },
    window: { addEventListener() {} },
    sessionStorage: { getItem: () => '' }, localStorage: { getItem: () => '' },
    location: { href: 'https://anchor.test/', pathname: '/', origin: 'https://anchor.test' },
    setTimeout(fn, ms) { const id = nextTimer++; timers.set(id, { fn, ms }); return id; },
    clearTimeout(id) { timers.delete(id); }, clearInterval() {}, setInterval() {},
    fetch, Date, AbortController, URL,
  };
  let source = fs.readFileSync(path.join(__dirname, '..', 'web', 'app.js'), 'utf8');
  assert.match(source, /\}\)\(\);\s*$/);
  source = source.replace(/\}\)\(\);\s*$/, `globalThis.api = {
    loadRequests, loadEbooks, sendReport, openReport, cancelRequests, cancelEbooks, cancelReport,
    acceptData, resetPrivateViews, mobileAction, mobileLaunchURL, launchMobileApp, applyEbookSearch, changeEbookPage, showOffline, load, renderStranger, render, showReqTab, openRequests, openEbooks,
    state() { return { reqBusy, ebookBusy, reporting, reqData, ebookData, ebookPage }; },
    setData(value) { data = value; }
  };})();`);
  vm.createContext(context);
  vm.runInContext(fs.readFileSync(path.join(__dirname, '..', 'web', 'request-view.js'), 'utf8'), context);
  vm.runInContext(fs.readFileSync(path.join(__dirname, '..', 'web', 'avatar.js'), 'utf8'), context);
  vm.runInContext(source, context);
  context.window.location = context.location;
  context.api.setData({ contact: 'admin', admin: false });
  return { api: context.api, get, timers, context, listeners, expire(ms) {
    const entry = [...timers.values()].find(t => t.ms === ms);
    assert.ok(entry, `missing ${ms}ms operation deadline`); entry.fn();
  } };
}

function pendingFetch(_url, options) {
  return new Promise((resolve, reject) => {
    options.signal.addEventListener('abort', () => reject(new Error('aborted')), { once: true });
  });
}
const answer = body => ({ ok: true, status: 200, json: async () => body });

for (const kind of ['requests', 'ebooks']) {
  test(`switching ${kind} tiles scopes requests and ignores the previous source`, async () => {
    const pending = [], signals = [], urls = [];
    const h = harness((url, options) => { urls.push(url); signals.push(options.signal); return new Promise(resolve => pending.push(resolve)); });
    const open = kind === 'ebooks' ? h.api.openEbooks : h.api.openRequests;
    open({ id: 'source-a', name: 'Source A' });
    open({ id: 'source-b', name: 'Source B' });
    assert.ok(signals[0].aborted);
    assert.deepEqual(urls, [`/api/${kind}?app=source-a`, `/api/${kind}?app=source-b`]);
    const rows = kind === 'ebooks' ? [{ id: 'same-book-id', title: 'Book B', format: 'epub' }] : [];
    pending[1](answer({ rows, marker: 'B', plexOk: true }));
    await new Promise(resolve => setImmediate(resolve));
    pending[0](answer({ rows: [], marker: 'A', plexOk: true }));
    await new Promise(resolve => setImmediate(resolve));
    assert.equal((kind === 'ebooks' ? h.api.state().ebookData : h.api.state().reqData).marker, 'B');
    if (kind === 'ebooks') {
      const addresses = [];
      function visit(node) { if (node.href) addresses.push(node.href); if (node.src) addresses.push(node.src); for (const child of node.children) visit(child); }
      visit(h.get('ebooks-list'));
      assert.ok(addresses.includes('/api/ebooks/same-book-id/download?app=source-b'));
      assert.ok(addresses.includes('/api/ebooks/same-book-id/cover?app=source-b'));
    }
  });
}

for (const [kind, timeout] of [['requests', 35000], ['ebooks', 100000], ['report', 20000]]) {
  test(`${kind} timeout restores controls`, async () => {
    const h = harness(pendingFetch);
    if (kind === 'report') h.api.openReport({ id: 'a', name: 'App' });
    const pending = kind === 'requests' ? h.api.loadRequests(false) : kind === 'ebooks' ? h.api.loadEbooks(false) : h.api.sendReport();
    h.expire(timeout); await pending;
    assert.equal(h.api.state().reqBusy, false);
    assert.equal(h.api.state().ebookBusy, false);
    assert.equal(h.get('report-send').disabled, false);
    // Report completion creates a short toast, not a lingering operation timer.
    assert.ok([...h.timers.values()].every(t => t.ms === 5000));
  });
}

test('deadline covers a stalled JSON body after successful headers', { timeout: 2000 }, async () => {
  let started;
  const decoding = new Promise(resolve => { started = resolve; });
  const h = harness(async (_url, options) => ({ ok: true, status: 200, json() {
    started();
    return new Promise((_, reject) => options.signal.addEventListener('abort', () => reject(new Error('body aborted')), { once: true }));
  } }));
  const pending = h.api.loadRequests(false);
  await decoding;
  h.expire(35000); await pending;
  assert.equal(h.api.state().reqBusy, false);
  assert.match(h.get('req-updated').textContent, /in time/);
});

test('identity/access changes abort in-flight operations and remove private state', async () => {
  const pending = [], signals = [];
  const h = harness((_url, options) => { signals.push(options.signal); return new Promise(resolve => pending.push(resolve)); });
  const admin = { scope: 'config-a-person-a', login: 'admin', known: true, admin: true, sections: [], contact: 'help' };
  h.api.acceptData(admin);
  h.get('req-dialog').showModal(); h.get('ebooks-dialog').showModal();
  const req = h.api.loadRequests(false), books = h.api.loadEbooks(false);
  h.api.openReport({ id: 'private', name: 'Private app' }); const report = h.api.sendReport();
  h.get('server-addr').textContent = 'private-server';
  h.get('req-admin').textContent = 'private-diagnostic';
  h.api.acceptData({ ...admin, scope: 'config-b-person-b', login: '', known: false, admin: false });
  assert.ok(signals.every(signal => signal.aborted));
  pending.forEach(resolve => resolve(answer({ rows: [{ id: 'old-private-book' }], message: 'stale success' })));
  await Promise.all([req, books, report]);
  assert.equal(h.api.state().reqData, null); assert.equal(h.api.state().ebookData, null);
  for (const id of ['req-dialog', 'ebooks-dialog', 'report-dialog']) assert.equal(h.get(id).open, false);
  for (const id of ['req-list', 'ebooks-list', 'req-admin', 'server-addr']) {
    assert.equal(h.get(id).children.length, 0); assert.equal(h.get(id).textContent, '');
  }
  assert.equal(h.get('toast').hidden, true);
});

test('unchanged access preserves cached data; new config/effective person clears it', async () => {
  const h = harness(async () => answer({ rows: [], marker: 'kept' }));
  const me = { scope: 'a', login: 'admin', known: true, admin: true, viewingAs: 'Same name', sections: [] };
  h.api.acceptData(me); await h.api.loadEbooks(false);
  h.api.acceptData({ ...me }); assert.equal(h.api.state().ebookData.marker, 'kept');
  h.api.acceptData({ ...me, scope: 'different-person-with-same-name' }); assert.equal(h.api.state().ebookData, null);
});

for (const [device, expected] of [[{}, 'setup'], [{ userAgent: 'iPhone' }, 'launch'], [{ userAgent: 'Android' }, 'store'], [{ platform: 'MacIntel', maxTouchPoints: 5 }, 'launch']]) {
  test(`native-app tile action for ${JSON.stringify(device)}`, () => {
    const h = harness(pendingFetch, device);
    assert.equal(h.api.mobileAction({ iosApp: 'absorb://', ios: 'https://store.test/ios', android: 'https://store.test/android' }), expected);
    assert.equal(h.api.mobileAction({ ios: 'https://store.test/' }), 'web');
  });
}

test('permission reset cancels a pending app-store fallback', () => {
  const h = harness(pendingFetch, { userAgent: 'iPhone' });
  h.api.launchMobileApp({ iosApp: 'absorb://', ios: 'https://store.test/' });
  assert.ok([...h.timers.values()].some(t => t.ms === 1600));
  h.api.resetPrivateViews();
  assert.ok([...h.timers.values()].every(t => t.ms !== 1600));
});

test('large ebook catalog has bounded DOM and searches the entire library', async () => {
  const rows = Array.from({ length: 20000 }, (_, i) => ({ id: String(i), title: `Book ${i}`, author: 'Writer' }));
  rows[19999].title = 'Unique final title';
  const h = harness(async () => answer({ rows })); await h.api.loadEbooks(false);
  assert.equal(h.get('ebooks-list').children.length, 100);
  assert.equal(h.get('ebooks-page').textContent, '1–100 of 20000 books');
  h.api.changeEbookPage(1); assert.equal(h.get('ebooks-page').textContent, '101–200 of 20000 books');
  h.api.changeEbookPage(999); assert.equal(h.api.state().ebookPage, 199);
  h.get('ebooks-search').value = 'unique final'; h.api.applyEbookSearch();
  assert.equal(h.get('ebooks-list').children.length, 1);
  assert.equal(h.api.state().ebookPage, 0); assert.equal(h.get('ebooks-paging').hidden, true);
  h.get('ebooks-search').value = 'no match'; h.api.applyEbookSearch();
  assert.equal(h.get('ebooks-list').children.length, 0); assert.equal(h.get('ebooks-nomatch').hidden, false);
});

for (const kind of ['requests', 'ebooks']) {
  test(`${kind} close/reopen ignores the old response`, async () => {
    const resolvers = [], signals = [];
    const h = harness((_url, options) => { signals.push(options.signal); return new Promise(resolve => resolvers.push(resolve)); });
    const load = kind === 'requests' ? h.api.loadRequests : h.api.loadEbooks;
    const cancel = kind === 'requests' ? h.api.cancelRequests : h.api.cancelEbooks;
    const old = load(false); cancel(); assert.equal(signals[0].aborted, true);
    const current = load(false);
    resolvers[1](answer({ rows: [], marker: 'new', plexOk: true })); await current;
    resolvers[0](answer({ rows: [], marker: 'old', plexOk: true })); await old;
    assert.equal((kind === 'requests' ? h.api.state().reqData : h.api.state().ebookData).marker, 'new');
  });
}

test('report close/reopen ignores an old success and preserves the new selection', async () => {
  const resolvers = [], bodies = [];
  const h = harness((_url, options) => { bodies.push(JSON.parse(options.body)); return new Promise(resolve => resolvers.push(resolve)); });
  h.api.openReport({ id: 'first', name: 'First' }); const old = h.api.sendReport();
  h.api.cancelReport(); h.api.openReport({ id: 'second', name: 'Second' }); const current = h.api.sendReport();
  resolvers[0](answer({ message: 'old result' })); await old;
  assert.equal(h.api.state().reporting.id, 'second'); assert.equal(h.get('report-send').disabled, true);
  resolvers[1](answer({ message: 'new result' })); await current;
  assert.deepEqual(bodies, [{ app: 'first' }, { app: 'second' }]);
  assert.equal(h.get('toast').textContent, 'new result'); assert.equal(h.get('report-send').disabled, false);
});

test('report sending preserves its measured size and resets it when reopened', async () => {
  const h = harness(pendingFetch);
  h.api.openReport({ id: 'first', name: 'First' });
  const pending = h.api.sendReport();
  assert.equal(h.get('report-send').style['min-width'], 'min(100%, 160px)');
  assert.equal(h.get('report-send').style['min-height'], '44px');
  h.api.cancelReport(); await pending;
  h.api.openReport({ id: 'next', name: 'Next' });
  assert.equal(h.get('report-send').style['min-width'], undefined);
  assert.equal(h.get('report-send').style['min-height'], undefined);
});

test('offline recovery instructions match the deployment mode', () => {
  const h = harness(pendingFetch);
  for (const mode of ['lan', 'proxy', 'tailscale', undefined]) {
    h.context.window.anchorMode = mode;
    h.api.showOffline('network');
    assert.equal(h.get('offline-steps').hidden, mode !== 'tailscale', String(mode));
    assert.match(h.get('offline-lead').textContent, mode === 'tailscale' ? /Tailscale/ : /network connection/);
  }
  h.api.showOffline('server');
  assert.equal(h.get('offline-steps').hidden, true);
});

test('signed-out guidance uses the deployment mode and hides stale administration navigation', () => {
  const h = harness(pendingFetch);
  h.api.setData({ title: 'Anchor', contact: 'the administrator', known: false, admin: false, login: '', sections: [] });
  for (const mode of ['lan', 'proxy', 'tailscale', undefined]) {
    h.context.window.anchorMode = mode;
    h.api.renderStranger();
    assert.match(h.get('stranger-body').textContent, mode === 'tailscale' ? /Tailscale/ : /My account/);
  }
  const adminLink = h.get('synthetic-admin-nav'); adminLink.hidden = false;
  const accountLink = h.get('synthetic-account-nav'); accountLink.hidden = true;
  h.context.document.querySelector = selector => selector.includes('/manage.html') ? adminLink : selector.includes('/account.html') ? accountLink : null;
  h.api.render();
  assert.equal(adminLink.hidden, true);
  assert.equal(accountLink.hidden, false, 'signed-out visitors retain a sign-in route');
});

test('changing media tabs resets list position but selecting the same tab preserves it', async () => {
  const h = harness(async () => answer({ rows: [], plexOk: true, coming: { rows: [], missing: [] } }));
  await h.api.loadRequests(false);
  h.get('req-list').scrollTop = 175;
  h.api.showReqTab('coming');
  assert.equal(h.get('req-list').scrollTop, 0);
  h.get('req-list').scrollTop = 50;
  h.api.showReqTab('coming');
  assert.equal(h.get('req-list').scrollTop, 50);
});

test('late session navigation respects the latest dashboard access', async () => {
  const h = harness(pendingFetch);
  h.api.setData({ title: 'Anchor', contact: 'the administrator', known: false, admin: false, login: '', sections: [] });
  let ready;
  h.context.window.anchorReady = new Promise(resolve => { ready = resolve; });
  h.listeners.get('DOMContentLoaded')();
  const adminLink = h.get('late-admin-navigation');
  const accountLink = h.get('late-account-navigation');
  adminLink.hidden = false;
  h.context.document.querySelector = selector => selector.includes('/manage.html') ? adminLink : selector.includes('/account.html') ? accountLink : null;
  ready({ admin: true });
  await h.context.window.anchorReady;
  assert.equal(adminLink.hidden, true);
  assert.equal(accountLink.hidden, false);
});

test('slash search shortcut preserves typing, dialog focus and modified keys', () => {
  const h = harness(pendingFetch);
  h.listeners.get('DOMContentLoaded')();
  h.get('search-wrap').hidden = false;
  const other = {};
  for (const event of [
    { target: { closest: () => ({}) } }, // a search field, editable text or open dialog
    { ctrlKey: true }, { metaKey: true }, { altKey: true }, { isComposing: true },
  ]) {
    h.context.document.activeElement = other;
    let prevented = false;
    h.listeners.get('keydown')({ key: '/', preventDefault() { prevented = true; }, ...event });
    assert.equal(prevented, false);
    assert.equal(h.context.document.activeElement, other);
  }
  let prevented = false;
  h.listeners.get('keydown')({ key: '/', preventDefault() { prevented = true; } });
  assert.equal(prevented, true);
  assert.equal(h.context.document.activeElement, h.get('search'));
});

test('offline shell recovery reloads to bootstrap a missing session before writes', async () => {
  const me = { scope: 'same', login: 'member', known: true, admin: false, name: 'Member', sections: [], contact: 'help', title: 'Anchor' };
  const h = harness(async () => answer(me));
  let reloads = 0;
  h.context.location.reload = () => { reloads++; };
  h.api.showOffline('network');
  await h.api.load();
  assert.equal(reloads, 1);
});

test('ordinary reconnect preserves the live session and private dialog data', async () => {
  const me = { scope: 'same', login: 'member', known: true, admin: false, name: 'Member', sections: [], contact: 'help', title: 'Anchor' };
  const h = harness(async url => answer(url === '/api/me' ? me : { rows: [], marker: 'cached' }));
  let reloads = 0;
  h.context.location.reload = () => { reloads++; };
  h.context.window.anchorCSRF = 'synthetic-live-csrf';
  h.api.acceptData(me);
  await h.api.loadEbooks(false);
  h.get('ebooks-dialog').showModal();
  h.api.showOffline('network');
  await h.api.load();
  assert.equal(reloads, 0);
  assert.equal(h.api.state().ebookData.marker, 'cached');
  assert.equal(h.get('ebooks-dialog').open, true);
  assert.equal(h.get('offline').hidden, true);
});

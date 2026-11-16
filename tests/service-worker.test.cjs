const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function worker({ offline = false, installFailure = false } = {}) {
  const handlers = {}, deleted = [], requests = [], network = [], added = [], entries = new Map();
  let activated = false, claimed = false;
  const context = {
    URL, Request: class { constructor(url, options) { this.url = url; this.cache = options.cache; } },
    Response: { error: () => 'network-error' }, location: { origin: 'https://anchor.test' },
    self: { addEventListener(name, handler) { handlers[name] = handler; }, skipWaiting() { activated = true; }, clients: { claim() { claimed = true; } } },
    caches: {
      keys: async () => ['anchor-shell-old', 'another-app-cache', 'anchor-shell-test-digest'],
      delete: async name => { deleted.push(name); return true; },
      open: async name => {
        requests.push(name);
        return {
          async addAll(paths) {
            added.push(...paths);
            if (installFailure) throw new Error('interrupted installation');
            paths.forEach(p => entries.set(p.url, `shell:${p.url}`));
          },
          match: async name => entries.get(name),
        };
      },
    },
    fetch: async request => { network.push(request.url); if (offline) throw new Error('offline'); return 'network-response'; },
  };
  const source = fs.readFileSync(path.join(__dirname, '..', 'web', 'sw.js'), 'utf8').replaceAll('__ANCHOR_SHELL_VERSION__', 'test-digest');
  vm.runInNewContext(source, context);
  return {
    deleted, added, requests, network,
    get activated() { return activated; }, get claimed() { return claimed; },
    lifecycle(name) { let pending; handlers[name]({ waitUntil(promise) { pending = promise; } }); return pending; },
    fetch(url, mode = 'cors', method = 'GET') {
      let response;
      handlers.fetch({ request: { url: new URL(url, 'https://anchor.test').href, method, mode }, respondWith(value) { response = value; } });
      return response;
    },
  };
}

test('worker installs a complete uncached shell before activating', async () => {
  const w = worker(); await w.lifecycle('install');
  assert.equal(w.activated, true); assert.equal(w.added.length, 8);
  assert.ok(w.added.some(p => p.url === '/avatar.js'));
  assert.ok(w.added.some(p => p.url === '/request-view.js'));
  assert.ok(w.added.every(p => p.cache === 'reload'));
  await w.lifecycle('activate');
  assert.deepEqual(w.deleted, ['anchor-shell-old']); assert.equal(w.claimed, true);
});

test('interrupted shell installation cannot activate the new worker', async () => {
  const w = worker({ installFailure: true });
  await assert.rejects(w.lifecycle('install'), /interrupted/);
  assert.equal(w.activated, false); assert.equal(w.claimed, false); assert.deepEqual(w.deleted, []);
});

test('shell resources stay in their release cache while shortcuts always reach Anchor', async () => {
  const w = worker(); await w.lifecycle('install');
  assert.equal(await w.fetch('/app.js'), 'shell:/app.js');
  assert.equal(await w.fetch('/?view=test', 'navigate'), 'shell:/');
  assert.deepEqual(w.network, []);
  assert.equal(await w.fetch('/open/private', 'navigate'), 'network-response');
  assert.deepEqual(w.network, ['https://anchor.test/open/private']);
});

test('protected APIs, non-shell resources and other origins bypass the worker', async () => {
  const w = worker();
  for (const [url, method] of [['/api/me', 'GET'], ['/api/ebooks/book/cover', 'GET'], ['/api/report', 'POST'], ['/icons/book', 'GET'], ['https://other.test/', 'GET']]) {
    assert.equal(w.fetch(url, 'cors', method), undefined);
  }
  assert.deepEqual(w.requests, []); assert.deepEqual(w.network, []);
});

test('offline shortcut navigation gets the public recovery shell, never a saved authorized page', async () => {
  const w = worker({ offline: true }); await w.lifecycle('install');
  assert.equal(await w.fetch('/open/private', 'navigate'), 'shell:/');
  assert.equal(await w.fetch('/style.css'), 'shell:/style.css');
  assert.equal(w.fetch('/api/ebooks'), undefined);
});

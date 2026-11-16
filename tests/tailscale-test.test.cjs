const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function harness(protocol, report) {
  let request, timer, cleared = false;
  const context = {
    window: {}, AbortController, URL, Error,
    location: { protocol, origin: protocol + '//anchor.example.ts.net:8443' },
    setTimeout(fn) { timer = fn; return 1; }, clearTimeout() { cleared = true; },
    fetch(url, options) { request = { url, options }; return Promise.resolve({ ok: true, json: async () => structuredClone(report) }); },
  };
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, '../web/tailscale-test-view.js'), 'utf8'), context);
  return { context, read: context.window.anchorTailscaleTest.read, request: () => request, cleared: () => cleared, expire: () => timer() };
}

test('Tailscale test excludes password cookies and sends a run token only over HTTPS', async () => {
  const secure = harness('https:', { state: 'verified', name: 'Member' });
  assert.equal((await secure.read('a&b')).state, 'verified');
  assert.equal(secure.request().options.credentials, 'omit');
  assert.equal(secure.request().options.cache, 'no-store');
  assert.equal(secure.request().url, '/api/tailscale-test?ticket=a%26b');
  assert.equal(secure.cleared(), true);
  const insecure = harness('http:', { state: 'verified', sections: [{ name: 'Private' }] });
  const report = await insecure.read('do-not-submit');
  assert.equal(report.state, 'insecure');
  assert.equal(report.sections, undefined);
  assert.equal(insecure.request().url, '/api/tailscale-test');
});

test('Tailscale test deadline covers response decoding and clears after a failure', async () => {
  const h = harness('https:', {});
  let decoding;
  const started = new Promise(resolve => { decoding = resolve; });
  h.context.fetch = async (_url, options) => ({ ok: true, json: () => { decoding(); return new Promise((_, reject) => options.signal.addEventListener('abort', () => reject(new Error('aborted')))); } });
  const pending = h.read();
  await started;
  h.expire();
  await assert.rejects(pending, /aborted/);
  assert.equal(h.cleared(), true);
});

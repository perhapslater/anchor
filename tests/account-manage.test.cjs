const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

// Execute the actual account/admin request and form lifecycle with a stalled
// synthetic response. No browser globals or production test hooks are added.
function pageHarness(page, bodyStalls) {
  const timers = new Map(), elements = new Map();
  const button = { disabled: false, addEventListener() {} };
  let decoding;
  const bodyStarted = new Promise(resolve => { decoding = resolve; });
  function get(id) {
    if (!elements.has(id)) elements.set(id, {
      hidden: true, disabled: false, textContent: '', classList: { toggle() {} },
      setAttribute(name, value) { this[name] = value; }, removeAttribute(name) { delete this[name]; },
      addEventListener() {}, scrollIntoView() {}, querySelectorAll: () => [], querySelector: () => button,
    });
    return elements.get(id);
  }
  const context = {
    AbortController, Error,
    window: { anchorReady: new Promise(() => {}), addEventListener() {} },
    document: { getElementById: get, addEventListener() {}, querySelectorAll: selector => selector === 'button' ? [button] : selector.includes('input') ? [button, get('draft'), get('already-disabled')] : [] },
    setTimeout(fn, delay) { timers.set(1, { fn, delay }); return 1; },
    clearTimeout(id) { timers.delete(id); },
    fetch(_url, options) {
      const stalled = () => new Promise((_, reject) => {
        options.signal.addEventListener('abort', () => reject(new Error('aborted')), { once: true });
      });
      if (!bodyStalls) return stalled();
      return Promise.resolve({ ok: true, json() { decoding(); return stalled(); } });
    },
  };
  let source = fs.readFileSync(path.join(__dirname, '..', 'web', `${page}.js`), 'utf8');
  source = source.replace(/\}\)\(\);\s*$/, `globalThis.api = ${page === 'manage' ? '{ api, work, serveCommands, connectionSummary, tailscaleReadiness }' : '{ send, submit }'};})();`);
  vm.runInNewContext(source, context);
  return { context, button, timers, get, bodyStarted };
}

test('management save protects drafts and restores previous disabled states', async () => {
  const h = pageHarness('manage', false);
  h.get('draft').value = 'Keep this draft';
  h.get('already-disabled').disabled = true;
  let finish;
  const pending = h.context.api.work(() => new Promise(resolve => { finish = resolve; }));
  assert.equal(h.get('draft').disabled, true, 'fields must not accept edits that the pending save would discard');
  assert.equal(h.get('content')['aria-busy'], 'true');
  finish(); await pending;
  assert.equal(h.get('draft').disabled, false);
  assert.equal(h.get('already-disabled').disabled, true);
  assert.equal(h.get('draft').value, 'Keep this draft');
  assert.equal(h.get('content')['aria-busy'], undefined);
});

test('Serve commands use edited ports and the selected execution context', () => {
  const { api } = pageHarness('manage', false).context;
  const host = api.serveCommands('host', '18082', '8444', '');
  assert.equal(host.setup, 'tailscale serve --bg --https=8444 http://127.0.0.1:18082');
  assert.equal(host.status, 'tailscale serve status');
  assert.equal(host.environment, 'ANCHOR_MODE=tailscale\nANCHOR_LISTEN=127.0.0.1:18082');
  const container = api.serveCommands('container', '9000', '443', 'tailscale-anchor');
  assert.equal(container.setup, 'docker exec tailscale-anchor tailscale serve --bg --https=443 http://127.0.0.1:9000');
  assert.equal(container.status, 'docker exec tailscale-anchor tailscale serve status');
  assert.equal(api.serveCommands('dev', '18083', '8445', '').setup, '.\\dev.ps1 -Port 18083 -Tailscale -TailscalePort 8445');
});

test('Serve command builder rejects shell injection and unusable port values', () => {
  const { serveCommands } = pageHarness('manage', false).context.api;
  for (const name of ['ts; echo unsafe', 'ts$(whoami)', '`whoami`', '-it', 'ts\ncommand', 'ts other', 'ts"name'])
    assert.throws(() => serveCommands('container', '8080', '8443', name), /container name/);
  for (const port of ['', '0', '65536', '1.5', '1e3', '8080; echo unsafe', '192.168.1.1:8080']) {
    assert.throws(() => serveCommands('host', port, '8443', ''), /port numbers/);
    assert.throws(() => serveCommands('host', '8080', port, ''), /port numbers/);
  }
  assert.throws(() => serveCommands('dev', '80', '8443', ''), /at least 1024/);
});

test('connection summary distinguishes deployment mode, browser transport and sign-in', () => {
  const { connectionSummary } = pageHarness('manage', false).context.api;
  const configured = connectionSummary({ mode: 'tailscale', method: 'local' }, 'http://127.0.0.1:8080');
  assert.equal(configured.mode, 'Tailscale Serve');
  assert.equal(configured.transport, 'HTTP · unencrypted');
  assert.equal(configured.signin, 'Local password');
  const verified = connectionSummary({ mode: 'tailscale', method: 'tailscale' }, 'https://anchor.example.ts.net');
  assert.equal(verified.transport, 'HTTPS · encrypted');
  assert.equal(verified.signin, 'Tailscale identity');
  assert.equal(connectionSummary({ mode: 'proxy', method: 'local' }, 'https://anchor.test').mode, 'HTTPS reverse proxy');
  assert.equal(connectionSummary({ mode: 'lan', method: 'local' }, 'http://anchor.test').mode, 'HTTP LAN');
});

test('Tailscale diagnostics distinguish linked emails, deployment mode, HTTPS and actual sign-in', () => {
  const { tailscaleReadiness } = pageHarness('manage', false).context.api;
  const local = Object.freeze({ authenticated: true, admin: true, mode: 'tailscale', method: 'local' });
  const linked = Object.freeze([Object.freeze({ enabled: true, tailscale: 'admin@example.test' })]);
  assert.equal(tailscaleReadiness(local, linked).state, 'linked');
  assert.equal(tailscaleReadiness(local, []).state, 'pending');
  assert.equal(tailscaleReadiness(local, [{ enabled: false, tailscale: 'paused@example.test' }]).state, 'pending');
  assert.equal(tailscaleReadiness({ ...local, mode: 'lan' }, linked).state, 'mode');
  assert.equal(tailscaleReadiness({ ...local, mode: 'proxy' }, linked).state, 'mode');
  assert.equal(tailscaleReadiness(local, linked, 'https://anchor.test', 'unlinked@example.test').state, 'unlinked');
  assert.equal(tailscaleReadiness({ ...local, method: 'tailscale' }, linked, 'http://localhost').state, 'insecure');
  assert.equal(tailscaleReadiness({ ...local, method: 'tailscale', user: { name: 'Alex' } }, linked, 'https://anchor.test').state, 'verified');
  assert.match(tailscaleReadiness({ ...local, method: 'tailscale', user: { name: 'Alex' } }, linked, 'https://anchor.test').text, /Alex on this device/);
  assert.equal(tailscaleReadiness({ ...local, method: 'tailscale', simulated: true }, linked, 'https://anchor.test').state, 'simulated');
  assert.equal(tailscaleReadiness({ authenticated: false }, linked).state, 'expired');
  assert.equal(local.method, 'local');
});

for (const page of ['account', 'manage']) {
  for (const bodyStalls of [false, true]) {
    test(`${page} stalled ${bodyStalls ? 'JSON body' : 'headers'} times out and restores controls`, async () => {
      const h = pageHarness(page, bodyStalls);
      h.get('draft').value = 'Unsaved synthetic form value';
      const api = h.context.api;
      const pending = page === 'manage'
        ? api.work(() => api.api('/api/admin/config', {}))
        : api.submit(h.get('login'), () => api.send('/api/auth/login', {}));
      assert.equal(h.button.disabled, true);
      if (bodyStalls) await h.bodyStarted;
      const deadline = [...h.timers.values()].find(timer => timer.delay === 20000);
      assert.ok(deadline, 'the request must have a deadline');
      deadline.fn();
      await pending;
      assert.equal(h.button.disabled, false);
      assert.match(h.get('message').textContent, /respond in time/);
      assert.equal(h.get('draft').value, 'Unsaved synthetic form value');
      assert.equal(h.timers.size, 0);
    });
  }
}

test('application ID HTML pattern is valid in browser Unicode Sets mode', () => {
  const html = fs.readFileSync(path.join(__dirname, '..', 'web', 'manage.html'), 'utf8');
  const pattern = html.match(/id="app-id"[\s\S]*?pattern="([^"]+)"/)[1];
  const validID = new RegExp(`^(?:${pattern})$`, 'v');
  for (const id of ['a', 'plex', 'my-service', 'app_123', 'a'.repeat(64)])
    assert.equal(validID.test(id), true, id);
  for (const id of ['', '-app', '_app', 'App', 'contains space', 'app/slash', 'a'.repeat(65)])
    assert.equal(validID.test(id), false, id);
});

for (const page of ['account', 'manage']) {
  test(`${page} completed responses retain server feedback and clear deadlines`, async () => {
    const h = pageHarness(page, false);
    const send = page === 'manage' ? h.context.api.api : h.context.api.send;
    h.context.fetch = async (_url, options) => {
      assert.equal(options.signal.aborted, false);
      return { ok: true, json: async () => ({ message: 'Saved.' }) };
    };
    assert.equal((await send('/api/test', {})).message, 'Saved.');
    assert.equal(h.timers.size, 0);
    h.context.fetch = async () => ({ ok: false, status: 409, json: async () => ({ message: 'Settings changed; reload first.' }) });
    await assert.rejects(send('/api/test', {}), /Settings changed; reload first/);
    assert.equal(h.timers.size, 0);
  });
}

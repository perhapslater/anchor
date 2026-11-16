const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

function harness() {
  const document = {};
  function element(tag) {
    const listeners = new Map();
    return {
      tag, children: [], attributes: {}, isConnected: true, open: false, disabled: false,
      append(...children) { this.children.push(...children); },
      setAttribute(name, value) { this.attributes[name] = value; },
      addEventListener(type, handler) { if (!listeners.has(type)) listeners.set(type, []); listeners.get(type).push(handler); },
      fire(type, values = {}) { const event = { target: this, defaultPrevented: false, preventDefault() { this.defaultPrevented = true; }, ...values }; for (const handler of listeners.get(type) || []) handler(event); return event; },
      focus() { document.activeElement = this; },
      showModal() { this.open = true; },
      close() { this.open = false; this.fire('close'); },
      getBoundingClientRect() { return { left: 10, right: 100, top: 10, bottom: 100 }; },
    };
  }
  document.body = element('body'); document.createElement = element;
  const opener = element('button'); opener.focus();
  const window = {};
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, '../web/confirm.js'), 'utf8'), { document, window });
  function get(id, root = document.body) { if (root.id === id) return root; for (const child of root.children) { const found = get(id, child); if (found) return found; } }
  return { document, opener, confirm: window.anchorConfirm, get };
}

test('Anchor confirmation uses safe text, focuses cancel, and requires explicit approval', async () => {
  const h = harness();
  const pending = h.confirm({ title: 'Remove <script>app</script>?', message: 'A & B', confirmLabel: 'Remove application', cancelLabel: 'Keep application', danger: true });
  assert.equal(h.get('anchor-confirm').attributes.role, 'alertdialog');
  assert.equal(h.get('anchor-confirm-title').textContent, 'Remove <script>app</script>?');
  assert.equal(h.get('anchor-confirm-detail').textContent, 'A & B');
  assert.equal(h.document.activeElement, h.get('anchor-confirm-cancel'));
  assert.equal(h.get('anchor-confirm-accept').textContent, 'Remove application');
  h.get('anchor-confirm-accept').fire('click');
  assert.equal(await pending, true);
  assert.equal(h.get('anchor-confirm').open, false);
  assert.equal(h.document.activeElement, h.opener);
});

test('Anchor confirmation cancels safely with button, Escape, backdrop or external close', async () => {
  const h = harness();
  for (const cancel of [() => h.get('anchor-confirm-cancel').fire('click'), () => h.get('anchor-confirm').fire('cancel'), () => h.get('anchor-confirm').fire('click', { clientX: 0, clientY: 0 }), () => h.get('anchor-confirm').close()]) {
    const pending = h.confirm({ message: 'Discard a draft?' });
    cancel();
    assert.equal(await pending, false);
    assert.equal(h.document.activeElement, h.opener);
  }
});

test('A concurrent confirmation cannot replace the pending action', async () => {
  const h = harness(), pending = h.confirm({ title: 'First action' });
  assert.equal(await h.confirm({ title: 'Second action' }), false);
  assert.equal(h.get('anchor-confirm-title').textContent, 'First action');
  h.get('anchor-confirm-cancel').fire('click');
  assert.equal(await pending, false);
});

test('Tab and Shift+Tab keep keyboard focus inside the confirmation', async () => {
  const h = harness(), pending = h.confirm();
  const dialog = h.get('anchor-confirm');
  assert.equal(dialog.fire('keydown', { key: 'Tab', shiftKey: true }).defaultPrevented, true);
  assert.equal(h.document.activeElement, h.get('anchor-confirm-accept'));
  dialog.fire('keydown', { key: 'Tab', shiftKey: false });
  assert.equal(h.document.activeElement, h.get('anchor-confirm-cancel'));
  h.get('anchor-confirm-cancel').fire('click');
  await pending;
});

test('Administration does not invoke browser confirmation or unload prompts', () => {
  const source = fs.readFileSync(path.join(__dirname, '../web/manage.js'), 'utf8');
  assert.doesNotMatch(source, /\b(?:confirm|alert|prompt)\s*\(/);
  assert.doesNotMatch(source, /addEventListener\(["']beforeunload["']/);
});

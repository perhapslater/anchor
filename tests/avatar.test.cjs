const { test } = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const fs = require('node:fs');
const path = require('node:path');
function avatar() {
  function node() {
    return { dataset: {}, children: [], attributes: {}, listeners: {}, classList: { add() {} },
      setAttribute(name, value) { this.attributes[name] = value; },
      addEventListener(name, callback) { this.listeners[name] = callback; },
      append(...children) { this.children.push(...children); },
      set textContent(value) { this.text = value; this.children = []; }, get textContent() { return this.text; },
    };
  }
  const context = { window: {}, document: { createElement: node }, location: { origin: 'https://anchor.test' }, URL };
  vm.runInNewContext(fs.readFileSync(path.join(__dirname, '../web/avatar.js'), 'utf8'), context);
  return { render: context.window.anchorAvatar.render, node };
}
test('avatars accept only provider HTTPS or the protected photo route and keep names as text', () => {
  const { render, node } = avatar();
  for (const picture of ['javascript:alert(1)', 'data:image/svg+xml,<svg/>', '//evil.test/a.png', '/api/admin/export', 'https://anchor.test/api/admin/export', 'https://user:pass@evil.test/a.png']) {
    const host = node(); render(host, { name: '<img onerror=evil()>', picture });
    assert.equal(host.children[1].hidden, true, picture);
    assert.equal(host.children[0].hidden, false);
    assert.equal(host.attributes['aria-label'], 'Profile image for <img onerror=evil()>');
  }
  for (const picture of ['https://profiles.example.test/photo.png', '/api/people/u_member/photo?v=' + 'a'.repeat(64)]) {
    const host = node(); render(host, { name: 'QA Member', picture });
    assert.equal(host.children[1].hidden, false);
    assert.equal(host.children[1].referrerPolicy, 'no-referrer');
  }
});
test('shared avatars retain initials when images fail and update when the account changes', () => {
  const { render, node } = avatar(), host = node();
  render(host, { name: 'QA Member', picture: 'https://profiles.example.test/photo.png' });
  const image = host.children[1]; image.listeners.error();
  assert.equal(image.hidden, true); assert.equal(host.children[0].hidden, false);
  assert.equal(host.children[0].textContent, 'QM');
  render(host, { name: 'Other Person' });
  assert.equal(host.children[0].textContent, 'OP');
  assert.equal(host.children[1].hidden, true);
});

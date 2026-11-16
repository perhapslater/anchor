const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');
const context = { window: {} };
vm.runInNewContext(fs.readFileSync(path.join(__dirname, '../web/app-order.js'), 'utf8'), context);
const { groupsFor, moveApplication, sameOrder } = context.window.anchorAppOrder;
const plain = value => JSON.parse(JSON.stringify(value));
const ids = apps => Array.from(apps, app => app.ID);
const sections = ['Movies', 'Books', 'Empty'];
const apps = Object.freeze([
  Object.freeze({ ID: 'a', Name: 'Alpha', Section: 'Movies', Access: ['family'], HomeScreen: false }),
  Object.freeze({ ID: 'b', Name: 'Beta', Section: 'Books', Disabled: true, Icon: 'book' }),
  Object.freeze({ ID: 'c', Name: 'Charlie', Section: 'movies', Guide: { intro: 'Keep this' } }),
]);

test('arrangement groups applications in dashboard section order, including empty sections and case aliases', () => {
  assert.deepEqual(plain(groupsFor(apps, sections)).map(group => [group.name, ids(group.apps)]), [['Movies', ['a', 'c']], ['Books', ['b']], ['Empty', []]]);
  assert.deepEqual(plain(groupsFor([{ ID: 'a', Section: 'New' }, { ID: 'b', Section: '' }], [])), [{ name: 'New', apps: [{ ID: 'a', Section: 'New' }] }, { name: 'Apps', apps: [{ ID: 'b', Section: '' }] }]);
});
test('moving within and between sections preserves all application data and leaves inputs unchanged', () => {
  const reordered = moveApplication(apps, sections, 'c', 'Movies', 'a');
  assert.deepEqual(ids(reordered), ['c', 'a', 'b']);
  const moved = moveApplication(reordered, sections, 'a', 'Books', 'b');
  assert.deepEqual(ids(moved), ['c', 'a', 'b']);
  assert.equal(moved[1].Section, 'Books');
  assert.deepEqual(plain(moved[1]), { ...plain(apps[0]), Section: 'Books' });
  assert.deepEqual(plain(moved[2]), plain(apps[1]));
  assert.equal(apps[0].Section, 'Movies'); assert.deepEqual(ids(apps), ['a', 'b', 'c']);
  const empty = moveApplication(moved, sections, 'a', 'Empty');
  assert.deepEqual(ids(empty), ['c', 'b', 'a']); assert.equal(empty[2].Section, 'Empty');
});
test('unchanged placement is clean, section moves are dirty, and invalid targets cannot lose applications', () => {
  const grouped = groupsFor(apps, sections).flatMap(group => group.apps);
  assert.equal(sameOrder(grouped, moveApplication(grouped, sections, 'a', 'Movies', 'a')), true);
  assert.equal(sameOrder(grouped, moveApplication(grouped, sections, 'b', 'Empty')), false);
  for (const args of [['missing', 'Movies', null], ['a', 'Unknown', null], ['a', 'Books', 'c']]) assert.throws(() => moveApplication(apps, sections, ...args));
  assert.equal(new Set(ids(apps)).size, 3);
});

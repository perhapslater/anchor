const { test } = require('node:test');
const assert = require('node:assert/strict');
const { readFileSync } = require('node:fs');
const { execFileSync } = require('node:child_process');
const path = require('node:path');

test('Git preserves the exact bytes of pinned icon assets', () => {
  const root = path.resolve(__dirname, '..');
  const manifest = JSON.parse(readFileSync(path.join(root, 'third_party/dashboard-icons/manifest.json'), 'utf8'));
  for (const asset of manifest.assets) {
    assert.ok(asset.file.startsWith('web/icons/'));
  }
  const input = manifest.assets.map(asset => asset.file).join('\n') + '\n';
  const hashes = args => execFileSync('git', ['hash-object', ...args, '--stdin-paths'], { cwd: root, input, encoding: 'utf8', windowsHide: true }).trim().split(/\r?\n/);
  const filtered = hashes([]), original = hashes(['--no-filters']);
  assert.equal(filtered.length, manifest.assets.length);
  for (let i = 0; i < manifest.assets.length; i++) assert.equal(filtered[i], original[i], manifest.assets[i].file + ' would change when added to Git');
});

// Opt-in real Chromium QA. Build Anchor and the Go test fixture into a temporary
// directory, then set ANCHOR_QA_BINARY, ANCHOR_QA_FIXTURE and optionally
// ANCHOR_PLAYWRIGHT_MODULE / ANCHOR_QA_CHROME. No production state is read.
const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs/promises');
const os = require('node:os');
const path = require('node:path');
const net = require('node:net');
const { spawn } = require('node:child_process');
const { chromium } = require(process.env.ANCHOR_PLAYWRIGHT_MODULE || 'playwright');

const binary = process.env.ANCHOR_QA_BINARY;
const fixture = process.env.ANCHOR_QA_FIXTURE;
if (!binary || !fixture) throw new Error('Set temporary ANCHOR_QA_BINARY and ANCHOR_QA_FIXTURE paths.');
const password = 'qa-pass'; // Deliberately short: every password-creation path must allow it.

async function port() {
  const server = net.createServer();
  await new Promise(resolve => server.listen(0, '127.0.0.1', resolve));
  const value = server.address().port;
  await new Promise(resolve => server.close(resolve));
  return value;
}
async function ready(url) {
  for (let i = 0; i < 100; i++) {
    try { if ((await fetch(url)).ok) return; } catch {}
    await new Promise(resolve => setTimeout(resolve, 100));
  }
  throw new Error('Disposable server did not start');
}

test('disposable real-browser product workflows', { timeout: 180000 }, async t => {
  await fs.access(binary); await fs.access(fixture);
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'anchor-browser-qa-'));
  const origin = `http://127.0.0.1:${await port()}`;
  const env = Object.fromEntries(Object.entries(process.env).filter(([key]) => !key.toUpperCase().startsWith('ANCHOR_')));
  const children = [];
  function start(exe, args, extra, capture = false) {
    const child = spawn(exe, args, { env: { ...env, ...extra }, windowsHide: true, stdio: capture ? ['ignore', 'pipe', 'ignore'] : 'ignore' });
    children.push(child);
    return child;
  }
  t.after(async () => {
    await Promise.all(children.map(child => new Promise(resolve => {
      if (child.exitCode !== null || child.signalCode !== null) return resolve();
      child.once('exit', resolve); child.kill();
    })));
    // This exact mkdtemp result is owned solely by this run.
    await fs.rm(dir, { recursive: true, force: true });
  });
  start(binary, [], { ANCHOR_CONFIG_DIR: dir, ANCHOR_MODE: 'lan', ANCHOR_ALLOW_HTTP: '1', ANCHOR_PUBLIC_URL: origin, ANCHOR_LISTEN: origin.slice(7) });
  await ready(origin + '/healthz');
  const browser = await chromium.launch({ executablePath: process.env.ANCHOR_QA_CHROME || 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe', headless: true,
    // Desktop scrollbars affect the centered workspace's position. Playwright's
    // hidden-scrollbar default masked the shift between short and tall sections.
    ignoreDefaultArgs: ['--hide-scrollbars'],
    args: ['--disable-background-networking', '--disable-component-update', '--no-first-run', '--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1'] });
  t.after(() => browser.close());
  const context = await browser.newContext();
  await context.addInitScript(() => {
    window.anchorQaConfirmationChoice = true;
    window.anchorQaConfirmations = [];
    new MutationObserver(records => {
      for (const record of records) {
        const dialog = record.target;
        if (record.attributeName !== 'open' || dialog.id !== 'anchor-confirm' || !dialog.open) continue;
        window.anchorQaConfirmations.push({ title: dialog.querySelector('h2').textContent, detail: dialog.querySelector('#anchor-confirm-detail').textContent });
        if (window.anchorQaConfirmationChoice !== null) dialog.querySelector(window.anchorQaConfirmationChoice ? '#anchor-confirm-accept' : '#anchor-confirm-cancel').click();
      }
    }).observe(document, { attributes: true, attributeFilter: ['open'], subtree: true });
  });
  await context.route('**/*', route => new URL(route.request().url()).origin === origin ? route.continue() : route.abort());
  const page = await context.newPage();
  const exceptions = [], consoleErrors = [];
  let auditing = false;
  const axeSource = process.env.ANCHOR_QA_AXE_PATH ? await fs.readFile(process.env.ANCHOR_QA_AXE_PATH, 'utf8') : null;
  async function auditUI(label) {
    if (!axeSource) return;
    auditing = true;
    try {
      if (!await page.evaluate(() => !!window.axe)) await page.evaluate(axeSource);
      const violations = await page.evaluate(async () => {
        const result = await axe.run(document, { runOnly: { type: 'tag', values: ['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa', 'best-practice'] } });
        return result.violations.map(v => ({ id: v.id, nodes: v.nodes.map(n => n.target) }));
      });
      assert.deepEqual(violations, [], label);
    } finally { auditing = false; }
  }
  page.on('pageerror', err => exceptions.push(err.message));
  page.on('console', msg => { if (msg.type() === 'error' && !auditing) consoleErrors.push(msg.text()); });
  page.on('dialog', async dialog => { exceptions.push('Unexpected browser confirmation: ' + dialog.message()); await dialog.dismiss(); });
  page.setDefaultTimeout(8000);
  const post = (url, body) => page.evaluate(async ({ url, body }) => {
    const response = await fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
    return { status: response.status, body: await response.json() };
  }, { url, body });
  const state = () => page.evaluate(() => fetch('/api/admin/state').then(r => r.json()));
  async function showBuilderPreview() {
    if (!await page.locator('#builder-preview').isVisible()) await page.locator('#toggle-builder-preview').click();
    await page.locator('#builder-preview').waitFor({ state: 'visible' });
  }
  const save = async mutate => {
    const before = await state(); mutate(before.config);
    const result = await post('/api/admin/config', { revision: before.revision, config: before.config });
    assert.equal(result.status, 200);
  };
  async function checkUserDialog(label) {
    assert.equal(await page.locator('#user-dialog').evaluate(el => el.matches(':modal')), true);
    assert.equal(await page.locator('#user-name').evaluate(el => el === document.activeElement), true);
    for (const theme of ['light', 'dark']) for (const width of [320, 1440]) {
      await page.emulateMedia({ colorScheme: theme }); await page.setViewportSize({ width, height: 450 });
      const geometry = await page.locator('#user-dialog').evaluate(el => {
        const dialog = el.getBoundingClientRect(), actions = el.querySelector('.user-dialog-actions').getBoundingClientRect();
        return { top: dialog.top, bottom: dialog.bottom, left: dialog.left, right: dialog.right,
          actionsBottom: actions.bottom, overflow: el.scrollWidth > el.clientWidth };
      });
      assert.ok(geometry.top >= 0 && geometry.bottom <= 450 && geometry.left >= 0 && geometry.right <= width, `${label} dialog fits viewport`);
      assert.ok(geometry.actionsBottom <= geometry.bottom && !geometry.overflow, `${label} actions stay visible without horizontal scrolling`);
      await page.locator('#user-enabled').focus();
      assert.equal(await page.locator('#user-enabled').evaluate(el => {
        const field = el.getBoundingClientRect(), body = document.querySelector('#user-dialog-body').getBoundingClientRect();
        return field.top >= body.top && field.bottom <= body.bottom;
      }), true, 'last field remains reachable by scrolling');
      await page.locator('#cancel-user').focus(); await page.keyboard.press('Tab');
      assert.equal(await page.locator('#close-user').evaluate(el => el === document.activeElement), true, 'Tab stays inside dialog');
      await page.keyboard.press('Shift+Tab');
      assert.equal(await page.locator('#cancel-user').evaluate(el => el === document.activeElement), true, 'Shift+Tab stays inside dialog');
      await auditUI(`${label} user dialog ${theme} ${width}`);
      if (process.env.ANCHOR_QA_ARTIFACT_DIR) {
        await fs.mkdir(process.env.ANCHOR_QA_ARTIFACT_DIR, { recursive: true });
        await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `${label}-user-dialog-${theme}-${width}.png`) });
      }
    }
    await page.emulateMedia({ colorScheme: 'light' }); await page.setViewportSize({ width: 1280, height: 900 });
    if (process.env.ANCHOR_QA_ARTIFACT_DIR) {
      await page.locator('#user-name').focus();
      await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `${label}-user-dialog-desktop.png`) });
    }
    await page.keyboard.press('Escape');
    await page.locator('#user-dialog').waitFor({ state: 'hidden' });
    assert.equal(await page.locator('#add-user').evaluate(el => el === document.activeElement), true, 'closing restores the opener');
    await page.locator('#add-user').click();
    await page.locator('#user-name').fill('Unsaved draft');
    await page.locator('#user-password').fill('unsaved-password');
    await page.evaluate(() => { window.anchorQaConfirmationChoice = false; });
    try {
      for (const close of [() => page.keyboard.press('Escape'), () => page.locator('#close-user').click(), () => page.locator('#cancel-user').click()]) {
        await close();
        await page.locator('#anchor-confirm').waitFor({ state: 'hidden' });
        assert.equal(await page.locator('#user-dialog').isVisible(), true, 'declining discard keeps dialog open');
        assert.equal(await page.locator('#user-name').inputValue(), 'Unsaved draft');
      }
    } finally { await page.evaluate(() => { window.anchorQaConfirmationChoice = true; }); }
    await page.locator('#cancel-user').click();
    await page.locator('#user-dialog').waitFor({ state: 'hidden' });
    assert.equal(await page.locator('#user-password').inputValue(), '', 'discard clears password');
    await page.locator('#add-user').click();
    assert.equal(await page.locator('#user-name').inputValue(), '', 'reopening starts clean');
  }

  await t.test('first-run setup, secure local session, consumed token', async () => {
    await page.goto(origin + '/');
    await page.waitForURL('**/account.html');
    await page.locator('#setup-token').fill((await fs.readFile(path.join(dir, 'setup-token'), 'utf8')).trim());
    await page.locator('#setup-name').fill('QA Administrator');
    await page.locator('#setup-username').fill('qa-admin');
    await page.locator('#setup-password').fill('a'.repeat(11));
    assert.equal(await page.locator('#setup-password-warning').isVisible(), true);
    await page.locator('#setup-password').fill('a'.repeat(12));
    assert.equal(await page.locator('#setup-password-warning').isVisible(), false);
    await page.locator('#setup-password').fill('😀'.repeat(11));
    assert.equal(await page.locator('#setup-password-warning').isVisible(), true, 'count characters rather than UTF-8 bytes or UTF-16 code units');
    await page.locator('#setup-password').fill('');
    assert.equal(await page.locator('#setup-password-warning').isVisible(), false, 'empty fields use required-field validation instead of a strength warning');
    await page.locator('#setup-password').fill(password);
    assert.equal(await page.locator('#setup-password').evaluate(el => el.validity.valid), true);
    for (const theme of ['light', 'dark']) {
      await page.emulateMedia({ colorScheme: theme }); await page.setViewportSize({ width: 320, height: 900 });
      await auditUI(`advisory setup password warning ${theme}`);
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
      if (process.env.ANCHOR_QA_ARTIFACT_DIR) {
        await fs.mkdir(process.env.ANCHOR_QA_ARTIFACT_DIR, { recursive: true });
        await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `password-warning-${theme}.png`), fullPage: true });
      }
    }
    await page.emulateMedia({ colorScheme: 'light' }); await page.setViewportSize({ width: 1280, height: 900 });
    // Keyboard submission avoids an Edge password popup consuming the first
    // pointer click after the mobile screenshots and viewport restoration.
    await page.getByRole('button', { name: 'Create administrator' }).focus();
    const [claim] = await Promise.all([page.waitForResponse(response => response.url().endsWith('/api/auth/setup')), page.getByRole('button', { name: 'Create administrator' }).press('Enter')]);
    if (claim.status() !== 200) assert.fail(`Administrator setup failed (${claim.status()}): ${await claim.text()}`);
    await page.waitForURL('**/manage.html');
    await page.locator('#content').waitFor({ state: 'visible' });
    await assert.rejects(fs.access(path.join(dir, 'setup-token')));
    const cookies = await context.cookies();
    assert.equal(cookies.length, 1); assert.equal(cookies[0].httpOnly, true); assert.equal(cookies[0].sameSite, 'Strict');
  });
  await t.test('resumable setup wizard, sign-in email guidance and simultaneous notification settings', async () => {
    await page.locator('#wizard-title').filter({ hasText: 'People' }).waitFor();
    assert.deepEqual(await page.locator('#wizard-steps .wizard-step-label').allTextContents(), ['People', 'Applications', 'Notifications', 'Tailscale(optional)', 'Review setup']);
    assert.equal((await state()).setup.step, 0);
    await page.locator('#wizard-pause').click();
    assert.equal(await page.locator('#setup-wizard').isVisible(), false);
    assert.equal(await page.locator('#open-setup').isVisible(), true);
    await page.locator('#open-setup').click();
    await page.locator('#wizard-title').filter({ hasText: 'People' }).waitFor();
    assert.equal(await page.locator('#user-list .person-card').count(), 1);
    assert.doesNotMatch(await page.locator('#user-list').innerText(), /Role:|Groups:|Local password:|Tailscale:/);
    const administrator = (await state()).users.find(user => user.username === 'qa-admin');
    const administratorCookie = (await context.cookies()).find(cookie => cookie.name === 'anchor_session').value;
    for (const name of [administrator.name, administrator.name + ' Edited', administrator.name]) {
      const card = page.locator('.person-card').filter({ has: page.locator(`[data-user-id="${administrator.id}"]`) });
      assert.ok((await card.locator('.person-tag').allTextContents()).includes('admin'), 'administrator group is visible');
      assert.equal(await card.locator('.person-role').textContent(), 'Administrator');
      await card.locator('summary').click();
      assert.equal(await card.getByRole('button', { name: 'Remove ' + (await state()).users.find(user => user.id === administrator.id).name, exact: true }).isDisabled(), true, 'only recovery administrator cannot be removed');
      assert.match(await card.innerText(), /You can still edit this account/);
      await card.getByRole('button', { name: 'Edit ' + (await state()).users.find(user => user.id === administrator.id).name, exact: true }).click();
      assert.equal(await page.locator('#user-password').inputValue(), '');
      await page.locator('#user-name').fill(name);
      await page.locator('#user-form button[type=submit]').click();
      await page.locator('#user-dialog').waitFor({ state: 'hidden' });
      await page.locator('#message').filter({ hasText: 'User saved.' }).waitFor();
      assert.equal(page.url(), origin + '/manage.html');
      assert.equal(await page.locator('#wizard-title').textContent(), 'People');
      assert.equal((await state()).setup.step, 0);
      assert.equal((await context.cookies()).find(cookie => cookie.name === 'anchor_session').value, administratorCookie);
      assert.equal(await page.locator('.person-name').first().textContent(), name);
    }
    await page.reload();
    await page.locator('#wizard-title').filter({ hasText: 'People' }).waitFor();
    assert.equal((await context.cookies()).find(cookie => cookie.name === 'anchor_session').value, administratorCookie);
    await page.locator('#add-user').click();
    await checkUserDialog('setup');
    await page.locator('#user-name').fill('Wizard Member');
    await page.locator('#user-username').fill('wizard-member');
    await page.locator('#user-groups').fill('family');
    await page.locator('#user-password').fill(password);
    assert.equal(await page.locator('#user-password-warning').isVisible(), true);
    await page.locator('#user-form button[type=submit]').click();
    await page.locator('#message').filter({ hasText: 'User saved.' }).waitFor();
    for (const theme of ['light', 'dark']) for (const width of [320, 1440]) {
      await page.emulateMedia({ colorScheme: theme }); await page.setViewportSize({ width, height: 900 });
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'People setup reflows');
      assert.ok(await page.locator('#setup-wizard').evaluate(el => el.getBoundingClientRect().height < 150), 'progress navigation stays compact');
      await auditUI(`People cards ${theme} ${width}`);
      if (process.env.ANCHOR_QA_ARTIFACT_DIR) {
        await fs.mkdir(process.env.ANCHOR_QA_ARTIFACT_DIR, { recursive: true });
        await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `people-${theme}-${width}.png`), fullPage: true });
      }
    }
    await page.emulateMedia({ colorScheme: 'light' }); await page.setViewportSize({ width: 1280, height: 900 });
    await page.locator('#wizard-next').click();
    await page.locator('#wizard-title').filter({ hasText: 'Applications' }).waitFor();
    assert.equal(await page.locator('#wizard-next').textContent(), 'Next: Notifications');
    await page.locator('#add-app').click();
    await page.locator('#app-form').waitFor({ state: 'visible' });
    assert.equal(await page.locator('#builder-actions').isVisible(), true);
    assert.equal(await page.locator('#wizard-footer').isVisible(), false);
    assert.equal(await page.locator('#setup-action-title').textContent(), 'Application editor');
    assert.equal(await page.locator('#wizard-steps button:enabled').count(), 0, 'wizard is paused even before the first draft edit');
    assert.equal(await page.locator('#builder-next').textContent(), 'Next: App details');
    await page.locator('#builder-next').click();
    assert.equal(await page.locator('#builder-next').textContent(), 'Next: Appearance');
    assert.equal(await page.locator('#builder-preview').isVisible(), false, 'preview is collapsed by default');
    await showBuilderPreview();
    await page.locator('#toggle-builder-preview').click();
    assert.equal(await page.locator('#builder-preview').isVisible(), false);
    await page.locator('#app-name').fill('Wizard example');
    await page.locator('#app-url').fill('https://example.test/');
    for (const theme of ['light', 'dark']) for (const width of [320, 768, 1440]) {
      await page.emulateMedia({ colorScheme: theme }); await page.setViewportSize({ width, height: 900 });
      await page.evaluate(() => window.scrollTo(0, document.body.scrollHeight));
      await page.evaluate(() => new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(resolve))));
      await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
      const footer = await page.locator('#setup-actions').boundingBox();
      assert.ok(footer.y >= 0 && footer.y + footer.height <= 901, 'shared footer remains in the viewport');
      assert.ok(await page.locator('#app-audience').evaluate(field => field.getBoundingClientRect().bottom <= document.getElementById('setup-actions').getBoundingClientRect().top), 'the final visible field can scroll above the fixed footer');
      assert.equal(await page.locator('#setup-actions button.primary:visible').count(), 1, 'only one primary continuation is visible');
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
      await auditUI(`shared application footer ${theme} ${width}`);
      if (process.env.ANCHOR_QA_ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `shared-footer-${theme}-${width}.png`) });
    }
    await page.emulateMedia({ colorScheme: 'light' }); await page.setViewportSize({ width: 1280, height: 900 });
    await page.locator('#builder-next').click();
    assert.equal(await page.locator('#builder-next').textContent(), 'Next: Connection');
    await page.locator('#builder-next').click();
    assert.equal(await page.locator('#builder-next').textContent(), 'Next: Review application');
    await page.locator('#builder-next').click();
    assert.equal(await page.locator('#app-save').evaluate(button => button.form.id), 'app-form');
    if (await page.locator('#app-advanced summary').isVisible()) await page.locator('#app-advanced summary').click(); await page.locator('#app-check').fill('off');
    await page.locator('#app-check').evaluate(field => field.scrollIntoView({ block: 'end' }));
    assert.ok(await page.locator('#app-check').evaluate(field => field.getBoundingClientRect().bottom <= document.getElementById('setup-actions').getBoundingClientRect().top), 'last field is not covered by the footer');
    await page.locator('#app-save').click();
    await page.locator('#message').filter({ hasText: 'Application saved.' }).waitFor();
    assert.equal((await state()).setup.step, 1, 'saving an application does not advance Anchor setup');
    assert.equal(await page.locator('#builder-actions').isVisible(), false);
    assert.equal(await page.locator('#wizard-footer').isVisible(), true);
    assert.equal(await page.locator('#wizard-next').textContent(), 'Next: Notifications');
    assert.equal(await page.locator('#setup-action-title').textContent(), 'Anchor setup');
    await page.locator('#add-app').click();
    await page.locator('#cancel-app').click();
    assert.equal(await page.locator('#wizard-footer').isVisible(), true, 'cancelling restores wizard actions');
    const wizardExample = (await state()).config.Apps.find(app => app.Name === 'Wizard example');
    await page.locator(`#app-list [data-app-id="${wizardExample.ID}"]`).getByRole('button', { name: 'Remove', exact: true }).click();
    await page.locator('#message').filter({ hasText: 'Saved.' }).waitFor();
    await page.locator('#wizard-steps li').nth(3).getByRole('button').click();
    await page.locator('#wizard-title').filter({ hasText: 'Tailscale' }).waitFor();
    assert.equal(await page.locator('#wizard-skip-top').isVisible(), true);
    assert.match(await page.locator('#wizard-steps li').nth(3).textContent(), /Tailscale\(optional\)/);
    assert.match(await page.locator('#wizard-steps li').nth(3).getByRole('button').getAttribute('aria-label'), /Tailscale \(optional\)/);
    assert.equal(await page.locator('#tailscale-guide').isVisible(), false, 'explain benefits before showing setup');
    for (const theme of ['light', 'dark']) for (const width of [320, 1440]) {
      await page.emulateMedia({ colorScheme: theme }); await page.setViewportSize({ width, height: 900 });
      await page.evaluate(() => window.scrollTo(0, 0));
      assert.match(await page.locator('#wizard-count').textContent(), /Tailscale \(optional\)/, 'compact mobile header retains the optional label');
      assert.ok(await page.locator('#setup-wizard').evaluate(el => el.getBoundingClientRect().height < 150));
      await auditUI(`Tailscale introduction ${theme} ${width}`);
      if (process.env.ANCHOR_QA_ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `tailscale-wizard-top-${theme}-${width}.png`) });
    }
    await page.emulateMedia({ colorScheme: 'light' }); await page.setViewportSize({ width: 1280, height: 900 });
    const peopleBeforeSkip = (await state()).users;
    await page.locator('#wizard-skip-top').click();
    await page.locator('#wizard-title').filter({ hasText: 'Review' }).waitFor();
    assert.ok((await state()).setup.skipped.includes(3));
    assert.deepEqual((await state()).users, peopleBeforeSkip, 'skipping does not change identity links or accounts');
    await page.locator('#wizard-steps li').nth(1).getByRole('button').click();
    await page.reload();
    await page.locator('#wizard-title').filter({ hasText: 'Applications' }).waitFor();
    await page.locator('#wizard-steps li').nth(3).getByRole('button').click();
    await page.locator('#wizard-title').filter({ hasText: 'Tailscale' }).waitFor();
    await page.locator('#start-tailscale').click();
    assert.ok((await page.locator('#tailscale-user option').allTextContents()).includes('Wizard Member'));
    const wizardMember = (await state()).users.find(user => user.username === 'wizard-member');
    await page.locator(`[data-tailscale-user="${wizardMember.id}"]`).click();
    assert.equal(await page.locator('#tailscale-email').evaluate(el => el === document.activeElement), true);
    await page.locator('#tailscale-user').selectOption(wizardMember.id);
    await page.locator('#tailscale-email').fill('wizard-member@example.test');
    await page.locator('#tailscale-form button[type=submit]').click();
    await page.locator('#tailscale-save-status').filter({ hasText: 'Tailscale identity saved.' }).waitFor();
    assert.equal((await state()).users.find(user => user.id === wizardMember.id).tailscale, 'wizard-member@example.test');
    const adminForSetup = (await state()).users.find(user => user.username === 'qa-admin');
    const setupCookie = (await context.cookies()).find(cookie => cookie.name === 'anchor_session').value;
    await page.locator(`[data-tailscale-user="${adminForSetup.id}"]`).click();
    await page.locator('#tailscale-user').selectOption(adminForSetup.id);
    await page.locator('#tailscale-email').fill('qa-admin@example.test');
    await Promise.all([page.waitForResponse(response => response.url().endsWith('/api/auth/status')), page.locator('#tailscale-form button[type=submit]').click()]);
    assert.equal(page.url(), origin + '/manage.html');
    assert.equal((await context.cookies()).find(cookie => cookie.name === 'anchor_session').value, setupCookie);
    await page.locator('#tailscale-dialog').waitFor({ state: 'hidden' });
    await page.locator(`[data-tailscale-user="${wizardMember.id}"]`).click();
    await page.locator('#tailscale-email').fill('qa-admin@example.test');
    await page.locator('#tailscale-form button[type=submit]').click();
    await page.locator('#tailscale-form-error').waitFor({ state: 'visible' });
    assert.equal(await page.locator('#tailscale-email').inputValue(), 'qa-admin@example.test', 'duplicate rejection keeps the draft inside the dialog');
    assert.equal((await state()).users.find(user => user.id === wizardMember.id).tailscale, 'wizard-member@example.test');
    for (const theme of ['light', 'dark']) {
      await page.emulateMedia({ colorScheme: theme }); await page.setViewportSize({ width: 320, height: 450 });
      await page.locator('#cancel-tailscale').focus(); await page.keyboard.press('Tab');
      assert.equal(await page.locator('#close-tailscale').evaluate(el => el === document.activeElement), true);
      assert.ok(await page.locator('#tailscale-dialog').evaluate(el => el.getBoundingClientRect().bottom <= innerHeight && el.scrollWidth <= el.clientWidth));
      await auditUI(`Tailscale email dialog ${theme}`);
    }
    await page.locator('#cancel-tailscale').click();
    await page.locator('#tailscale-dialog').waitFor({ state: 'hidden' });
    await page.emulateMedia({ colorScheme: 'light' }); await page.setViewportSize({ width: 1280, height: 900 });
    const beforeVerify = await page.evaluate(() => fetch('/api/auth/status').then(r => r.json()));
    const setupBeforeVerify = (await state()).setup;
    const verifyWrites = [];
    const recordVerify = request => { if (request.method() !== 'GET') verifyWrites.push(request.url()); };
    page.on('request', recordVerify);
    await Promise.all([page.waitForResponse(response => response.url().endsWith('/api/tailscale-test')), page.locator('#verify-tailscale').click()]);
    page.off('request', recordVerify);
    await page.locator('#tailscale-verification[data-state="mode"]').waitFor();
    await page.locator('#tailscale-check-result[data-state="mode"]').waitFor({ state: 'visible' });
    assert.equal(await page.locator('#tailscale-check-result h3').textContent(), 'Automatic Tailscale sign-in is not enabled');
    assert.equal(await page.locator('#tailscale-address').isEditable(), true);
    assert.equal(page.url(), origin + '/manage.html');
    assert.deepEqual(verifyWrites, [], 'verification makes no changes');
    const afterVerify = await page.evaluate(() => fetch('/api/auth/status').then(r => r.json()));
    assert.equal(afterVerify.method, beforeVerify.method); assert.equal(afterVerify.mode, beforeVerify.mode); assert.equal(afterVerify.user.id, beforeVerify.user.id);
    assert.deepEqual((await state()).setup, setupBeforeVerify);
    await context.route('**/api/tailscale-test', route => route.abort());
    await page.locator('#verify-tailscale').click();
    await page.locator('#tailscale-check-result[data-state="error"]').waitFor();
    assert.match(await page.locator('#tailscale-check-result').innerText(), /connected to Tailscale/);
    assert.equal(page.url(), origin + '/manage.html', 'failed verification does not redirect');
    await context.unroute('**/api/tailscale-test');
    await Promise.all([page.waitForResponse(response => response.url().endsWith('/api/tailscale-test')), page.locator('#verify-tailscale').click()]);
    if (process.env.ANCHOR_QA_ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, 'tailscale-ready.png'), fullPage: true });
    await page.locator('#serve-deployment > summary').click();
    assert.equal(await page.locator('#tailscale-form').isVisible(), false, 'identity editing lives in a dialog');
    assert.equal(await page.locator('#connection-overview').isVisible(), false);
    assert.match(await page.locator('#tailscale-email').locator('..').textContent(), /email address this person uses to sign into Tailscale/);
    await page.locator('#serve-target-port').fill('18082');
    await page.locator('#serve-https-port').fill('8444');
    assert.equal(await page.locator('#serve-command').inputValue(), 'tailscale serve --bg --https=8444 http://127.0.0.1:18082');
    assert.deepEqual(await page.locator('#serve-context option').allTextContents(), ['Server', 'Container', 'Local Development']);
    assert.doesNotMatch(await page.locator('#serve-instructions').textContent(), /Unraid/);
    assert.match(await page.locator('#serve-run-location').textContent(), /your server's terminal/);
    assert.equal(await page.locator('#serve-platform-details').evaluate(el => el.open), false);
    await page.locator('#serve-platform-details summary').click();
    assert.equal(await page.locator('[data-serve-platform="host"]').isVisible(), true);
    assert.match(await page.locator('[data-serve-platform="host"]').textContent(), /Linux with Docker/);
    await page.setViewportSize({ width: 320, height: 900 });
    await auditUI('expanded server platform notes 320');
    assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
    await page.locator('#serve-platform-details summary').click();
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.locator('#serve-context').selectOption('container');
    await page.locator('#serve-container').fill('tailscale-anchor');
    assert.equal(await page.locator('#serve-command').inputValue(), 'docker exec tailscale-anchor tailscale serve --bg --https=8444 http://127.0.0.1:18082');
    await page.locator('#serve-container').fill('ts; unsafe');
    assert.equal(await page.locator('#serve-command').inputValue(), '');
    assert.equal(await page.locator('#copy-serve-command').isDisabled(), true);
    await page.locator('#serve-context').selectOption('host');
    assert.equal(await page.locator('#serve-container').isDisabled(), true);
    await page.locator('#serve-target-port').press('Enter');
    assert.match(await page.locator('#serve-command').inputValue(), /^tailscale serve --bg/);
    await page.locator('#serve-context').selectOption('container');
    await page.locator('#serve-container').fill('tailscale-anchor');
    await page.evaluate(() => {
      window.syntheticCopiedCommands = [];
      Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async text => window.syntheticCopiedCommands.push(text) } });
    });
    await page.locator('#copy-serve-command').click();
    assert.deepEqual(await page.evaluate(() => window.syntheticCopiedCommands), ['docker exec tailscale-anchor tailscale serve --bg --https=8444 http://127.0.0.1:18082']);
    await page.evaluate(() => { navigator.clipboard.writeText = async () => { throw new Error('Synthetic clipboard permission denied'); }; });
    await page.locator('#copy-serve-command').click();
    await page.locator('#serve-copy-result').filter({ hasText: 'Command selected' }).waitFor();
    assert.equal(await page.locator('#serve-command').evaluate(el => el.selectionEnd - el.selectionStart), (await page.locator('#serve-command').inputValue()).length);
    await page.locator('#serve-context').selectOption('dev');
    assert.equal(await page.locator('#serve-command').inputValue(), '.\\dev.ps1 -Port 18082 -Tailscale -TailscalePort 8444');
    await page.locator('#serve-context').selectOption('host');
    for (const theme of ['light', 'dark']) for (const width of [320, 1440]) {
      await page.emulateMedia({ colorScheme: theme }); await page.setViewportSize({ width, height: 900 });
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'Tailscale setup reflows');
      await auditUI(`tailscale setup ${theme} ${width}`);
      if (process.env.ANCHOR_QA_ARTIFACT_DIR) {
        await fs.mkdir(process.env.ANCHOR_QA_ARTIFACT_DIR, { recursive: true });
        await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `tailscale-${theme}-${width}.png`), fullPage: true });
      }
    }
    await page.emulateMedia({ colorScheme: 'light' }); await page.setViewportSize({ width: 1280, height: 900 });
    await page.reload();
    await page.locator('#wizard-title').filter({ hasText: 'Tailscale' }).waitFor();
    assert.equal((await state()).setup.step, 3);
    await page.locator('#wizard-steps li').nth(1).getByRole('button').click();
    await page.locator('#wizard-title').filter({ hasText: 'Applications' }).waitFor();
    await page.locator('#wizard-skip').click();
    await page.locator('#wizard-title').filter({ hasText: 'Notifications' }).waitFor();
    assert.equal(await page.locator('#wizard-skip-top').isVisible(), false);
    await page.locator('#notification-ntfy-tab').click();
    await page.locator('#notify-enabled').check();
    await page.locator('#notify-server').fill('https://ntfy.example.test');
    await page.locator('#notify-topic').fill('setup-test');
    await page.locator('#key-ntfy').fill('synthetic-publish-token');
    await page.locator('#wizard-next').click();
    await page.locator('#message').filter({ hasText: 'Save or cancel your form changes' }).waitFor();
    assert.equal((await state()).setup.step, 2);
    await page.locator('#notification-ntfy-tab').click();
    await page.locator('#notifications-form button[type=submit]').click();
    await page.locator('#message').filter({ hasText: 'ntfy settings saved.' }).waitFor();
    await page.locator('#notification-webhook-tab').click();
    await page.locator('#webhook-enabled').check();
    await page.locator('#key-webhook').fill('https://discord.com/api/webhooks/123/synthetic-private-token');
    await page.locator('#webhook-form button[type=submit]').click();
    await page.locator('#message').filter({ hasText: 'Webhook settings saved.' }).waitFor();
    assert.ok((await state()).config.Notify); assert.ok((await state()).config.Webhook);
    assert.equal(await page.locator('#key-webhook').inputValue(), '');
    assert.equal(await page.locator('#test-ntfy').isEnabled(), true);
    assert.equal(await page.locator('#test-webhook').isEnabled(), true);
    await page.reload();
    await page.locator('#wizard-title').filter({ hasText: 'Notifications' }).waitFor();
    assert.equal(await page.locator('#notify-server').inputValue(), 'https://ntfy.example.test');
    assert.equal(await page.locator('#webhook-enabled').isChecked(), true);
    for (const theme of ['light', 'dark']) for (const width of [320, 1440]) {
      await page.emulateMedia({ colorScheme: theme }); await page.setViewportSize({ width, height: 900 });
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'wizard reflows');
      await auditUI(`setup notifications ${theme} ${width}`);
      if (process.env.ANCHOR_QA_ARTIFACT_DIR) {
        await fs.mkdir(process.env.ANCHOR_QA_ARTIFACT_DIR, { recursive: true });
        await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `setup-${theme}-${width}.png`), fullPage: true });
      }
    }
    await page.emulateMedia({ colorScheme: 'light' }); await page.setViewportSize({ width: 1280, height: 900 });
    // No messages are sent to external services. Remove synthetic destinations
    // before the remaining workflows create applications with health checks.
    await page.locator('#notification-ntfy-tab').click();
    await page.locator('#notify-enabled').uncheck();
    await page.locator('#notifications-form button[type=submit]').click();
    await page.locator('#message').filter({ hasText: 'ntfy settings saved.' }).waitFor();
    await page.locator('#notification-webhook-tab').click();
    await page.locator('#webhook-enabled').uncheck();
    await page.locator('#clear-webhook').check();
    await page.locator('#webhook-form button[type=submit]').click();
    await page.locator('#message').filter({ hasText: 'Webhook settings saved.' }).waitFor();
    await page.locator('#wizard-next').click();
    await page.locator('#wizard-title').filter({ hasText: 'Tailscale' }).waitFor();
    await page.locator('#wizard-skip-top').click();
    await page.locator('#wizard-title').filter({ hasText: 'Review' }).waitFor();
    assert.equal(await page.locator('#review-connection-mode').textContent(), 'HTTP LAN');
    assert.equal(await page.locator('#review-connection-address').textContent(), origin);
    assert.equal(await page.locator('#review-connection-signin').textContent(), 'Local password');
    assert.match(await page.locator('#review-tailscale-status').textContent(), /not enabled in this deployment/);
    assert.equal(await page.locator('#wizard-progress-fill').evaluate(el => el.style.width), '100%');
    for (const theme of ['light', 'dark']) for (const width of [320, 1440]) {
      await page.emulateMedia({ colorScheme: theme }); await page.setViewportSize({ width, height: 900 });
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'Review reflows');
      await auditUI(`Review connection ${theme} ${width}`);
      if (process.env.ANCHOR_QA_ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `review-${theme}-${width}.png`), fullPage: true });
    }
    await page.emulateMedia({ colorScheme: 'light' }); await page.setViewportSize({ width: 1280, height: 900 });
    assert.match(await page.locator('#wizard-guide').textContent(), /Set up later: Tailscale, Applications/);
    await page.locator('#wizard-next').click();
    await page.locator('#setup-wizard').waitFor({ state: 'hidden' });
    assert.equal((await state()).setup.completed, true);
    assert.equal(await page.locator('#open-setup').isVisible(), false);
    await page.reload(); await page.locator('#content').waitFor({ state: 'visible' });
    assert.equal(await page.locator('#setup-wizard').isVisible(), false);
    assert.equal(await page.locator('#open-setup').isVisible(), false);
    await page.evaluate(() => document.getElementById('open-setup').click());
    assert.equal(await page.locator('#setup-wizard').isVisible(), false, 'stale wizard control cannot reopen completed setup');
    assert.equal((await state()).setup.completed, true);
    await page.locator('[data-panel="security"]').click(); await page.locator('#connection-tailscale-tab').click();
    assert.equal(await page.locator('#wizard-skip-top').isVisible(), false);
    assert.doesNotMatch(await page.locator('.admin-nav').innerText(), /optional/);
    assert.doesNotMatch(await page.locator('body').innerText(), /\(optional\)/, 'wizard-only tag is absent from the administration page');
    await page.getByRole('link', { name: 'Dashboard', exact: true }).click();
    await page.getByRole('link', { name: 'Administration', exact: true }).click();
    await page.locator('#content').waitFor({ state: 'visible' });
    assert.equal(await page.locator('[data-view="apps"]').isVisible(), true);
    assert.equal(await page.locator('#setup-wizard').isVisible(), false);
    assert.equal(await page.locator('#open-setup').isVisible(), false);
  });
  await t.test('notification tabs preserve both drafts and saves while Anchor confirmations protect navigation', async () => {
    const original = (await state()).config;
    await page.locator('[data-panel="notifications"]').click();
    await page.locator('#notification-ntfy-tab').click();
    await page.locator('#notify-enabled').check();
    await page.locator('#notify-server').fill('https://ntfy.example.test');
    await page.locator('#notify-topic').fill('confirmation-test');
    await page.locator('#notification-webhook-tab').click();
    await page.locator('#webhook-enabled').check();
    await page.locator('#webhook-format').selectOption('json');
    await page.locator('#key-webhook').fill('https://hooks.example.test/unsaved');
    await page.evaluate(() => { window.anchorQaConfirmationChoice = null; });
    const writes = [], record = request => { if (request.method() === 'POST' && request.url().endsWith('/api/admin/config')) writes.push(request.url()); };
    page.on('request', record);
    try {
      await page.getByRole('link', { name: 'Dashboard', exact: true }).click();
      await page.locator('#anchor-confirm').waitFor({ state: 'visible' });
      assert.equal(await page.locator('#anchor-confirm-title').textContent(), 'Leave this page?');
      await page.getByRole('button', { name: 'Keep editing', exact: true }).click();
      assert.equal(page.url(), origin + '/manage.html');
      assert.equal(await page.locator('#notify-topic').inputValue(), 'confirmation-test');
      await page.locator('[data-panel="settings"]').click();
      await page.locator('#anchor-confirm').waitFor({ state: 'visible' });
      assert.equal(await page.locator('#anchor-confirm-title').textContent(), 'Discard unsaved changes?');
      await page.getByRole('button', { name: 'Keep editing', exact: true }).click();
      assert.equal(await page.locator('[data-view="notifications"]').isVisible(), true);
      await page.locator('#notification-ntfy-tab').click();
      await page.locator('#notifications-form button[type=submit]').click();
      await page.locator('#message').filter({ hasText: 'ntfy settings saved.' }).waitFor();
      assert.equal(writes.length, 1, 'one client save submits exactly once without discarding the other');
      assert.equal((await state()).config.Notify.Topic, 'confirmation-test');
      assert.equal(await page.locator('#key-webhook').inputValue(), 'https://hooks.example.test/unsaved');
      await page.locator('#notification-ntfy-tab').click();
      await page.locator('#notify-topic').fill('ntfy-second-draft');
      await page.locator('#notification-webhook-tab').click();
      await page.locator('#webhook-form button[type=submit]').click();
      await page.locator('#message').filter({ hasText: 'Webhook settings saved.' }).waitFor();
      assert.equal(writes.length, 2);
      assert.equal(await page.locator('#notify-topic').inputValue(), 'ntfy-second-draft');
      assert.equal((await state()).config.Notify.Topic, 'confirmation-test');
      assert.equal((await state()).config.Webhook.Format, 'json');
      await page.locator('#notification-webhook-tab').press('Home');
      assert.equal(await page.locator('#notification-ntfy-tab').getAttribute('aria-selected'), 'true');
      assert.equal(await page.locator('#notification-webhook').isVisible(), false);
      await page.locator('#notification-ntfy-tab').click();
      await page.locator('#notifications-form button[type=submit]').click();
      await page.locator('#message').filter({ hasText: 'ntfy settings saved.' }).waitFor();
      assert.equal((await state()).config.Notify.Topic, 'ntfy-second-draft');
      await page.locator('#notification-webhook-tab').click();
      for (const theme of ['light', 'dark']) for (const width of [320, 1440]) {
        await page.emulateMedia({colorScheme: theme}); await page.setViewportSize({width, height: 900});
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
        if (process.env.ANCHOR_QA_ARTIFACT_DIR) await page.screenshot({path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `webhook-tab-${theme}-${width}.png`), fullPage: true});
      }
      await page.emulateMedia({colorScheme: 'light'}); await page.setViewportSize({width: 1280, height: 900});

    } finally {
      page.off('request', record);
      await page.evaluate(() => { window.anchorQaConfirmationChoice = true; });
      const current = await state();
      assert.equal((await post('/api/admin/config', { revision: current.revision, config: original })).status, 200);
      await page.reload(); await page.locator('#content').waitFor({ state: 'visible' });
    }
  });
  await t.test('application create, rename with stable ID, bundled icon and guide', async () => {
    await page.locator('[data-panel="apps"]').click();
    await page.locator('#add-app').click();
    await page.locator('#builder-next').click();
    await page.locator('#app-name').fill('Plex');
    assert.equal(await page.locator('#app-icon').inputValue(), 'plex');
    await page.locator('#app-url').fill(origin + '/healthz');
    await page.locator('#builder-next').click();
    await page.locator('#app-description').fill('Synthetic app');
    assert.equal(await page.locator('#preview-name').textContent(), 'Plex');
    await page.locator('#builder-next').click();
    await page.locator('#builder-next').click();
    if (await page.locator('#app-advanced summary').isVisible()) await page.locator('#app-advanced summary').click();
    await page.locator('#app-id').fill('invalid!');
    assert.equal(await page.locator('#app-id').evaluate(el => el.validity.patternMismatch), true);
    await page.locator('#app-id').fill('plex');
    await page.locator('#app-check').fill('off');
    await page.locator('#app-save').click();
    await page.locator('#message').filter({ hasText: 'Application saved.' }).waitFor();
    await page.locator('#app-list').getByRole('button', { name: 'Edit', exact: true }).click();
    assert.equal(await page.locator('#app-id').inputValue(), 'plex');
    await page.locator('#app-name').fill('QA renamed application');
    assert.equal(await page.locator('#builder-next').isVisible(), false);
    assert.equal(await page.locator('#builder-back').isVisible(), false);
    assert.equal(await page.locator('#app-save').textContent(), 'Save changes');
    assert.equal(await page.locator('[data-step="-1"]').isVisible(), false);
    assert.equal(await page.locator('#builder-tabs [role="tab"]').count(), 4);
    await page.locator('#app-url').fill('invalid-url');
    await page.locator('[data-step="1"]').click();
    await page.locator('#app-save').click();
    assert.equal(await page.locator('[data-app-step="0"]').isVisible(), true, 'invalid fields reveal their own tab');
    assert.equal((await state()).config.Apps[0].Name, 'Plex', 'invalid draft was not saved');
    await page.locator('#app-url').fill(origin + '/healthz');
    await page.locator('[data-step="1"]').press('End');
    assert.equal(await page.locator('[data-step="3"]').getAttribute('aria-selected'), 'true');
    await page.locator('[data-step="3"]').press('Home');
    assert.equal(await page.locator('[data-step="0"]').getAttribute('aria-selected'), 'true');
    for (const theme of ['light', 'dark']) for (const width of [320, 1440]) {
      await page.emulateMedia({ colorScheme: theme }); await page.setViewportSize({width, height: 900});
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
      await auditUI(`application edit ${theme} ${width}`);
      if (process.env.ANCHOR_QA_ARTIFACT_DIR) await page.screenshot({path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `application-edit-${theme}-${width}.png`), fullPage: true});
    }
    await page.emulateMedia({ colorScheme: 'light' }); await page.setViewportSize({width: 1280, height: 900});

    await page.locator('#app-save').click();
    await page.locator('#app-form').waitFor({ state: 'hidden' });
    const cfg = await state(); assert.equal(cfg.config.Apps[0].ID, 'plex');
    await page.goto(origin + '/open/plex?add=1');
    await page.getByRole('heading', { name: 'Add QA renamed application to your home screen' }).waitFor();
    const response = await context.request.get(origin + '/open/plex', { maxRedirects: 0 });
    assert.equal(response.status(), 302); assert.equal(response.headers().location, origin + '/healthz');
  });
  let member;
  await t.test('create member in UI, group-restricted and admin-only tiles', async () => {
    await page.goto(origin + '/manage.html');
    await page.getByRole('button', { name: 'Users & groups' }).click();
    await page.locator('#add-user').click();
    await checkUserDialog('admin');
    await page.locator('#user-name').fill('QA Member');
    await page.locator('#user-username').fill('qa-member');
    await page.locator('#user-groups').fill('family');
    await page.locator('#user-password').fill(password);
    let releaseSave;
    const saveGate = new Promise(resolve => { releaseSave = resolve; });
    await context.route('**/api/admin/users', async route => {
      await saveGate;
      await route.fulfill({ status: 400, json: { message: 'Choose a different local login.' } });
    });
    await page.locator('#user-form button[type=submit]').click();
    await page.keyboard.press('Escape');
    assert.equal(await page.locator('#user-dialog').isVisible(), true, 'Escape cannot dismiss a pending save');
    assert.equal(await page.locator('#cancel-user').isDisabled(), true);
    releaseSave();
    await page.locator('#user-form-error').filter({ hasText: 'Choose a different local login.' }).waitFor();
    assert.equal(await page.locator('#user-dialog').isVisible(), true, 'save failure stays inside dialog');
    assert.equal(await page.locator('#user-password').inputValue(), password, 'save failure preserves draft');
    await context.unroute('**/api/admin/users');
    await page.locator('#user-form button[type=submit]').click();
    await page.locator('#message').filter({ hasText: 'User saved.' }).waitFor();
    await page.locator('#user-dialog').waitFor({ state: 'hidden' });
    await page.getByRole('button', { name: 'Edit QA Member', exact: true }).click();
    assert.equal(await page.locator('#user-form-title').textContent(), 'Edit user');
    assert.equal(await page.locator('#user-name').inputValue(), 'QA Member');
    assert.equal(await page.locator('#user-password').inputValue(), '');
    assert.equal(await page.locator('#user-form-error').isVisible(), false);
    await page.locator('#user-form button[type=submit]').click();
    await page.locator('#user-dialog').waitFor({ state: 'hidden' });
    assert.equal(await page.getByRole('button', { name: 'Edit QA Member', exact: true }).evaluate(el => el === document.activeElement), true, 'saving an edit restores focus to the refreshed user card');
    await page.getByRole('button', { name: 'Edit QA Member', exact: true }).click();
    await page.keyboard.press('Escape');
    await page.locator('#user-dialog').waitFor({ state: 'hidden' });
    assert.equal(await page.getByRole('button', { name: 'Edit QA Member', exact: true }).evaluate(el => el === document.activeElement), true);
    member = (await state()).users.find(user => user.username === 'qa-member');
    assert.ok(member);
    await save(cfg => {
      cfg.Apps[0].Access = ['family'];
      cfg.Apps.push({ ...cfg.Apps[0], ID: 'private', Name: 'Admin only', Access: ['admin'] });
    });
  });
  await t.test('dashboard search and preview permission isolation', async () => {
    await page.goto(origin + '/');
    await page.locator('#sections .tile').first().waitFor();
    assert.equal(await page.locator('#sections .tile').count(), 2);
    await page.locator('#search').fill('does-not-exist');
    await page.locator('#no-results').waitFor({ state: 'visible' });
    await page.locator('#search').fill('');
    await page.locator('#preview-as').selectOption(member.id);
    await page.locator('#preview-note').filter({ hasText: 'QA Member' }).waitFor();
    assert.equal(await page.locator('#sections .tile').count(), 1);
    assert.equal(await page.locator('#sections .add-home').isVisible(), true);
    assert.equal((await page.evaluate(() => fetch('/api/me?as=bad-user').then(r => r.json()))).admin, true);
    await page.locator('#preview-as').selectOption('');
  });
  await t.test('Administration labels keep their position and typography across short and tall sections', async () => {
    await page.goto(origin + '/manage.html');
    await page.locator('#content').waitFor({ state: 'visible' });
    const panels = ['apps', 'users', 'notifications', 'security', 'settings', 'transfer'];
    for (const theme of ['light', 'dark']) for (const width of [320, 768, 1440]) {
      await page.emulateMedia({ colorScheme: theme }); await page.setViewportSize({ width, height: 900 });
      let initial;
      const scrollingStates = new Set();
      for (const panel of panels) {
        await page.locator(`[data-panel="${panel}"]`).click();
        await page.evaluate(() => window.scrollTo(0, 0));
        const labels = await page.locator('.admin-nav button').evaluateAll(buttons => buttons.map(button => {
          const style = getComputedStyle(button), box = button.getBoundingClientRect();
          const text = document.createRange(); text.selectNodeContents(button);
          return { panel: button.dataset.panel, left: box.left, width: box.width, labelLeft: text.getBoundingClientRect().left, font: style.fontFamily, size: style.fontSize, lineHeight: style.lineHeight, padding: style.padding, align: style.textAlign, weight: style.fontWeight, selected: button.getAttribute('aria-pressed') === 'true' };
        }));
        assert.equal(labels.length, 6);
        const reference = labels[0];
        for (const label of labels) {
          assert.ok(Math.abs(label.width - reference.width) < 1, `${label.panel} matches Applications column width`);
          for (const property of ['font', 'size', 'lineHeight', 'padding', 'align']) assert.equal(label[property], reference[property], `${label.panel} shares ${property}`);
          assert.equal(label.weight, label.selected ? '650' : '400');
        }
        const positions = labels.map(({ left, width, labelLeft }) => ({ left, width, labelLeft }));
        if (!initial) initial = positions;
        assert.deepEqual(positions, initial, `${panel}: switching sections does not move the sidebar or its text`);
        scrollingStates.add(await page.evaluate(() => document.documentElement.scrollHeight > document.documentElement.clientHeight));
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `${panel} reflows at ${width}px`);
        if (width === 1440 && ['apps', 'notifications'].includes(panel) && process.env.ANCHOR_QA_ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `sidebar-${panel}-${theme}.png`) });
      }
      if (width === 1440) {
        assert.equal(scrollingStates.size, 2, 'desktop check includes sections with and without vertical scrolling');
        assert.ok(await page.evaluate(() => innerWidth > document.documentElement.clientWidth), 'browser exposes scrollbar width for this regression');
      }
      if (process.env.ANCHOR_QA_ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `administration-navigation-${theme}-${width}.png`), fullPage: true });
    }
    await page.emulateMedia({ colorScheme: 'light' }); await page.setViewportSize({ width: 1280, height: 900 });
  });
  await t.test('user removal confirms inside Anchor, preserves cancellation and cleans direct audiences', async () => {
    const before = await state();
    assert.equal((await post('/api/admin/users', { revision: before.revision, user: { name: 'Removal Test <Member>', username: 'remove-test', groups: ['family'], enabled: true }, password })).status, 200);
    const target = (await state()).users.find(user => user.username === 'remove-test');
    await save(cfg => {
      cfg.Apps.push({ ...cfg.Apps[0], ID: 'removal-personal', Name: 'Removal personal app', Access: [target.id] });
      cfg.Notice = { Message: 'Removal personal announcement', Style: 'info', Access: [target.id] };
    });
    await page.goto(origin + '/manage.html');
    await page.locator('[data-panel="users"]').click();
    const card = page.locator('.person-card').filter({ has: page.locator(`[data-user-id="${target.id}"]`) });
    await card.locator('summary').click();
    const remove = card.getByRole('button', { name: 'Remove ' + target.name, exact: true });
    await page.evaluate(() => { window.anchorQaConfirmationChoice = null; });
    try {
      await remove.click();
      const confirmation = page.locator('#anchor-confirm');
      await confirmation.waitFor({ state: 'visible' });
      assert.match(await confirmation.innerText(), /Removal Test <Member>/, 'user names are displayed as text');
      assert.equal(await confirmation.locator('#anchor-confirm-cancel').evaluate(el => el === document.activeElement), true);
      assert.ok((await state()).users.some(user => user.id === target.id), 'confirmation does not delete early');
      await page.keyboard.press('Escape');
      await confirmation.waitFor({ state: 'hidden' });
      assert.ok((await state()).users.some(user => user.id === target.id));
      assert.equal(await remove.evaluate(el => el === document.activeElement), true);
      await remove.click();
      await confirmation.waitFor({ state: 'visible' });
      await page.setViewportSize({ width: 320, height: 600 });
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
      if (process.env.ANCHOR_QA_ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, 'remove-user-mobile.png'), fullPage: true });
      await confirmation.locator('#anchor-confirm-accept').click();
      await page.locator('#message').filter({ hasText: 'User removed.' }).waitFor();
      assert.equal(await card.count(), 0);
      const current = await state();
      assert.equal(current.users.some(user => user.id === target.id), false);
      assert.deepEqual(current.config.Apps.find(app => app.ID === 'removal-personal').Access, ['admin']);
      assert.deepEqual(current.config.Notice.Access, ['admin']);
      assert.equal(current.setup.step, before.setup.step);
      assert.equal(current.setup.completed, before.setup.completed);
      assert.equal(await page.locator('#add-user').evaluate(el => el === document.activeElement), true);
    } finally {
      await page.evaluate(() => { window.anchorQaConfirmationChoice = true; });
      await page.setViewportSize({ width: 1280, height: 900 });
      await save(cfg => { cfg.Apps = cfg.Apps.filter(app => app.ID !== 'removal-personal'); cfg.Notice = before.config.Notice; });
    }
  });
  await t.test('another recovery administrator permits self-removal and signs out only that account', async () => {
    const before = await state();
    assert.equal((await post('/api/admin/users', { revision: before.revision, user: { name: 'Temporary Administrator', username: 'temporary-admin', groups: ['admin'], enabled: true }, password })).status, 200);
    const temporary = (await state()).users.find(user => user.username === 'temporary-admin');
    const selfContext = await browser.newContext();
    await selfContext.route('**/*', route => new URL(route.request().url()).origin === origin ? route.continue() : route.abort());
    try {
      const login = await selfContext.request.post(origin + '/api/auth/login', { headers: { Origin: origin }, data: { Username: temporary.username, Password: password } });
      assert.equal(login.status(), 200);
      const self = await selfContext.newPage();
      self.on('pageerror', error => exceptions.push(error.message));
      self.on('dialog', async dialog => { exceptions.push('Unexpected browser confirmation: ' + dialog.message()); await dialog.dismiss(); });
      await self.goto(origin + '/manage.html');
      await self.locator('[data-panel="users"]').click();
      const card = self.locator('.person-card').filter({ has: self.locator(`[data-user-id="${temporary.id}"]`) });
      await card.locator('summary').click();
      await card.getByRole('button', { name: 'Remove ' + temporary.name, exact: true }).click();
      await self.locator('#anchor-confirm').waitFor({ state: 'visible' });
      assert.match(await self.locator('#anchor-confirm-detail').innerText(), /your own account and will be signed out/);
      await self.locator('#anchor-confirm-accept').click();
      await self.waitForURL('**/account.html');
      assert.equal((await (await selfContext.request.get(origin + '/api/auth/status')).json()).authenticated, false);
      assert.equal((await selfContext.cookies()).some(cookie => cookie.name === 'anchor_session'), false);
      const current = await state();
      assert.equal(current.users.some(user => user.id === temporary.id), false);
      assert.ok(current.users.some(user => user.username === 'qa-admin'));
    } finally { await selfContext.close(); }
  });
  await t.test('navigation, dismissible HTTP notice, guided builder and contextual API keys', async () => {
    await page.goto(origin + '/manage.html');
    await page.locator('#content').waitFor({ state: 'visible' });
    if (process.env.ANCHOR_QA_ARTIFACT_DIR) {
      await fs.mkdir(process.env.ANCHOR_QA_ARTIFACT_DIR, { recursive: true });
      await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, 'navigation-and-notice.png'), fullPage: true });
    }
    await page.getByRole('button', { name: 'Dismiss connection notice' }).click();
    for (const pathname of ['/', '/account.html', '/manage.html']) {
      await page.goto(origin + pathname); await page.waitForFunction(() => !!window.anchorCSRF);
      assert.equal(await page.locator('#http-note').isVisible(), false);
      assert.equal(await page.locator('.auth-nav a').first().evaluate(el => getComputedStyle(el).textDecorationLine), 'none');
      assert.equal(await page.getByRole('link', { name: 'My account', exact: true }).count(), 0, 'administrators use Administration instead of My account');
    }
    await page.locator('#add-app').click();
    await page.locator('#builder-next').click();
    await page.locator('#builder-next').click();
    assert.equal(await page.locator('[data-app-step="0"]').isVisible(), true);
    await page.locator('#app-name').fill('Welcome space');
    await page.locator('#app-url').fill('https://example.test/');
    await page.locator('#app-audience').selectOption('group:family');
    assert.match(await page.locator('#app-audience-help').innerText(), /Administrators always have access/);
    assert.equal(await page.locator('#app-audience option[value="group:admin"]').textContent(), 'Administrators only');
    await page.locator('#builder-next').click();
    await page.locator('#app-description').fill('A friendly place to get started');
    await page.locator('#builder-next').click();
    await page.locator('input[name="app-behavior"][value="guide"]').check();
    await page.locator('#guide-intro').fill('Start here with our household guide.');
    await page.locator('#guide-steps').fill('Open the service\nSign in with your account');
    await showBuilderPreview();
    assert.equal(await page.locator('#preview-audience').textContent(), 'Visible to: Administrators, family');
    await page.locator('#preview-open').click();
    await page.locator('#experience-content').filter({ hasText: 'Start here with our household guide.' }).waitFor();
    await auditUI('guide preview');
    await page.getByRole('button', { name: 'Close preview' }).click();
    await page.locator('#mobile-enabled').check();
    await page.locator('#mobile-server').fill('https://reader.example.test/');
    await page.locator('#preview-mobile').click();
    await page.locator('#experience-content').filter({ hasText: 'https://reader.example.test/' }).waitFor();
    await page.keyboard.press('Escape');
    assert.equal(await page.locator('#preview-mobile').evaluate(el => el === document.activeElement), true);
    for (const theme of ['light', 'dark']) {
      await page.emulateMedia({ colorScheme: theme });
      for (const width of [320, 768, 1440]) {
        await page.setViewportSize({ width, height: 900 });
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `${width}px builder overflows`);
        if (width !== 768) await auditUI(`builder ${theme} ${width}`);
        if (process.env.ANCHOR_QA_ARTIFACT_DIR) {
          await fs.mkdir(process.env.ANCHOR_QA_ARTIFACT_DIR, { recursive: true });
          await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `builder-${theme}-${width}.png`), fullPage: true });
        }
      }
    }
    await page.emulateMedia({ colorScheme: 'light' });
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.locator('#cancel-app').click();
    await page.locator('[data-panel="apps"]').click();
    assert.equal(await page.locator('[data-panel="integrations"]').count(), 0);
    for (const width of [320, 768, 1440]) {
      await page.setViewportSize({ width, height: 900 });
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `${width}px integrations overflow`);
      if (width !== 768) await auditUI(`integrations ${width}`);
    }
    await page.locator('[data-panel="notifications"]').click();
    await page.getByRole('heading', { name: 'ntfy connection' }).waitFor();
    await page.locator('#notification-ntfy-tab').click();
    await page.locator('#key-ntfy').fill('synthetic-ntfy-token');
    await page.locator('#notifications-form button[type=submit]').click();
    await page.locator('#message').filter({ hasText: 'ntfy settings saved.' }).waitFor();
    assert.equal((await state()).keys.ntfy, true);
    for (const width of [320, 1440]) {
      await page.setViewportSize({ width, height: 900 });
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
      await auditUI(`notifications ${width}`);
      if (process.env.ANCHOR_QA_ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `notifications-${width}.png`), fullPage: true });
    }
    await page.setViewportSize({ width: 1280, height: 900 });
  });
  await t.test('Connection overview stays read-only and Users & groups owns local sign-in and recovery', async () => {
    await page.locator('[data-panel="security"]').click();
    assert.equal(await page.locator('[data-view="security"] h1').textContent(), 'Connection info');
    assert.equal(await page.locator('#connection-overview form').count(), 0);
    assert.equal(await page.locator('#connection-overview h2').count(), 0, 'no redundant Current connection heading');
    assert.equal(await page.locator('#sign-out:visible').count(), 1);
    for (const width of [320, 1440]) {
      await page.setViewportSize({ width, height: 900 });
      assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
      await auditUI(`connection info ${width}`);
      if (process.env.ANCHOR_QA_ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `connection-info-${width}.png`), fullPage: true });
    }
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.locator('[data-panel="security"]').click(); await page.locator('#connection-tailscale-tab').click();
    await page.locator(`[data-tailscale-user="${member.id}"]`).click();
    await page.locator('#tailscale-user').selectOption(member.id);
    await page.locator('#tailscale-email').fill('qa-member@example.test');
    await page.locator('#tailscale-form button[type=submit]').click();
    await page.locator('#tailscale-save-status').filter({ hasText: 'Tailscale identity saved.' }).waitFor();
    await page.locator('#tailscale-address').fill('https://anchor.example.ts.net:8443');
    await page.locator('#connection-tailscale-tab').press('Home');
    assert.equal(await page.locator('#connection-overview').isVisible(), true);
    await page.locator('#connection-overview-tab').press('End');
    assert.equal(await page.locator('#tailscale-address').inputValue(), 'https://anchor.example.ts.net:8443');
    assert.equal(await page.locator('#connection-tailscale').isVisible(), true);

    await page.locator('[data-panel="users"]').click();
    await page.locator('#user-recovery summary').click();
    assert.match(await page.locator('#user-recovery').innerText(), /recover-admin/);
    await page.getByRole('button', { name: 'Edit QA Member', exact: true }).click();
    await page.locator('#user-local').uncheck();
    assert.equal(await page.locator('#user-password').isDisabled(), true);
    await page.locator('#user-form button[type=submit]').click();
    await page.locator('#user-dialog').waitFor({ state: 'hidden' });
    assert.equal((await state()).users.find(user => user.id === member.id).has_password, false);
    await page.getByRole('button', { name: 'Edit QA Member', exact: true }).click();
    await page.locator('#user-local').check();
    await page.locator('#user-password').fill(password);
    assert.equal(await page.locator('#user-password-warning').isVisible(), true);
    await page.locator('#user-form button[type=submit]').click();
    await page.locator('#user-dialog').waitFor({ state: 'hidden' });
    assert.equal((await state()).users.find(user => user.id === member.id).has_password, true);
    member = (await state()).users.find(user => user.id === member.id);
    const admin = (await state()).users.find(user => user.groups.includes('admin'));
    await page.locator('[data-panel="security"]').click(); await page.locator('#connection-tailscale-tab').click();
    await page.locator(`[data-tailscale-user="${admin.id}"]`).click();
    await page.locator('#tailscale-user').selectOption(admin.id);
    await page.locator('#tailscale-email').fill('qa-admin@example.test');
    await page.locator('#tailscale-form button[type=submit]').click();
    await page.locator('#tailscale-save-status').filter({ hasText: 'Tailscale identity saved.' }).waitFor();
    await page.locator('[data-panel="users"]').click();
    await page.getByRole('button', { name: 'Edit ' + admin.name, exact: true }).click();
    await page.locator('#user-local').uncheck();
    await page.locator('#user-form button[type=submit]').click();
    await page.locator('#user-form-error').filter({ hasText: 'keep at least one enabled administrator' }).waitFor();
    assert.equal((await state()).users.find(user => user.id === admin.id).has_password, true);
    await page.locator('#cancel-user').click();
    await page.locator('#user-dialog').waitFor({ state: 'hidden' });
    await page.reload(); await page.locator('#content').waitFor({ state: 'visible' });
  });
  await t.test('builder saves optional experiences, preserves edits and removes disabled helpers', async () => {
    await page.locator('#add-app').click();
    await page.locator('#builder-next').click();
    await page.locator('#app-name').fill('Plex');
    await page.locator('#app-url').fill('https://plex.example.test/login?start=1');
    await page.locator('[data-step="2"]').click();
    await page.locator('input[name="app-behavior"][value="guide"]').check();
    await page.locator('#guide-intro').fill('Welcome to the library');
    await page.locator('#guide-steps').fill('Use your own account\nChoose something to watch');
    await page.locator('#mobile-enabled').check();
    await page.locator('#mobile-ios').fill('https://store.example.test/plex');
    await page.locator('[data-step="3"]').click();
    if (process.env.ANCHOR_QA_ARTIFACT_DIR) await page.locator('#app-form').screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, 'builder-review.png') });
    await page.locator('#app-save').click();
    await page.locator('#message').filter({ hasText: 'Application saved.' }).waitFor();
    let saved = (await state()).config.Apps.find(app => app.ID === 'plex-2');
    assert.equal(saved.Guide.intro, 'Welcome to the library');
    assert.equal(saved.Mobile.server, 'https://plex.example.test');
    assert.equal(saved.Icon, 'plex');
    await page.locator('#app-list [data-app-id="plex-2"]').getByRole('button', { name: 'Edit', exact: true }).click();
    await page.locator('[data-step="2"]').click();
    assert.equal(await page.locator('#mobile-enabled').isChecked(), true);
    assert.equal(await page.locator('input[name="app-behavior"][value="guide"]').isChecked(), true);
    await page.locator('#mobile-ios').fill('not-a-valid-url');
    await page.locator('#mobile-enabled').uncheck();
    await page.locator('input[name="app-behavior"][value="web"]').check();
    await page.locator('[data-step="3"]').click();
    await page.locator('#app-save').click();
    await page.locator('#message').filter({ hasText: 'Application saved.' }).waitFor();
    saved = (await state()).config.Apps.find(app => app.ID === 'plex-2');
    assert.equal(saved.Mobile, null); assert.equal(saved.Guide, null);
    await save(cfg => { cfg.Apps = cfg.Apps.filter(app => app.ID !== 'plex-2'); });
    await page.reload(); await page.locator('#content').waitFor({ state: 'visible' });
  });
  await t.test('multiple Seerr connections and ebook libraries, setup previews and source switching', async () => {
    const originalConfig = (await state()).config;
    for (const [kind, name, audience] of [['requests', 'Media one', 'everyone'], ['requests', 'Media two', 'group:admin'], ['ebooks', 'Books one', 'everyone'], ['ebooks', 'Books two', 'group:admin']]) {
      await page.locator('#add-app').click();
      await page.locator(`input[name="application-type"][value="${kind}"]`).check();
      await auditUI('application type selection');
      await page.locator('#builder-next').click();
      await page.locator('#app-name').fill(name);
      await page.locator('#app-url').fill('https://source.example.test/');
      await page.locator('#app-audience').selectOption(audience);
      await page.locator('[data-step="2"]').click();
      await showBuilderPreview();
      if (kind === 'requests') {
        await page.locator('#builder-plex').fill('https://plex.example.test/');
        for (const provider of ['seerr', 'plex']) await page.locator('#builder-key-input-' + provider).fill('synthetic-' + name + '-' + provider);
        assert.equal(await page.locator('#builder-calendar').evaluate(el => el.open), true);
        await page.locator('#builder-sonarr').fill('https://sonarr.example.test');
        await page.locator('#builder-key-input-sonarr').fill('synthetic-sonarr-key');
        const previewCalls = [];
        const recordPreview = request => { if (new URL(request.url()).pathname.startsWith('/api/requests')) previewCalls.push(request.url()); };
        page.on('request', recordPreview);
        await page.locator('#preview-requests').click();
        const preview = page.locator('#experience-preview');
        assert.equal(await preview.locator('h2').textContent(), 'Request status');
        assert.equal(await preview.locator('.req-row').count(), 11);
        await preview.getByRole('searchbox').fill('shogun');
        assert.equal(await preview.locator('.req-row:visible').count(), 1);
        await preview.getByRole('searchbox').fill('missing-title');
        assert.equal(await preview.getByText('No titles match that.', { exact: true }).isVisible(), true);
        await preview.getByRole('searchbox').fill('');
        await preview.getByRole('button', { name: 'Refresh now', exact: true }).click();
        assert.match(await preview.locator('.request-sample-updated').textContent(), /no servers contacted/);
        for (const theme of ['light', 'dark']) for (const width of [320, 1440]) {
          await page.emulateMedia({ colorScheme: theme }); await page.setViewportSize({ width, height: 900 });
          await auditUI(`request preview ${theme} ${width}`);
          assert.ok(await preview.evaluate(el => el.scrollWidth <= el.clientWidth));
          if (process.env.ANCHOR_QA_ARTIFACT_DIR) await preview.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `request-preview-${theme}-${width}.png`) });
        }
        await page.emulateMedia({ colorScheme: 'light' }); await page.setViewportSize({ width: 1280, height: 900 });
        await preview.getByRole('button', { name: 'Coming up', exact: true }).click();
        await page.locator('#experience-content').filter({ hasText: 'New episode' }).waitFor();
        assert.equal(await preview.locator('.req-row').count(), 4, 'Sonarr-only preview shows the TV calendar');
        page.off('request', recordPreview);
        assert.deepEqual(previewCalls, [], 'preview never calls a real integration');
      } else {
        if (!await page.locator('#abs-connection-help').evaluate(el => el.open)) await page.locator('#abs-connection-help summary').click();
        await page.locator('#builder-abs-library').fill(name.toLowerCase().replace(' ', '-'));
        await page.locator('#builder-key-input-abs').fill('synthetic-' + name + '-key');
        await page.locator('#preview-open').click();
        await page.locator('#experience-preview').getByRole('searchbox').fill('Example ebook');
        assert.equal(await page.locator('.preview-sample-list li').count(), 1);
        await page.getByRole('button', { name: 'Download sample', exact: true }).click();
        await page.getByRole('button', { name: 'Preview only · no file downloaded', exact: true }).waitFor();
      }
      await auditUI(kind + ' sample preview');
      const builderDraft = () => page.locator('#app-form').evaluate(form => [...form.querySelectorAll('input, select, textarea')].map(input => ({ id: input.id, name: input.name, value: input.value, checked: input.checked })));
      const draftBeforeClose = await builderDraft();
      const previewDialogs = [];
      const previewConfirmations = await page.evaluate(() => window.anchorQaConfirmations.length);
      const recordPreviewDialog = dialog => previewDialogs.push(dialog.message());
      page.on('dialog', recordPreviewDialog);
      const previewTrigger = kind === 'requests' ? 'preview-requests' : 'preview-open';
      try {
        await page.getByRole('button', { name: 'Close preview', exact: true }).click();
        await page.locator('#experience-preview').waitFor({ state: 'hidden' });
        await page.waitForFunction(id => document.activeElement === document.getElementById(id), previewTrigger);
        assert.deepEqual(previewDialogs, [], `${kind} X closes without a discard prompt`);
        assert.deepEqual(await builderDraft(), draftBeforeClose, `${kind} X preserves the application draft`);
        await page.locator('#' + previewTrigger).click();
        await page.locator('#experience-preview').getByRole('searchbox').fill('Temporary preview search');
        // A focused search input can consume Escape to clear its text first.
        await page.getByRole('button', { name: 'Close preview', exact: true }).focus();
        await page.keyboard.press('Escape');
        await page.locator('#experience-preview').waitFor({ state: 'hidden' });
        assert.deepEqual(previewDialogs, [], `${kind} Escape closes without a discard prompt`);
        assert.deepEqual(await builderDraft(), draftBeforeClose, `${kind} preview controls preserve the application draft`);
        if (kind === 'requests') {
          await page.locator('#preview-open').click();
          await page.getByRole('button', { name: 'Close preview', exact: true }).click();
          await page.locator('#experience-preview').waitFor({ state: 'hidden' });
          assert.deepEqual(previewDialogs, [], 'Seerr tile preview also closes without a discard prompt');
          assert.deepEqual(await builderDraft(), draftBeforeClose);
        }
        assert.equal(await page.evaluate(() => window.anchorQaConfirmations.length), previewConfirmations, 'previews do not request in-app confirmations either');
      } finally { page.off('dialog', recordPreviewDialog); }
      // Closing a preview must not clear the real editor's unsaved state.
      const editorConfirmationStart = await page.evaluate(() => window.anchorQaConfirmations.length);
      await page.evaluate(() => { window.anchorQaConfirmationChoice = false; });
      try {
        await page.locator('#cancel-app').click();
        await page.locator('#anchor-confirm').waitFor({ state: 'hidden' });
        const editorDialogs = await page.evaluate(start => window.anchorQaConfirmations.slice(start), editorConfirmationStart);
        assert.equal(editorDialogs.length, 1, `${kind} draft is still protected by the editor`);
        assert.equal(editorDialogs[0].title, 'Discard unsaved changes?');
        assert.deepEqual(await builderDraft(), draftBeforeClose);
        assert.equal(await page.locator('#app-form').isVisible(), true);
      } finally { await page.evaluate(() => { window.anchorQaConfirmationChoice = true; }); }
      for (const theme of ['light', 'dark']) {
        await page.emulateMedia({ colorScheme: theme });
        for (const width of [320, 1440]) {
          await page.setViewportSize({ width, height: 900 });
          assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
          await auditUI(`${kind} setup ${theme} ${width}`);
          if (process.env.ANCHOR_QA_ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `${kind}-setup-${theme}-${width}.png`), fullPage: true });
        }
      }
      await page.emulateMedia({ colorScheme: 'light' }); await page.setViewportSize({ width: 1280, height: 900 });
      await page.locator('[data-step="3"]').click();
      if (await page.locator('#app-advanced summary').isVisible()) await page.locator('#app-advanced summary').click(); await page.locator('#app-check').fill('off');
      await page.locator('#app-save').click();
      await page.locator('#message').filter({ hasText: 'Application saved.' }).waitFor();
    }
    let current = await state();
    assert.equal(current.config.RequestConnections.length, 2); assert.equal(current.config.EbookLibraries.length, 2);
    assert.equal(current.keys['requests/media-one/seerr'], true); assert.equal(current.keys['ebooks/books-two/abs'], true);
    await page.locator('[data-panel="apps"]').click();
    assert.equal(await page.locator('#app-list .application-connections').count(), 4);
    for (const panel of ['apps']) {
      await page.locator(`[data-panel="${panel}"]`).click();
      for (const theme of ['light', 'dark']) for (const width of [320, 768, 1440]) {
        await page.emulateMedia({ colorScheme: theme }); await page.setViewportSize({ width, height: 900 });
        await auditUI(`${panel} cards ${theme} ${width}`);
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
        if (process.env.ANCHOR_QA_ARTIFACT_DIR) await page.locator(`[data-view="${panel}"]`).screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `${panel}-cards-${theme}-${width}.png`) });
      }
      const details = page.locator(`[data-view="${panel}"] .service-details summary`).first();
      await details.click(); await auditUI(`${panel} expanded details`); await details.click();
    }
    await page.emulateMedia({ colorScheme: 'light' }); await page.setViewportSize({ width: 1280, height: 900 });
    await page.locator('#app-list [data-app-id="media-one"]').getByRole('button', { name: 'Edit', exact: true }).click();
    await page.locator('[data-step="2"]').click();
    assert.equal(await page.locator('#builder-key-input-seerr').inputValue(), '');
    assert.match(await page.locator('#builder-key-input-seerr').getAttribute('placeholder'), /Saved/);
    await page.locator('[data-step="3"]').click(); await page.locator('#app-save').click();
    await page.locator('#message').filter({ hasText: 'Application saved.' }).waitFor();
    assert.equal((await state()).keys['requests/media-one/seerr'], true);
    await context.route('**/api/requests**', route => {
      const app = new URL(route.request().url()).searchParams.get('app');
      return route.fulfill({ json: { rows: [
        { title: 'Movie for ' + app, kind: 'movie', state: 'ready' },
        { title: 'Completed show', kind: 'show', year: 2013, state: 'ready' },
        { title: 'Ongoing show', kind: 'show', state: 'partial', ready: 13 },
        { title: 'Future movie', kind: 'movie', state: 'soon' },
        { title: 'Shōgun', kind: 'show', state: 'downloading' },
        { title: 'Waiting', kind: 'movie', state: 'pending' },
      ], plexOk: true, age: 0, coming: { rows: [{ title: 'Upcoming show', kind: 'show', date: '2026-12-01', note: 'episodes', count: 2 }], missing: [] } } });
    });
    await context.route('**/api/ebooks**', route => {
      const url = new URL(route.request().url());
      if (url.pathname.endsWith('/cover')) return route.fulfill({ status: 404, body: '' });
      return route.fulfill({ json: { rows: [{ id: 'same-book-id', title: 'Book for ' + url.searchParams.get('app'), format: 'epub' }], age: 0 } });
    });
    await page.goto(origin + '/'); await page.locator('#sections .tile').first().waitFor();
    for (const id of ['media-one', 'media-two', 'media-one']) {
      await page.locator(`[data-id="${id}"] .req-open`).click();
      await page.locator('#req-list').filter({ hasText: 'Movie for ' + id }).waitFor();
      assert.equal(await page.locator('#req-title').textContent(), 'Request status');
      assert.equal(await page.locator('#req-tabs').isVisible(), true);
      assert.match(await page.locator('#req-list').textContent(), /13 episodes ready/);
      await page.locator('#req-search').fill('shogun');
      assert.equal(await page.locator('#req-list .req-row:visible').count(), 1);
      await page.locator('#req-tab-coming').click();
      assert.match(await page.locator('#req-list').textContent(), /2 new episodes/);
      await page.locator('#req-tab-requests').click();
      assert.equal(await page.locator('#req-search').inputValue(), '');
      if (process.env.ANCHOR_QA_ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, 'request-dashboard.png'), fullPage: true });
      await page.keyboard.press('Escape');
    }
    for (const id of ['books-one', 'books-two', 'books-one']) {
      await page.locator(`[data-id="${id}"] .tile-link`).click();
      await page.locator('#ebooks-list').filter({ hasText: 'Book for ' + id }).waitFor();
      assert.equal(await page.locator('#ebooks-list .ebook-download').getAttribute('href'), `/api/ebooks/same-book-id/download?app=${id}`);
      await page.keyboard.press('Escape');
    }
    await context.unroute('**/api/requests**'); await context.unroute('**/api/ebooks**');
    // Existing singular configurations keep their source and stored keys on edit.
    await save(cfg => { cfg.Requests = { App: 'plex', Seerr: 'https://legacy.example.test', Plex: 'https://plex.example.test' }; cfg.Ebooks = { App: 'plex', Server: 'https://legacy-books.example.test', LibraryID: 'legacy-books' }; });
    current = await state();
    assert.equal((await post('/api/admin/secrets', { revision: current.revision, values: { seerr: 'synthetic-legacy-seerr', plex: 'synthetic-legacy-plex', abs: 'synthetic-legacy-abs' } })).status, 200);
    await page.goto(origin + '/manage.html'); await page.locator('#content').waitFor({ state: 'visible' });
    await page.locator('#app-list .application-card').filter({ hasText: 'QA renamed application' }).getByRole('button', { name: 'Edit', exact: true }).click();
    await page.locator('#app-save').click();
    await page.locator('#message').filter({ hasText: 'Application saved.' }).waitFor();
    assert.equal((await state()).config.Requests.App, 'plex');
    assert.equal((await state()).config.Ebooks.LibraryID, 'legacy-books');
    await page.locator('#app-list [data-app-id="plex"]').getByRole('button', {name: 'Edit', exact: true}).click();
    await page.locator('[data-step="2"]').click();
    assert.equal(await page.locator('#builder-requests').isVisible(), true);
    assert.equal(await page.locator('#builder-ebooks').isVisible(), true);
    await page.locator('#builder-abs-library').fill('legacy-books-updated');
    await page.locator('#app-save').click();
    await page.locator('#message').filter({ hasText: 'Application saved.' }).waitFor();
    assert.equal((await state()).config.Ebooks.LibraryID, 'legacy-books-updated');
    assert.equal((await state()).config.Requests.Seerr, 'https://legacy.example.test');
    for (const key of ['seerr', 'plex', 'abs']) assert.equal((await state()).keys[key], true);
    await page.locator('#app-list [data-app-id="plex"]').getByRole('button', {name: 'Edit', exact: true}).click();
    await page.locator('[data-step="2"]').click();
    await page.locator('#edit-application-type summary').click();
    await page.locator('input[name="application-type"][value="web"]').check();
    await page.evaluate(() => {window.anchorQaConfirmationChoice = null;});
    try {
      await page.locator('#app-save').click();
      await page.locator('#anchor-confirm').waitFor({state: 'visible'});
      assert.equal(await page.locator('#anchor-confirm-title').textContent(), 'Remove application connections?');
      await page.getByRole('button', {name: 'Keep editing', exact: true}).click();
      assert.equal((await state()).config.Requests.App, 'plex');
      assert.equal((await state()).config.Ebooks.LibraryID, 'legacy-books-updated');
      await page.locator('#app-save').click();
      await page.getByRole('button', {name: 'Save and remove connections', exact: true}).click();
      await page.locator('#message').filter({ hasText: 'Application saved.' }).waitFor();
      assert.equal((await state()).config.Requests, null);
      assert.equal((await state()).config.Ebooks, null);
      for (const key of ['seerr', 'plex', 'abs']) assert.equal((await state()).keys[key], false);
    } finally {await page.evaluate(() => {window.anchorQaConfirmationChoice = true;});}

    current = await state();
    assert.equal((await post('/api/admin/config', { revision: current.revision, config: originalConfig })).status, 200);
    assert.equal((await state()).keys['ebooks/books-two/abs'], undefined);
    await page.reload(); await page.locator('#content').waitFor({ state: 'visible' });
  });
  await t.test('arrange dashboard supports drag, exact placement, drafts, conflicts and touch', async () => {
    const original = (await state()).config;
    await save(cfg => {
      cfg.Sections = ['Movies', 'Books', 'Tools'];
      cfg.Apps = ['Alpha', 'Beta', 'Charlie', 'Delta', 'Echo', 'Foxtrot', 'Golf', 'Hotel'].map((name, i) => ({ ID: 'order-' + name.toLowerCase(), Name: name, URL: 'https://apps.example.test/' + name.toLowerCase(), Icon: 'plex', Description: 'Preserve ' + name, Section: i < 3 ? 'Movies' : 'Books', Access: ['everyone'], Check: 'off', HomeScreen: false, Disabled: name === 'Charlie' }));
    });
    try {
      await page.goto(origin + '/manage.html'); await page.locator('#content').waitFor({ state: 'visible' });
      await page.getByRole('tab', { name: 'Manage applications' }).focus(); await page.keyboard.press('ArrowRight');
      assert.equal(await page.getByRole('tab', { name: 'Arrange dashboard' }).getAttribute('aria-selected'), 'true');
      const sectionIDs = name => page.locator(`[data-order-section="${name}"] [data-order-app]`).evaluateAll(tiles => tiles.map(tile => tile.dataset.orderApp));
      const handle = name => page.getByRole('button', { name: 'Move ' + name, exact: true });
      const dragBefore = async (name, targetID) => {
        await handle(name).scrollIntoViewIfNeeded();
        const from = await handle(name).boundingBox(), to = await page.locator(`[data-order-app="${targetID}"]`).boundingBox();
        await page.mouse.move(from.x+from.width/2, from.y+from.height/2); await page.mouse.down();
        await page.mouse.move(from.x+from.width/2-15, from.y+from.height/2, { steps: 3 });
        await page.mouse.move(to.x+12, to.y+to.height/2, { steps: 10 }); await page.mouse.up();
      };
      await dragBefore('Beta', 'order-alpha');
      assert.deepEqual(await sectionIDs('Movies'), ['order-beta', 'order-alpha', 'order-charlie']);
      assert.equal((await state()).config.Apps[0].ID, 'order-alpha', 'dragging only changes a draft');
      await page.route('**/api/admin/config', route => route.fulfill({ status: 409, json: { message: 'Settings changed. Reload before saving.' } }), { times: 1 });
      await page.getByRole('button', { name: 'Save order', exact: true }).click();
      await page.locator('.order-status.error').filter({ hasText: 'arrangement has been kept' }).waitFor();
      assert.deepEqual(await sectionIDs('Movies'), ['order-beta', 'order-alpha', 'order-charlie']);
      await page.getByRole('button', { name: 'Discard changes', exact: true }).click();
      assert.deepEqual(await sectionIDs('Movies'), ['order-alpha', 'order-beta', 'order-charlie']);
      await handle('Beta').focus(); await page.keyboard.press('Enter');
      await page.locator('.order-move-dialog').waitFor({ state: 'visible' });
      await page.locator('.order-move-section').selectOption('Books'); await page.locator('.order-move-position').selectOption('order-delta');
      await page.locator('#order-move-form button[type=submit]').click();
      assert.equal((await sectionIDs('Books'))[0], 'order-beta');
      for (const theme of ['light', 'dark']) for (const width of [320, 768, 1440]) {
        await page.emulateMedia({ colorScheme: theme }); await page.setViewportSize({ width, height: 900 });
        await auditUI(`arrangement grid ${theme} ${width}`);
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
        if (process.env.ANCHOR_QA_ARTIFACT_DIR) await page.locator('#applications-arrange').screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `arrange-${theme}-${width}.png`) });
      }
      await page.emulateMedia({ colorScheme: 'light' }); await page.setViewportSize({ width: 1280, height: 900 });
      await handle('Beta').click(); await auditUI('exact placement dialog'); await page.keyboard.press('Escape');
      await page.getByRole('button', { name: 'Save order', exact: true }).click();
      await page.locator('.order-status').filter({ hasText: 'Dashboard order saved.' }).waitFor();
      const saved = (await state()).config;
      assert.equal(saved.Apps.find(app => app.ID === 'order-beta').Section, 'Books');
      assert.equal(saved.Apps.find(app => app.ID === 'order-charlie').Disabled, true);
      assert.equal(saved.Apps.find(app => app.ID === 'order-beta').HomeScreen, false);
      assert.equal(new Set(saved.Apps.map(app => app.ID)).size, 8);
      const dashboard = await page.evaluate(() => fetch('/api/me').then(response => response.json()));
      assert.equal(dashboard.sections.find(section => section.name === 'Books').apps[0].id, 'order-beta');
      await page.reload(); await page.getByRole('tab', { name: 'Arrange dashboard' }).click();
      assert.equal((await sectionIDs('Books'))[0], 'order-beta');
      // Exercise the pointer handler with genuine browser touch events, including
      // a destination in a previously empty section and a long-page scroll.
      await page.setViewportSize({ width: 390, height: 900 });
      await handle('Alpha').scrollIntoViewIfNeeded();
      const scrollStart = await page.evaluate(() => scrollY), cancelFrom = await handle('Alpha').boundingBox();
      await page.mouse.move(cancelFrom.x+22, cancelFrom.y+22); await page.mouse.down();
      await page.mouse.move(cancelFrom.x+10, 890, { steps: 8 });
      await page.waitForFunction(start => scrollY > start+25, scrollStart);
      await page.keyboard.press('Escape'); await page.mouse.up();
      assert.equal(await page.locator('.order-ghost').count(), 0);
      assert.equal(await page.locator('.order-move-dialog').isVisible(), false);
      assert.equal(await page.getByRole('button', { name: 'Save order', exact: true }).isDisabled(), true, 'cancelled drag leaves saved order unchanged');
      await handle('Delta').scrollIntoViewIfNeeded();
      const from = await handle('Delta').boundingBox();
      const touch = await context.newCDPSession(page);
      await touch.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: from.x+22, y: from.y+22 }] });
      await touch.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: from.x+10, y: from.y+10 }] });
      await page.locator('[data-order-section="Tools"] .order-empty').scrollIntoViewIfNeeded();
      const to = await page.locator('[data-order-section="Tools"] .order-empty').boundingBox();
      await touch.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: to.x+to.width/2, y: to.y+to.height/2 }] });
      await touch.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] }); await touch.detach();
      assert.deepEqual(await sectionIDs('Tools'), ['order-delta']);
      await page.getByRole('button', { name: 'Discard changes', exact: true }).click();
      assert.deepEqual(await sectionIDs('Tools'), []);
      await page.setViewportSize({ width: 1280, height: 900 });
    } finally {
      await page.unroute('**/api/admin/config');
      const current = await state();
      assert.equal((await post('/api/admin/config', { revision: current.revision, config: original })).status, 200);
      await page.goto(origin + '/manage.html'); await page.locator('#content').waitFor({ state: 'visible' });
    }
  });
  await t.test('Tailscale photo renders in the signed-in header and clears when absent', async () => {
    const photoURL = 'https://profiles.example.test/qa-avatar.png';
    const image = await fs.readFile(path.join(__dirname, '../web/brand/icon-192.png'));
    const addPhoto = async route => {
      const response = await route.fetch();
      return route.fulfill({ response, json: { ...await response.json(), picture: photoURL } });
    };
    await context.route(photoURL, route => route.fulfill({ contentType: 'image/png', body: image }));
    await context.route('**/api/me', addPhoto);
    try {
      await page.goto(origin + '/');
      await page.locator('#who-pic').waitFor({ state: 'visible' });
      assert.equal(await page.locator('#who-pic').getAttribute('src'), photoURL);
      assert.equal(await page.locator('#who-pic').getAttribute('referrerpolicy'), 'no-referrer');
      await page.waitForFunction(() => document.getElementById('who-pic').naturalWidth > 0);
      assert.equal(await page.locator('#who-name').count(), 0, 'the greeting supplies the name without a repeated header label');
      assert.match(await page.locator('#who').getAttribute('aria-label'), /QA Administrator/);
      assert.match(await page.locator('#who').getAttribute('title'), /QA Administrator/);
      for (const theme of ['light', 'dark']) for (const width of [320, 390, 768, 1440]) {
        await page.emulateMedia({ colorScheme: theme }); await page.setViewportSize({ width, height: 900 });
        const geometry = await page.evaluate(() => {
          const box = selector => { const r = document.querySelector(selector).getBoundingClientRect(); return { left: r.left, right: r.right, top: r.top, bottom: r.bottom }; };
          return { avatar: box('#who-pic'), brand: box('.brand'), nav: box('.auth-nav'), header: box('.top') };
        });
        assert.ok(Math.abs(geometry.avatar.right - geometry.header.right) < 1, `${width}px avatar stays at the far right`);
        if (width <= 560) {
          assert.ok(geometry.avatar.top < geometry.brand.bottom && geometry.avatar.bottom > geometry.brand.top, 'mobile avatar shares the brand row');
          assert.ok(geometry.nav.top >= geometry.avatar.bottom, 'mobile navigation wraps below the avatar');
        } else assert.ok(geometry.avatar.left >= geometry.nav.right, 'desktop avatar follows the navigation');
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
        if (process.env.ANCHOR_QA_ARTIFACT_DIR) await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `profile-photo-${theme}-${width}.png`) });
      }
      await context.route(photoURL, route => route.fulfill({ status: 404, body: 'Not found' }));
      await page.reload();
      await page.waitForFunction(() => {
        const photo = document.getElementById('who-pic');
        return photo.hasAttribute('src') && photo.complete && photo.naturalWidth === 0 && photo.hidden && !document.getElementById('who').hidden;
      });
    } finally {
      await context.unroute('**/api/me', addPhoto); await context.unroute(photoURL);
      await page.emulateMedia({ colorScheme: 'light' }); await page.setViewportSize({ width: 1280, height: 900 });
    }
    await page.reload(); await page.locator('#greeting').filter({ hasText: 'QA' }).waitFor();
    assert.equal(await page.locator('#who').isVisible(), true);
    assert.equal(await page.locator('#who span').textContent(), 'QA');
    assert.equal(await page.locator('#who-pic').isVisible(), false);
    assert.equal(await page.locator('#who-pic').getAttribute('src'), null);
  });
  await t.test('administrators and members upload private shared profile photos without losing sessions or drafts', async () => {
    const png = await fs.readFile(path.join(__dirname, '../web/brand/icon-192.png'));
    const saved = await state(), administrator = saved.users.find(user => user.username === 'qa-admin');
    const adminCookie = (await context.cookies()).find(cookie => cookie.name === 'anchor_session').value;
    const choose = (target, selector) => target.locator(selector + ' input[type=file]').setInputFiles({ name: 'profile.png', mimeType: 'image/png', buffer: png });
    await page.goto(origin + '/manage.html'); await page.locator('[data-panel="users"]').click();
    await page.getByRole('button', { name: 'Edit ' + administrator.name, exact: true }).click();
    await page.locator('#user-name').fill('Unpublished name draft');
    await choose(page, '#user-photo');
    await page.locator('#user-photo').getByRole('button', { name: 'Upload photo', exact: true }).click();
    await page.locator('#user-photo [role=status]').filter({ hasText: 'Profile photo saved.' }).waitFor();
    assert.equal(await page.locator('#user-name').inputValue(), 'Unpublished name draft');
    assert.equal(await page.locator('#user-photo').getByRole('button', { name: 'Upload photo', exact: true }).isDisabled(), true);
    const withPhoto = (await state()).users.find(user => user.id === administrator.id);
    assert.equal(withPhoto.name, administrator.name); assert.equal(withPhoto.custom_picture, true);
    assert.equal((await context.cookies()).find(cookie => cookie.name === 'anchor_session').value, adminCookie);
    await page.locator('#user-photo input[type=file]').setInputFiles({ name: 'fake.png', mimeType: 'image/png', buffer: Buffer.from('<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>') });
    await page.locator('#user-photo').getByRole('button', { name: 'Upload photo', exact: true }).click();
    await page.locator('#user-photo [role=alert]').filter({ hasText: 'valid PNG or JPEG' }).waitFor();
    assert.equal((await state()).users.find(user => user.id === administrator.id).picture, withPhoto.picture);
    await page.locator('#cancel-user').click(); await page.locator('#user-dialog').waitFor({ state: 'hidden' });
    await page.goto(origin + '/'); await page.locator('#who-pic').waitFor({ state: 'visible' });
    assert.equal(await page.locator('#who-pic').getAttribute('src'), withPhoto.picture);
    await page.goto(origin + '/manage.html'); await page.locator('[data-panel="users"]').click();
    const card = page.locator('.person-card').filter({ has: page.locator(`[data-user-id="${administrator.id}"]`) });
    assert.equal(await card.locator('.person-avatar img').getAttribute('src'), withPhoto.picture);
    await page.getByRole('button', { name: 'Edit QA Member', exact: true }).click();
    await choose(page, '#user-photo'); await page.locator('#user-photo').getByRole('button', { name: 'Upload photo', exact: true }).click();
    await page.locator('#user-photo [role=status]').filter({ hasText: 'Profile photo saved.' }).waitFor();
    await page.locator('#cancel-user').click(); await page.locator('#user-dialog').waitFor({ state: 'hidden' });
    const memberPhoto = (await state()).users.find(user => user.id === member.id);
    const memberContext = await browser.newContext();
    await memberContext.route('**/*', route => new URL(route.request().url()).origin === origin ? route.continue() : route.abort());
    try {
      const login = await memberContext.request.post(origin + '/api/auth/login', { headers: { Origin: origin }, data: { Username: 'qa-member', Password: password } });
      assert.equal(login.status(), 200);
      const own = await memberContext.newPage(); own.on('pageerror', error => exceptions.push(error.message));
      own.on('dialog', async dialog => { exceptions.push('Unexpected browser confirmation: ' + dialog.message()); await dialog.dismiss(); });
      await own.goto(origin + '/'); await own.locator('#who-pic').waitFor({ state: 'visible' });
      assert.equal(await own.locator('#who-pic').getAttribute('src'), memberPhoto.picture);
      assert.equal((await memberContext.request.get(origin + withPhoto.picture)).status(), 404, 'member cannot retrieve the administrator image');
      await own.getByRole('link', { name: 'My account', exact: true }).click();
      await choose(own, '#account-photo'); await own.locator('#account-photo').getByRole('button', { name: 'Upload photo', exact: true }).click();
      await own.locator('#account-photo [role=status]').filter({ hasText: 'Profile photo saved.' }).waitFor();
      assert.equal((await (await memberContext.request.get(origin + '/api/auth/status')).json()).authenticated, true);
      assert.equal(await own.locator('#account-avatar img').getAttribute('src'), memberPhoto.picture);
      for (const theme of ['light', 'dark']) for (const width of [320, 1440]) {
        await own.emulateMedia({ colorScheme: theme }); await own.setViewportSize({ width, height: 900 });
        assert.ok(await own.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
        if (process.env.ANCHOR_QA_ARTIFACT_DIR) await own.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `member-photo-${theme}-${width}.png`), fullPage: true });
      }
      await own.locator('#account-photo').getByRole('button', { name: 'Use default photo', exact: true }).click();
      await own.locator('#anchor-confirm-cancel').click();
      assert.equal((await (await memberContext.request.get(origin + '/api/auth/status')).json()).user.custom_picture, true);
      await own.locator('#account-photo').getByRole('button', { name: 'Use default photo', exact: true }).click();
      await own.locator('#anchor-confirm-accept').click();
      await own.locator('#account-photo [role=status]').filter({ hasText: 'Default profile photo restored.' }).waitFor();
      assert.equal(await own.locator('#account-avatar span').textContent(), 'QM');
      assert.equal((await (await memberContext.request.get(origin + '/api/auth/status')).json()).authenticated, true);
    } finally { await memberContext.close(); }
    await page.getByRole('button', { name: 'Edit ' + administrator.name, exact: true }).click();
    await page.locator('#user-photo').getByRole('button', { name: 'Use default photo', exact: true }).click();
    await page.locator('#user-photo [role=status]').filter({ hasText: 'Default profile photo restored.' }).waitFor();
    await page.locator('#cancel-user').click(); await page.locator('#user-dialog').waitFor({ state: 'hidden' });
  });
  await t.test('Tailscale status explains HTTPS sign-in, saved links and simulations without claiming network-wide success', async () => {
    const saved = await state(), admin = saved.users.find(user => user.username === 'qa-admin');
    const webRoot = path.resolve(__dirname, '../web');
    // These intercepted origins test rendering, not a live Serve deployment.
    for (const [scheme, method, observed, simulated, expected] of [
      ['https', 'tailscale', admin.tailscale, false, 'verified'],
      ['http', 'tailscale', admin.tailscale, false, 'insecure'],
      ['https', 'local', 'unknown@example.test', false, 'unlinked'],
      ['https', 'local', '', false, 'linked'],
      ['https', 'tailscale', admin.tailscale, true, 'simulated'],
    ]) {
      // .ts.net is HTTPS-only in Chromium's HSTS list. Use a reserved test
      // hostname for the deliberately insecure browser case.
      const base = scheme === 'http' ? 'http://anchor-ui.example.test' : 'https://anchor-ui.example.ts.net';
      const uiContext = await browser.newContext();
      const uiErrors = [];
      await uiContext.addInitScript(() => {
        window.copiedAnchorAddresses = [];
        Object.defineProperty(navigator, 'clipboard', { value: { writeText: async value => window.copiedAnchorAddresses.push(value) }, configurable: true });
      });
      await uiContext.route('**/*', async route => {
        const url = new URL(route.request().url());
        if (url.origin !== base) return route.abort();
        if (url.pathname === '/api/auth/status') return route.fulfill({ headers: simulated ? { 'X-Anchor-Dev-Identities': '1' } : {}, json: { authenticated: true, admin: true, mode: 'tailscale', method, user: admin, csrf: 'synthetic-ui-only' } });
        if (url.pathname === '/api/admin/state') return route.fulfill({ json: { ...saved, tailscale_identity: observed } });
        if (url.pathname === '/api/tailscale-test') return route.fulfill({ json: { state: expected === 'linked' ? 'missing' : expected === 'insecure' ? 'verified' : expected, title: 'Tailscale connection test', detail: 'Connect this device to Tailscale and use the HTTPS Serve address.', identity: observed, name: method === 'tailscale' ? admin.name : '', groups: admin.groups, sections: [], checked_at: new Date().toISOString() } });
        const filename = path.resolve(webRoot, '.' + url.pathname);
        if (!filename.startsWith(webRoot + path.sep)) return route.abort();
        try {
          const contentType = { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml', '.png': 'image/png' }[path.extname(filename)] || 'application/octet-stream';
          return route.fulfill({ contentType, body: await fs.readFile(filename) });
        } catch { return route.abort(); }
      });
      try {
        const uiPage = await uiContext.newPage(); uiPage.on('pageerror', error => uiErrors.push(error.message));
        await uiPage.goto(base + '/manage.html');
        await uiPage.locator('[data-panel="security"]').click(); await uiPage.locator('#connection-tailscale-tab').click();
        await uiPage.locator(`#tailscale-verification[data-state="${expected}"]`).waitFor();
        await uiPage.locator('#verify-tailscale').click();
        await uiPage.locator(`#tailscale-check-result[data-state="${expected === 'linked' ? 'missing' : expected}"]`).waitFor({ state: 'visible' });
        assert.equal(await uiPage.locator('#tailscale-address').isEditable(), true);
        if (expected === 'verified') {
          assert.match(await uiPage.locator('#tailscale-verification').textContent(), /QA Administrator on this device/);
          await uiPage.locator('#copy-tailscale-address').click();
          assert.deepEqual(await uiPage.evaluate(() => window.copiedAnchorAddresses), [base]);
        }
        assert.deepEqual(uiErrors, []);
      } finally { await uiContext.close(); }
    }
  });
  await t.test('guided Tailscale test opens a safe tab, reports effective access and keeps previews separate', async () => {
    const saved = await state(), admin = saved.users.find(user => user.username === 'qa-admin');
    const setupOrigin = 'http://setup.example.test', serveOrigin = 'https://anchor-check.example.ts.net';
    const webRoot = path.resolve(__dirname, '../web'), ui = await browser.newContext();
    const errors = [];
    let submitted = null;
    const report = { state: 'verified', title: 'Automatic sign-in verified', detail: 'Recognized through Tailscale.', identity: 'member@example.test', name: 'Test Member', groups: ['family'], checked_at: new Date().toISOString(), address: serveOrigin, sections: [{ name: 'Media', apps: [{ name: 'Photos', status: 'up' }, { name: 'Music', maintenance: 'Updating', locked: true }] }] };
    await ui.addInitScript(() => {
      window.copiedTestLinks = [];
      Object.defineProperty(navigator, 'clipboard', { value: { writeText: async value => window.copiedTestLinks.push(value) }, configurable: true });
    });
    await ui.route('**/*', async route => {
      const url = new URL(route.request().url());
      if (![setupOrigin, serveOrigin].includes(url.origin)) return route.abort();
      if (url.pathname === '/api/auth/status') return route.fulfill({ json: { authenticated: true, admin: true, mode: 'tailscale', method: 'local', user: admin, csrf: 'ui-only' } });
      if (url.pathname === '/api/admin/state') return route.fulfill({ json: { ...saved, setup: { ...saved.setup, completed: true }, tailscale_identity: '' } });
      if (url.pathname === '/api/admin/tailscale-test') {
        if (route.request().method() === 'POST') return route.fulfill({ json: { ticket: 'ui-only-ticket', url: serveOrigin + '/tailscale-test.html#ticket=ui-only-ticket' } });
        return route.fulfill({ json: { report: submitted } });
      }
      if (url.pathname === '/api/tailscale-test') {
        assert.equal(route.request().headers().cookie, undefined, 'test does not rely on local password cookies');
        if (url.origin === setupOrigin) return route.fulfill({ json: { state: 'missing', title: "Tailscale sign-in wasn't detected", detail: 'Connect this device to Tailscale and open the HTTPS Serve address.' } });
        if (url.searchParams.has('ticket')) submitted = report;
        return route.fulfill({ json: report });
      }
      if (url.pathname === '/api/me') return route.fulfill({ json: { login: admin.id, sections: report.sections } });
      const filename = path.resolve(webRoot, '.' + url.pathname);
      if (!filename.startsWith(webRoot + path.sep)) return route.abort();
      try { return route.fulfill({ contentType: { '.html': 'text/html', '.js': 'text/javascript', '.css': 'text/css', '.svg': 'image/svg+xml' }[path.extname(filename)] || 'application/octet-stream', body: await fs.readFile(filename) }); }
      catch { return route.abort(); }
    });
    try {
      const setup = await ui.newPage(); setup.on('pageerror', error => errors.push(error.message));
      await setup.goto(setupOrigin + '/manage.html');
      await setup.locator('[data-panel="security"]').click(); await setup.locator('#connection-tailscale-tab').click();
      if (!await setup.locator('#tailscale-guide').isVisible()) await setup.locator('#start-tailscale').click();
      await setup.locator('#verify-tailscale').click();
      await setup.locator('#tailscale-check-result[data-state="missing"]').waitFor();
      assert.match(await setup.locator('#tailscale-check-result').textContent(), /Connect this device to Tailscale/);
      await setup.locator('#open-tailscale-test').click();
      assert.match(await setup.locator('#tailscale-check-result').textContent(), /Enter Anchor's Tailscale HTTPS address/);
      await setup.locator('#tailscale-address').fill(serveOrigin);
      const popup = setup.waitForEvent('popup');
      await setup.locator('#open-tailscale-test').click();
      const testTab = await popup; testTab.on('pageerror', error => errors.push(error.message));
      await testTab.locator('#test-result[data-state="verified"]').waitFor();
      assert.equal(await testTab.evaluate(() => window.opener), null);
      assert.equal(new URL(testTab.url()).hash, '');
      await setup.locator('#tailscale-check-result[data-state="verified"]').waitFor();
      assert.match(await setup.locator('#tailscale-check-result').innerText(), /Test Member[\s\S]*family[\s\S]*Photos[\s\S]*Music[\s\S]*Maintenance/);
      assert.equal(setup.url(), setupOrigin + '/manage.html');
      await testTab.locator('#copy-test-link').click();
      assert.deepEqual(await testTab.evaluate(() => window.copiedTestLinks), [serveOrigin + '/tailscale-test.html']);
      await setup.locator('summary').filter({ hasText: "Preview someone's configured access" }).click();
      await setup.locator('#tailscale-preview').click();
      await setup.locator('#tailscale-preview-result[data-state="preview"]').waitFor();
      assert.match(await setup.locator('#tailscale-preview-result').innerText(), /does not verify/);
      for (const theme of ['light', 'dark']) for (const width of [320, 1440]) {
        await setup.emulateMedia({ colorScheme: theme }); await setup.setViewportSize({ width, height: 900 });
        assert.ok(await setup.evaluate(() => document.documentElement.scrollWidth <= innerWidth), 'test results reflow');
        if (process.env.ANCHOR_QA_ARTIFACT_DIR) await setup.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `tailscale-test-result-${theme}-${width}.png`), fullPage: true });
      }
      assert.deepEqual(errors, []);
    } finally { await ui.close(); }
  });
  await t.test('development identity picker switches real accounts without Tailscale or passwords', { skip: !process.env.ANCHOR_SIMULATOR_BINARY }, async () => {
    const before = await state();
    const admin = before.users.find(user => user.username === 'qa-admin');
    const member = before.users.find(user => user.username === 'wizard-member');
    const simulatorDir = path.join(dir, 'identity-simulation'); await fs.mkdir(simulatorDir);
    // Use the application's consistent backup of this disposable test DB.
    const backup = await page.evaluate(async () => {
      const response = await fetch('/api/admin/backup', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
      if (!response.ok) throw new Error('Could not clone the synthetic identity fixture');
      return Array.from(new Uint8Array(await response.arrayBuffer()));
    });
    await fs.writeFile(path.join(simulatorDir, 'anchor.db'), Buffer.from(backup));
    const simulatorOrigin = `http://127.0.0.1:${await port()}`;
    start(process.env.ANCHOR_SIMULATOR_BINARY, [], { ANCHOR_CONFIG_DIR: simulatorDir, ANCHOR_MODE: 'tailscale', ANCHOR_LISTEN: simulatorOrigin.slice(7), ANCHOR_DEV_IDENTITIES: '1' });
    await ready(simulatorOrigin + '/healthz');
    assert.equal((await context.request.get(origin + '/__dev/identity-state')).status(), 404, 'production binary has no identity picker');
    const simContext = await browser.newContext();
    const secondContext = await browser.newContext();
    await Promise.all([simContext, secondContext].map(c => c.route('**/*', route => new URL(route.request().url()).origin === simulatorOrigin ? route.continue() : route.abort())));
    try {
      const simPage = await simContext.newPage();
      const simErrors = []; simPage.on('pageerror', error => simErrors.push(error.message));
      async function choose(target, id) {
        await target.goto(simulatorOrigin + '/__dev/identities');
        await target.locator('#identity:not([disabled])').waitFor();
        await target.locator('#identity').selectOption(id);
        await target.locator('#identity-form button').click();
      }
      await choose(simPage, admin.id);
      await simPage.locator('#greeting').filter({ hasText: admin.name.split(' ')[0] }).waitFor();
      assert.equal((await simContext.request.get(simulatorOrigin + '/api/admin/state')).status(), 200);
      assert.equal(await simPage.getByRole('link', { name: 'Change test user' }).isVisible(), true);
      await simPage.goto(simulatorOrigin + '/manage.html');
      await simPage.locator('[data-panel="security"]').click(); await simPage.locator('#connection-tailscale-tab').click();
      await simPage.locator('#tailscale-verification[data-state="simulated"]').waitFor();
      assert.equal(await simPage.locator('#tailscale-address').inputValue(), '');
      await simPage.locator('#verify-tailscale').click();
      await simPage.locator('#tailscale-check-result[data-state="simulated"]').waitFor({ state: 'visible' });
      assert.match(await simPage.locator('#tailscale-check-result').textContent(), /Development simulation/);
      await simPage.locator('[data-panel="security"]').click();
      assert.equal(await simPage.locator('#sign-out').isVisible(), false, 'Tailscale sessions do not offer local sign-out');
      await simPage.goto(simulatorOrigin + '/account.html');
      await simPage.locator('#profile-method').filter({ hasText: 'simulated Tailscale identity' }).waitFor();
      await choose(simPage, member.id);
      await simPage.locator('#greeting').filter({ hasText: member.name.split(' ')[0] }).waitFor();
      assert.equal((await simContext.request.get(simulatorOrigin + '/api/admin/state')).status(), 403);
      assert.equal(await simPage.getByRole('link', { name: 'Administration', exact: true }).count(), 0);
      await simPage.getByRole('link', { name: 'My account', exact: true }).click();
      await simPage.getByText('Local sign-in details', { exact: true }).click();
      assert.equal(await simPage.locator('#current-password-field').isVisible(), false);
      assert.equal(await simPage.locator('#sign-out').isVisible(), false);
      const accountURL = simPage.url();
      await simPage.locator('#local-username').fill('wizard-member-updated');
      await simPage.locator('#new-password').fill('updated-member-password');
      await simPage.locator('#change-password button[type=submit]').click();
      await simPage.locator('#message').filter({ hasText: 'Your Tailscale sign-in stays active.' }).waitFor();
      assert.equal(simPage.url(), accountURL);
      assert.equal(await simPage.locator('#profile').isVisible(), true);
      assert.equal(await simPage.locator('#new-password').inputValue(), '');
      const updatedStatus = await (await simContext.request.get(simulatorOrigin + '/api/auth/status')).json();
      assert.equal(updatedStatus.authenticated, true); assert.equal(updatedStatus.method, 'tailscale');
      assert.equal(updatedStatus.user.id, member.id); assert.equal(updatedStatus.user.username, 'wizard-member-updated');
      if (process.env.ANCHOR_QA_ARTIFACT_DIR) await simPage.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, 'tailscale-member-credentials.png'), fullPage: true });
      await simPage.locator('#local-username').fill(member.username); await simPage.locator('#new-password').fill(password);
      await Promise.all([simPage.waitForResponse(response => response.url().endsWith('/api/auth/account') && response.status() === 200), simPage.locator('#change-password button[type=submit]').click()]);

      const secondPage = await secondContext.newPage(); await choose(secondPage, admin.id);
      await secondPage.locator('#greeting').filter({ hasText: admin.name.split(' ')[0] }).waitFor();
      const status = await (await simContext.request.get(simulatorOrigin + '/api/auth/status')).json();
      assert.equal(status.user.id, member.id); assert.equal(status.method, 'tailscale', 'uses actual Serve-header authentication');
      await simPage.getByRole('link', { name: 'Change test user' }).click();
      await simPage.locator('#identity:not([disabled])').waitFor();
      for (const theme of ['light', 'dark']) for (const width of [320, 1440]) {
        await simPage.emulateMedia({ colorScheme: theme }); await simPage.setViewportSize({ width, height: 900 });
        if (axeSource) {
          await simPage.evaluate(axeSource);
          const violations = await simPage.evaluate(async () => (await axe.run(document, { runOnly: { type: 'tag', values: ['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa', 'best-practice'] } })).violations.map(v => v.id));
          assert.deepEqual(violations, [], `identity picker ${theme} ${width}`);
        }
        assert.ok(await simPage.evaluate(() => document.documentElement.scrollWidth <= innerWidth));
        if (process.env.ANCHOR_QA_ARTIFACT_DIR) await simPage.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `identity-picker-${theme}-${width}.png`), fullPage: true });
      }
      for (const id of ['unlinked', 'signed-out']) {
        await choose(simPage, id); await simPage.waitForURL('**/account.html');
        assert.equal((await (await simContext.request.get(simulatorOrigin + '/api/auth/status')).json()).authenticated, false);
        assert.equal(await simPage.getByRole('link', { name: 'Change test user' }).isVisible(), true);
      }
      assert.deepEqual(simErrors, []);
      assert.deepEqual((await state()).users, before.users, 'identity choices preserve existing people');
    } finally { await simContext.close(); await secondContext.close(); }
  });
  await t.test('settings UI, optimistic conflict, export, import preview/apply and backup', async () => {
    await page.goto(origin + '/manage.html');
    await page.getByRole('button', { name: 'Dashboard settings' }).click();
    await page.locator('#settings-title').fill('QA Anchor');
    await page.locator('#settings-form button[type=submit]').click();
    await page.locator('#message').filter({ hasText: 'Dashboard settings saved.' }).waitFor();
    const before = await state();
    assert.equal(before.config.Title, 'QA Anchor');
    await save(cfg => { cfg.Subtitle = 'Concurrent update'; });
    assert.equal((await post('/api/admin/config', { revision: before.revision, config: before.config })).status, 409);
    const yaml = await page.evaluate(() => fetch('/api/admin/export').then(r => r.text()));
    assert.ok(yaml.includes('schema_version: 1')); assert.ok(!yaml.includes(password)); assert.ok(!yaml.includes('qa-member'));
    const preview = await post('/api/admin/import/preview', { yaml }); assert.equal(preview.status, 200);
    const current = await state(); assert.equal(current.config.Subtitle, 'Concurrent update');
    assert.equal((await post('/api/admin/config', { revision: current.revision, config: preview.body.config })).status, 200);
    const backup = await page.evaluate(async () => {
      const r = await fetch('/api/admin/backup', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: '{}' });
      return { status: r.status, prefix: Array.from(new Uint8Array(await r.arrayBuffer()).slice(0, 16)), cache: r.headers.get('Cache-Control') };
    });
    assert.equal(backup.status, 200); assert.equal(Buffer.from(backup.prefix).toString(), 'SQLite format 3\0'); assert.equal(backup.cache, 'no-store');
  });
  await t.test('disable/re-enable, reorder, maintenance and delete persist', async () => {
    await save(cfg => { cfg.Apps[0].Disabled = true; });
    await page.goto(origin + '/');
    await page.locator('#sections .tile').first().waitFor(); assert.equal(await page.locator('#sections .tile').count(), 1);
    await save(cfg => { cfg.Apps[0].Disabled = false; cfg.Apps[0].Maintenance = 'Synthetic maintenance'; });
    await page.goto(origin + '/manage.html'); await page.locator('#content').waitFor({ state: 'visible' });
    await page.getByRole('tab', { name: 'Arrange dashboard' }).click();
    await page.locator('.order-handle').first().click();
    await page.locator('.order-move-position').selectOption('');
    await page.locator('#order-move-form button[type=submit]').click();
    await page.getByRole('button', { name: 'Save order', exact: true }).click();
    await page.locator('.order-status').filter({ hasText: 'Dashboard order saved.' }).waitFor();
    await page.goto(origin + '/'); await page.locator('#sections .tile').first().waitFor();
    assert.equal(await page.locator('#sections .tile').count(), 2);
    assert.ok((await state()).config.Apps[1].Maintenance.includes('Synthetic'));
    await page.goto(origin + '/manage.html'); await page.locator('#content').waitFor({ state: 'visible' });
    const remove = page.locator('#app-list [data-app-id="private"]').getByRole('button', { name: 'Remove', exact: true });
    await page.evaluate(() => { window.anchorQaConfirmationChoice = null; });
    try {
      await remove.click();
      const confirmation = page.locator('#anchor-confirm');
      await confirmation.waitFor({ state: 'visible' });
      assert.equal(await confirmation.getAttribute('role'), 'alertdialog');
      assert.equal(await confirmation.locator('h2').textContent(), 'Remove application?');
      assert.equal(await page.locator('#anchor-confirm-cancel').evaluate(el => el === document.activeElement), true, 'safe action receives focus');
      assert.ok((await state()).config.Apps.some(app => app.ID === 'private'), 'opening a confirmation does not delete');
      await page.keyboard.press('Escape');
      await confirmation.waitFor({ state: 'hidden' });
      assert.equal(await remove.evaluate(el => el === document.activeElement), true, 'cancel restores focus');
      assert.ok((await state()).config.Apps.some(app => app.ID === 'private'));
      await remove.click();
      for (const theme of ['light', 'dark']) for (const width of [320, 1440]) {
        await page.emulateMedia({ colorScheme: theme }); await page.setViewportSize({ width, height: 900 });
        assert.ok(await confirmation.evaluate(el => el.scrollWidth <= el.clientWidth));
        await auditUI(`application removal confirmation ${theme} ${width}`);
        if (process.env.ANCHOR_QA_ARTIFACT_DIR) await confirmation.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `confirm-remove-${theme}-${width}.png`) });
      }
      await page.emulateMedia({ colorScheme: 'light' }); await page.setViewportSize({ width: 1280, height: 900 });
      await page.locator('#anchor-confirm-cancel').focus(); await page.keyboard.press('Shift+Tab');
      assert.equal(await page.locator('#anchor-confirm-accept').evaluate(el => el === document.activeElement), true, 'focus stays in the confirmation');
      await page.getByRole('button', { name: 'Keep application', exact: true }).click();
      await confirmation.waitFor({ state: 'hidden' });
      assert.ok((await state()).config.Apps.some(app => app.ID === 'private'));
      await remove.click();
      await page.getByRole('button', { name: 'Remove application', exact: true }).click();
      await page.locator('#message').filter({ hasText: 'Saved.' }).waitFor();
    } finally { await page.evaluate(() => { window.anchorQaConfirmationChoice = true; }); }
    await page.goto(origin + '/'); await page.locator('#sections .tile').first().waitFor(); assert.equal(await page.locator('#sections .tile').count(), 1);
  });
  await t.test('desktop, tablet, mobile dashboard/editor/account have no horizontal overflow', async () => {
    for (const width of [1440, 768, 390]) {
      await page.setViewportSize({ width, height: 900 });
      for (const pathname of ['/', '/manage.html', '/account.html']) {
        await page.goto(origin + pathname);
        await page.locator(pathname === '/' ? '#sections .tile' : pathname === '/manage.html' ? '#content' : '#profile').first().waitFor({ state: 'visible' });
        assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), `${width}px ${pathname} overflows`);
        if (process.env.ANCHOR_QA_ARTIFACT_DIR && pathname === '/') {
          await fs.mkdir(process.env.ANCHOR_QA_ARTIFACT_DIR, { recursive: true });
          await page.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, `dashboard-${width}.png`), fullPage: true });
        }
      }
    }
    await page.setViewportSize({ width: 1280, height: 900 });
  });
  await t.test('PWA install, protected cache exclusion, offline/reconnect', async () => {
    await page.goto(origin + '/');
    await page.evaluate(() => navigator.serviceWorker.ready);
    await page.reload();
    assert.ok(await page.evaluate(() => !!navigator.serviceWorker.controller));
    const cached = await page.evaluate(async () => {
      const keys = await caches.keys();
      return (await Promise.all(keys.map(async key => (await (await caches.open(key)).keys()).map(r => new URL(r.url).pathname)))).flat();
    });
    assert.ok(cached.includes('/app.js')); assert.ok(!cached.some(p => p.startsWith('/api/') || p.startsWith('/open/') || p === '/account.html' || p === '/manage.html'));
    try {
      await context.setOffline(true);
      await page.reload();
      await page.locator('#offline').waitFor({ state: 'visible' });
      assert.equal(await page.locator('#offline-steps').isVisible(), false);
    } finally {
      await context.setOffline(false);
    }
    await page.locator('#main-content').waitFor({ state: 'visible' });
    await page.waitForFunction(() => !!window.anchorCSRF);
    await page.getByRole('link', { name: 'Administration', exact: true }).waitFor();
  });
  await t.test('member login, denied admin APIs/private shortcut and active-session revocation', async () => {
    const memberContext = await browser.newContext();
    await memberContext.route('**/*', route => new URL(route.request().url()).origin === origin ? route.continue() : route.abort());
    const memberPage = await memberContext.newPage();
    await memberPage.goto(origin + '/account.html');
    await memberPage.locator('#username').fill('qa-member'); await memberPage.locator('#password').fill(password);
    await memberPage.locator('#login button[type=submit]').click(); await memberPage.waitForURL(origin + '/');
    await memberPage.locator('#sections .tile').first().waitFor();
    assert.equal(await memberPage.getByRole('link', { name: 'My account', exact: true }).isVisible(), true);
    assert.equal((await memberContext.request.get(origin + '/api/admin/state')).status(), 403);
    const shortcut = await memberContext.request.get(origin + '/open/private', { maxRedirects: 0 });
    assert.equal(shortcut.status(), 302); assert.equal(shortcut.headers().location, '/');
    const me = await memberPage.evaluate(() => fetch('/api/me?as=admin').then(r => r.json())); assert.equal(me.admin, false); assert.equal(me.viewingAs, undefined);
    const current = await state();
    assert.equal((await post('/api/admin/users', { revision: current.revision, user: { ...member, enabled: false }, password: '' })).status, 200);
    await memberPage.reload(); await memberPage.waitForURL('**/account.html');
    await memberPage.locator('#login').waitFor({ state: 'visible' });
    await memberContext.close();
  });
  await t.test('members keep My account password changes and local sign-out', async () => {
    const before = await state();
    assert.equal((await post('/api/admin/users', { revision: before.revision, user: { name: 'Account Member', username: 'account-member', groups: ['family'], enabled: true }, password })).status, 200);
    const memberContext = await browser.newContext();
    await memberContext.route('**/*', route => new URL(route.request().url()).origin === origin ? route.continue() : route.abort());
    const changedPassword = 'member-new-pass';
    const signIn = async value => {
      const request = () => memberContext.request.post(origin + '/api/auth/login', { headers: { Origin: origin }, data: { Username: 'account-member', Password: value } });
      let result = await request();
      if (result.status() === 429) { await new Promise(resolve => setTimeout(resolve, 1100)); result = await request(); }
      assert.equal(result.status(), 200);
    };
    try {
      await signIn(password);
      const memberPage = await memberContext.newPage();
      memberPage.on('pageerror', error => exceptions.push(error.message));
      await memberPage.goto(origin + '/');
      await memberPage.getByRole('link', { name: 'My account', exact: true }).click();
      await memberPage.locator('#profile-name').filter({ hasText: 'Account Member' }).waitFor();
      assert.equal(await memberPage.getByRole('link', { name: 'Administration', exact: true }).count(), 0);
      await memberPage.getByText('Local sign-in details', { exact: true }).click();
      await memberPage.locator('#current-password').fill(password); await memberPage.locator('#new-password').fill(changedPassword);
      await memberPage.locator('#change-password button[type=submit]').click();
      await memberPage.locator('#login').waitFor({ state: 'visible' });
      assert.equal((await (await memberContext.request.get(origin + '/api/auth/status')).json()).authenticated, false);
      await signIn(changedPassword);
      await memberPage.goto(origin + '/');
      await memberPage.getByRole('link', { name: 'My account', exact: true }).click();
      await memberPage.locator('#sign-out').click();
      await memberPage.locator('#login').waitFor({ state: 'visible' });
      assert.equal((await (await memberContext.request.get(origin + '/api/auth/status')).json()).authenticated, false);
      assert.equal((await context.request.get(origin + '/api/admin/state')).status(), 200, 'member sign-out preserves the administrator session');
    } finally { await memberContext.close(); }
  });
  await t.test('administrators change their password in Users & groups and sign in again', async () => {
    const changedPassword = 'new-pass';
    await page.goto(origin + '/manage.html');
    await page.locator('[data-panel="users"]').click();
    const administrator = (await state()).users.find(user => user.username === 'qa-admin');
    await page.getByRole('button', { name: 'Edit ' + administrator.name, exact: true }).click();
    await page.locator('#user-password').fill(changedPassword);
    assert.equal(await page.locator('#user-password-warning').isVisible(), true);
    await page.locator('#user-form button[type=submit]').click();
    await page.waitForURL('**/account.html'); await page.locator('#login').waitFor({ state: 'visible' });
    await page.locator('#username').fill('qa-admin'); await page.locator('#password').fill(changedPassword);
    const response = await Promise.all([
      page.waitForResponse(r => r.url().endsWith('/api/auth/login') && r.request().method() === 'POST'),
      page.locator('#login button[type=submit]').click(),
    ]).then(([response]) => response);
    // Browser contexts share loopback IP: respect the deliberate one-second
    // login throttle if the preceding member workflow was particularly fast.
    if (response.status() === 429) {
      await page.locator('#message').filter({ hasText: 'Too many sign-in attempts' }).waitFor();
      await new Promise(resolve => setTimeout(resolve, 1100));
      await page.locator('#login button[type=submit]').click();
    } else assert.equal(response.status(), 200);
    await page.waitForURL(origin + '/');
    await page.locator('#admin-bar').waitFor({ state: 'visible' });
  });
  await t.test('shared header sign-out protects drafts and ends the administrator session', async () => {
    await page.goto(origin + '/manage.html');
    await page.locator('[data-panel="settings"]').click();
    await page.locator('#settings-subtitle').fill('unsaved-password');
    await page.evaluate(() => { window.anchorQaConfirmationChoice = false; });
    await page.locator('#sign-out').click();
    await page.waitForFunction(() => window.anchorQaConfirmations.at(-1)?.title === 'Discard unsaved changes?');
    assert.equal(await page.locator('#settings-subtitle').inputValue(), 'unsaved-password');
    assert.equal((await (await context.request.get(origin + '/api/auth/status')).json()).authenticated, true);
    await page.evaluate(() => { window.anchorQaConfirmationChoice = true; });
    await page.locator('#sign-out').click();
    await page.waitForURL('**/account.html');
    await page.locator('#login').waitFor({ state: 'visible' });
    assert.equal(await page.locator('#profile').isVisible(), false);
  });
  assert.deepEqual(exceptions, [], 'uncaught browser exceptions');
  // Deliberate offline and rejected requests can produce browser network errors.
  assert.ok(consoleErrors.every(text => /Failed to load resource|net::ERR_/.test(text)), consoleErrors.join('\n'));

  await t.test('20,000-book fixture paging, full search, dialog keyboard and preview reset', async () => {
    const fixtureProcess = start(fixture, ['-test.run=^TestBrowserFixture$', '-test.timeout=11m'], { ANCHOR_BROWSER_TEST: '1', ANCHOR_BROWSER_LISTEN: '127.0.0.1:0' }, true);
    // Use the address of the listener actually acquired by our child. Never
    // attach this workflow to a service that already occupies the manual port.
    const fixtureOrigin = await new Promise((resolve, reject) => {
      let output = '';
      const timer = setTimeout(() => reject(new Error('Fixture did not announce a loopback listener')), 8000);
      fixtureProcess.on('exit', () => { clearTimeout(timer); reject(new Error('Fixture exited before startup')); });
      fixtureProcess.stdout.on('data', data => {
        output += data.toString();
        const match = output.match(/Disposable browser fixture: (http:\/\/127\.0\.0\.1:\d+)/);
        if (match) { clearTimeout(timer); resolve(match[1]); }
      });
    });
    await ready(fixtureOrigin + '/api/me');
    const fixtureContext = await browser.newContext();
    await fixtureContext.route('**/*', route => new URL(route.request().url()).origin === fixtureOrigin ? route.continue() : route.abort());
    const fixturePage = await fixtureContext.newPage();
    const errors = []; fixturePage.on('pageerror', error => errors.push(error.message));
    await fixturePage.goto(fixtureOrigin);
    await fixturePage.locator('#sections .tile').first().waitFor();
    await fixturePage.getByRole('link', { name: /^Ebooks:/ }).click();
    await fixturePage.locator('#ebooks-list .ebook-row').first().waitFor();
    assert.equal(await fixturePage.locator('#ebooks-list').evaluate(el => el.children.length), 100);
    if (process.env.ANCHOR_QA_ARTIFACT_DIR) await fixturePage.screenshot({ path: path.join(process.env.ANCHOR_QA_ARTIFACT_DIR, 'ebook-catalog.png'), fullPage: true });
    await fixturePage.locator('#ebooks-next').click(); assert.equal(await fixturePage.locator('#ebooks-page').textContent(), '101–200 of 20000 books');
    await fixturePage.locator('#ebooks-search').fill('Book 19999');
    assert.equal(await fixturePage.locator('#ebooks-list').evaluate(el => el.children.length), 1);
    await fixturePage.keyboard.type('/');
    assert.equal(await fixturePage.locator('#ebooks-search').inputValue(), 'Book 19999/');
    assert.equal(await fixturePage.locator('#ebooks-search').evaluate(el => el === document.activeElement), true);
    await fixturePage.keyboard.press('Tab');
    assert.equal(await fixturePage.evaluate(() => document.querySelector('#ebooks-dialog').contains(document.activeElement)), true);
    await fixturePage.keyboard.press('Escape'); await fixturePage.locator('#ebooks-dialog').waitFor({ state: 'hidden' });
    await fixturePage.getByRole('link', { name: /^Ebooks:/ }).click();
    await fixturePage.keyboard.press('Escape');
    await fixturePage.locator('#preview-as').selectOption('mom@example.com');
    await fixturePage.locator('#preview-note').filter({ hasText: 'Mom' }).waitFor();
    assert.equal(await fixturePage.locator('#ebooks-list').evaluate(el => el.children.length), 0);
    assert.deepEqual(errors, []);
    await fixtureContext.close();
  });
});

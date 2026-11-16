// Opt-in UI audit. See docs/testing.md. All application state is disposable.
const fs = require('node:fs/promises');
const path = require('node:path');
const os = require('node:os');
const net = require('node:net');
const { spawn } = require('node:child_process');
const { createHash } = require('node:crypto');
const { pathToFileURL } = require('node:url');
const assert = require('node:assert/strict');
const tools = process.env.ANCHOR_UI_TOOLS;
if (!tools) throw new Error('Set ANCHOR_UI_TOOLS to an external node_modules directory.');
const { chromium, firefox, webkit } = require(path.join(tools, 'playwright'));
const { PNG } = require(path.join(tools, 'pngjs'));
const binary = process.env.ANCHOR_UI_BINARY;
const seed = process.env.ANCHOR_UI_SEED_BINARY;
const output = process.env.ANCHOR_UI_OUTPUT;
const phase = process.env.ANCHOR_UI_PHASE || 'reference';
if (!binary || !seed || !output || !['reference', 'candidate', 'verify'].includes(phase)) throw new Error('Set binary, seed binary, output and phase.');
if (phase === 'verify' && !process.env.ANCHOR_UI_BASELINE) throw new Error('Verification requires ANCHOR_UI_BASELINE.');
const strict = phase !== 'reference';
const widths = [320, 375, 430, 768, 1024, 1440, 1920];
const issues = [], captures = [], diagnostics = [], checks = [];
const children = [], dirs = [];
const fixedTime = '2026-11-12T15:00:00.000Z';
let browser;

async function freePort() {
  const s = net.createServer(); await new Promise((r, j) => { s.once('error', j); s.listen(0, '127.0.0.1', r); });
  const n = s.address().port; await new Promise(r => s.close(r)); return n;
}
async function child(exe, args, env, wait = false) {
  await fs.access(exe);
  const p = spawn(exe, args, { windowsHide: true, stdio: ['ignore', 'pipe', 'pipe'], env });
  children.push(p);
  let logs = ''; p.stdout.on('data', d => { logs += d; }); p.stderr.on('data', d => { logs += d; });
  if (wait) await new Promise((r, j) => { p.once('error', j); p.once('exit', code => code === 0 ? r() : j(new Error(`Synthetic seed failed (${code})`))); });
  return p;
}
async function installation(mode = 'lan', empty = false) {
  const dir = await fs.mkdtemp(path.join(os.tmpdir(), 'anchor-ui-fixture-')); dirs.push(dir);
  const origin = `http://127.0.0.1:${await freePort()}`;
  const env = Object.fromEntries(Object.entries(process.env).filter(([k]) => !k.toUpperCase().startsWith('ANCHOR_')));
  Object.assign(env, { ANCHOR_CONFIG_DIR: dir, ANCHOR_LISTEN: origin.slice(7), ANCHOR_MODE: mode });
  if (mode === 'lan') Object.assign(env, { ANCHOR_ALLOW_HTTP: '1', ANCHOR_PUBLIC_URL: origin });
  if (!empty) await child(seed, ['-test.run=^TestUIVisualSeed$'], { ...env, ANCHOR_UI_SEED: '1' }, true);
  const p = await child(binary, [], env);
  for (let i = 0; i < 100; i++) {
    if (p.exitCode !== null) throw new Error('Owned UI server exited before readiness');
    try { if ((await fetch(origin + '/healthz')).ok) return origin; } catch {}
    await new Promise(r => setTimeout(r, 50));
  }
  throw new Error('Owned UI server failed readiness');
}
function note(condition, id, detail) {
  checks.push({ id, pass: !!condition, detail });
  if (!condition) issues.push({ id, detail });
}
async function jsonPost(page, url, body) {
  return page.evaluate(async ({ url, body }) => {
    const r = await fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
    return { status: r.status, body: await r.json() };
  }, { url, body });
}
async function login(page, origin, member = false) {
  await page.goto(origin + '/account.html');
  await page.locator('#username').fill(member ? 'visual-member' : 'visual-admin');
  await page.locator('#password').fill('synthetic-visual-password');
  await page.locator('#login button[type=submit]').click();
  await page.waitForURL(origin + '/'); await page.locator('.tile').first().waitFor();
}
async function contextFor(origin, colorScheme, extra = {}) {
  const context = await browser.newContext({ colorScheme, viewport: { width: 1440, height: 900 }, deviceScaleFactor: 1, locale: 'en-US', timezoneId: 'America/New_York', reducedMotion: 'reduce', serviceWorkers: 'block', ...extra });
  context.on('page', page => {
    page.setDefaultTimeout(7000);
    page.on('pageerror', err => diagnostics.push({ theme: colorScheme, type: 'exception', text: err.message }));
    page.on('console', msg => { if (msg.type() === 'error') diagnostics.push({ theme: colorScheme, type: page.__qaAxeActive ? 'axe-console' : 'console', text: msg.text(), location: msg.location(), time: Date.now() }); });
    page.on('requestfailed', request => diagnostics.push({ theme: colorScheme, type: 'request-failed', url: request.url(), error: request.failure()?.errorText, time: Date.now() }));
    page.on('dialog', dialog => dialog.accept());
  });
  await context.addInitScript(({ fixedTime }) => {
    const RealDate = Date;
    class FixedDate extends RealDate { constructor(...args) { super(...(args.length ? args : [fixedTime])); } static now() { return new RealDate(fixedTime).getTime(); } }
    window.Date = FixedDate;
    // Exercise copy feedback without reading or changing the host clipboard.
    Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async text => { window.__qaCopied = text; } } });
    const scroll = Element.prototype.scrollIntoView;
    window.__qaScrollCalls = [];
    Element.prototype.scrollIntoView = function(options) { window.__qaScrollCalls.push(options); return scroll.call(this, options); };
    const NativeEvents = window.EventSource;
    window.__qaStreams = [];
    window.EventSource = class extends NativeEvents { constructor(...args) { super(...args); window.__qaStreams.push(this); } };
  }, { fixedTime });
  await context.route('**/*', r => new URL(r.request().url()).origin === origin ? r.continue() : r.abort());
  return context;
}
async function audit(page, name, options = {}) {
  await page.evaluate(() => document.fonts.ready);
  await page.evaluate(() => {
    if (window.__qaCaretStable) return;
    const sheet = document.styleSheets[0];
    sheet.insertRule(':root { caret-color: transparent; }', sheet.cssRules.length);
    window.__qaCaretStable = true;
  });
  await page.locator('img').evaluateAll(images => Promise.all(images.filter(image => image.getClientRects().length).map(async image => { image.loading = 'eager'; await image.decode().catch(() => {}); })));
  await page.evaluate(() => new Promise(r => requestAnimationFrame(() => requestAnimationFrame(r))));
  const size = page.viewportSize();
  const filename = `${name}-${size.width}x${size.height}-${options.theme || 'light'}.png`;
  await page.screenshot({ path: path.join(output, filename), fullPage: !options.viewportOnly, animations: 'allow', caret: 'initial' });
  const geometry = await page.evaluate(() => {
    const visible = e => e.getClientRects().length && getComputedStyle(e).visibility !== 'hidden';
    const dialog = document.querySelector('dialog[open]');
    const root = dialog || document;
    const outside = [...root.querySelectorAll('input,select,textarea,button,a,h1,h2,.tile-name,.tile-desc,.notice span,.who-name')].filter(visible).map(e => {
      const b = e.getBoundingClientRect(); const s = getComputedStyle(e);
      // Horizontal navigation and internal scroll lists are intentional.
      if (e.closest('.admin-nav,.req-list,.ebook-list,.icon-grid')) return null;
      if (b.left < -1 || b.right > innerWidth + 1) return { tag: e.tagName, id: e.id, class: e.className, text: (e.textContent || '').slice(0, 80), left: b.left, right: b.right, font: s.fontSize };
      return null;
    }).filter(Boolean);
    const d = dialog && dialog.getBoundingClientRect();
    const body = getComputedStyle(document.body);
    const rootStyle = getComputedStyle(document.documentElement);
    const lists = [...document.querySelectorAll('dialog[open] .req-list,dialog[open] .ebook-list')].filter(visible).map(e => ({ id: e.id, height: e.clientHeight, rows: e.children.length }));
    const luminance = color => {
      const rgb = color.match(/[\d.]+/g).slice(0, 3).map(Number).map(n => { n /= 255; return n <= .04045 ? n / 12.92 : ((n + .055) / 1.055) ** 2.4; });
      return .2126 * rgb[0] + .7152 * rgb[1] + .0722 * rgb[2];
    };
    const contrast = [...root.querySelectorAll('.button-link,button.primary,.btn:not(.secondary)')].filter(e => visible(e) && !e.disabled).map(e => {
      const s = getComputedStyle(e), a = luminance(s.color), b = luminance(s.backgroundColor);
      return { text: e.textContent.trim().slice(0, 60), ratio: (Math.max(a, b) + .05) / (Math.min(a, b) + .05), color: s.color, background: s.backgroundColor };
    });
    const borders = [...root.querySelectorAll('input:not([type="checkbox"]),select,textarea')].filter(e => visible(e) && !e.disabled).map(e => {
      const s = getComputedStyle(e), a = luminance(s.borderTopColor), b = luminance(s.backgroundColor);
      return { id: e.id, ratio: (Math.max(a, b) + .05) / (Math.min(a, b) + .05) };
    });
    const broken = [...root.querySelectorAll('img')].filter(e => visible(e) && e.complete && !e.naturalWidth).map(e => e.id || e.src.split('/').pop());
    const wrappedActions = [...root.querySelectorAll('.req-foot > .btn')].filter(e => visible(e) && !e.disabled).filter(e => { const range = document.createRange(); range.selectNodeContents(e); return range.getClientRects().length > 1; }).map(e => e.id);
    return { overflow: document.documentElement.scrollWidth - innerWidth, outside, contrast, borders, broken, wrappedActions, dialog: d && { top: d.top, bottom: d.bottom, left: d.left, right: d.right, scrollHeight: dialog.scrollHeight, clientHeight: dialog.clientHeight }, lists,
      style: { font: body.fontFamily, background: body.backgroundColor, color: body.color, accent: rootStyle.getPropertyValue('--accent').trim(), surface: rootStyle.getPropertyValue('--surface').trim() } };
  });
  note(geometry.overflow <= 1, `${name}/document-overflow/${size.width}/${options.theme}`, geometry.overflow);
  note(geometry.outside.length === 0, `${name}/controls-in-viewport/${size.width}/${options.theme}`, geometry.outside);
  note(geometry.contrast.every(c => c.ratio >= 4.5), `${name}/primary-contrast/${size.width}/${options.theme}`, geometry.contrast.filter(c => c.ratio < 4.5));
  note(geometry.broken.length === 0, `${name}/broken-images/${size.width}/${options.theme}`, geometry.broken);
  note(geometry.wrappedActions.length === 0, `${name}/toolbar-labels/${size.width}/${options.theme}`, geometry.wrappedActions);
  note(geometry.borders.every(c => c.ratio >= 3), `${name}/input-boundary-contrast/${size.width}/${options.theme}`, geometry.borders.filter(c => c.ratio < 3));
  if (geometry.dialog) {
    note(geometry.dialog.top >= -1 && geometry.dialog.bottom <= size.height + 1, `${name}/dialog-bounds/${size.width}/${options.theme}`, geometry.dialog);
    for (const list of geometry.lists) note(!list.rows || list.height >= 48, `${name}/usable-list/${size.width}/${options.theme}`, list);
  }
  let accessibility;
  if (options.axe) {
    page.__qaAxeActive = true;
    if (!await page.evaluate(() => !!window.axe)) await page.evaluate(await fs.readFile(path.join(tools, 'axe-core/axe.min.js'), 'utf8'));
    accessibility = await page.evaluate(async () => {
      const result = await axe.run(document, { runOnly: { type: 'tag', values: ['wcag2a', 'wcag2aa', 'wcag21aa', 'wcag22aa', 'best-practice'] } });
      return { violations: result.violations.map(v => ({ id: v.id, impact: v.impact, help: v.help, nodes: v.nodes.map(n => ({ target: n.target, failure: n.failureSummary, data: n.any.map(c => c.data) })) })), incomplete: result.incomplete.map(v => ({ id: v.id, nodes: v.nodes.length })) };
    });
    for (const v of accessibility.violations) issues.push({ id: `${name}/axe/${options.theme}`, detail: v });
    page.__qaAxeActive = false;
  }
  captures.push({ name, file: filename, viewport: size, theme: options.theme || 'light', geometry, accessibility });
}

async function run() {
  await fs.mkdir(output, { recursive: true });
  const previous = await fs.readFile(path.join(output, 'manifest.json'), 'utf8').then(JSON.parse, () => null);
  if (previous?.approved) throw new Error('Refusing to overwrite an approved visual baseline. Choose a new output directory.');
  const engine = process.env.ANCHOR_UI_ENGINE || 'chromium';
  browser = await ({ chromium, firefox, webkit })[engine].launch(engine === 'chromium' ? { executablePath: process.env.ANCHOR_QA_CHROME || 'C:\\Program Files\\Google\\Chrome\\Application\\chrome.exe', headless: true, args: ['--disable-background-networking', '--disable-component-update', '--host-resolver-rules=MAP * ~NOTFOUND, EXCLUDE 127.0.0.1'] } : { headless: true });
  const version = browser.version();
  const origin = await installation();
  const setupOrigin = await installation('lan', true);
  const tailscaleOrigin = await installation('tailscale');
  const image = await fs.readFile(path.join(__dirname, '../web/brand/icon-192.png'));
  for (const theme of ['light', 'dark']) {
    const context = await contextFor(origin, theme);
    const page = await context.newPage();
    let requestState = 'ready', ebookState = 'ready', meState = 'ready', imagesFail = false, reportHold = null;
    const requests = { rows: Array.from({ length: 8 }, (_, i) => ({ title: i === 0 ? 'Shōgun — A very long show title with 日本語 and O’Connor’s collection' : `A ${i % 2 ? 'movie' : 'show'} title ${i + 1}`, kind: i % 2 ? 'movie' : 'show', year: 2025, state: ['ready', 'partial', 'downloading', 'soon', 'pending', 'declined', 'notfound', 'unconfirmed'][i], ready: 3, date: '2026-12-12', posters: i < 2 ? [`movie/${i + 1}`] : [], detail: i === 0 ? 'Synthetic administrator detail — no real integration data.' : '' })), coming: { rows: [{ title: 'A new season', kind: 'show', season: 2, note: 'premiere', date: '2026-12-15' }, { title: 'Upcoming cinema release', kind: 'movie', note: 'cinemas', date: '2026-12-20' }], missing: ['movies'] }, updated: fixedTime, age: 120, plexOk: true, problems: ['Synthetic upstream temporarily unavailable.'] };
    const books = { rows: Array.from({ length: 20000 }, (_, i) => ({ id: `visual-${i}`, title: i === 0 ? 'An exceptionally long ebook title — café, 日本語, and O’Connor’s annotated edition' : `Book ${String(i).padStart(5, '0')}`, author: i === 0 ? 'A long author name · illustrated edition' : 'Synthetic Author', format: 'EPUB', year: '2025' })), age: 120, updated: fixedTime };
    await context.route('**/api/requests**', async route => {
      if (route.request().url().includes('/poster/')) return route.fulfill(imagesFail ? { status: 404, body: '' } : { status: 200, contentType: 'image/png', body: image });
      if (requestState === 'loading') await new Promise(r => setTimeout(r, 1200));
      return route.fulfill({ status: requestState === 'error' ? 503 : 200, json: requestState === 'error' ? { message: 'Request status is temporarily unavailable. Please try again.' } : requestState === 'empty' ? { ...requests, rows: [], coming: undefined, problems: [] } : requests }).catch(() => {});
    });
    await context.route('**/api/ebooks**', async route => {
      if (route.request().url().includes('/cover')) return route.fulfill(imagesFail ? { status: 404, body: '' } : { status: 200, contentType: 'image/png', body: image });
      if (ebookState === 'loading') await new Promise(r => setTimeout(r, 1200));
      return route.fulfill({ status: ebookState === 'error' ? 503 : 200, json: ebookState === 'error' ? { message: 'The library could not be loaded. Please try again.' } : ebookState === 'empty' ? { ...books, rows: [] } : books }).catch(() => {});
    });
    await context.route('**/api/me*', async route => {
      if (meState === 'offline') return route.abort('internetdisconnected');
      if (meState === 'loading') await new Promise(r => setTimeout(r, 1200));
      const response = await route.fetch(); const me = await response.json();
      if (meState === 'empty') me.sections = [];
      if (meState === 'unknown') Object.assign(me, { known: false, admin: false, sections: [], login: '', name: '' });
      if (meState === 'long') Object.assign(me, { title: 'AnchorLongDashboardTitle'.repeat(4), name: 'LongPersonName'.repeat(8), contact: 'HouseholdAdministrator'.repeat(4), subtitle: 'A long dashboard subtitle with Unicode — café, 日本語, and punctuation. '.repeat(5) });
      if (me.sections?.length) { me.sections[0].apps[0].status = 'up'; if (me.sections[1]) { me.sections[1].apps[1].status = 'down'; me.sections[1].apps[1].locked = true; me.sections[1].apps[1].adminOpen = true; } }
      await route.fulfill({ response, json: me });
    });
    await context.route('**/api/report', async route => { if (reportHold) await reportHold; return route.fulfill({ status: 200, json: { message: 'Your synthetic report was sent to the household administrator.' } }); });
    await login(page, origin);
    const first = await page.evaluate(() => fetch('/api/admin/state').then(r => r.json()));
    await jsonPost(page, '/api/admin/config', { revision: first.revision, config: first.config });
    try { await page.waitForFunction(() => window.__qaStreams.some(stream => stream.readyState === EventSource.OPEN), null, { timeout: 3000 }); note(true, `events/open/${theme}`); }
    catch { note(false, `events/open/${theme}`); }
    note(!diagnostics.some(d => d.theme === theme && d.type === 'console' && /Content Security Policy/.test(d.text)), `normal-browsing/csp/${theme}`);
    const initialTheme = await page.evaluate(() => getComputedStyle(document.body).backgroundColor);
    await page.emulateMedia({ colorScheme: theme === 'light' ? 'dark' : 'light' });
    note(initialTheme !== await page.evaluate(() => getComputedStyle(document.body).backgroundColor), `theme/live-switch/${theme}`);
    await page.emulateMedia({ colorScheme: theme });
    const selectedWidths = process.env.ANCHOR_UI_QUICK === '1' ? [320, 1440] : widths;
    for (const width of selectedWidths) {
      await page.setViewportSize({ width, height: width <= 430 ? 740 : 900 });
      const opts = { theme, axe: width === 1440 || width === 320 };
      await page.goto(origin + '/'); await page.locator('.tile').first().waitFor();
      await audit(page, 'dashboard', opts);
      await page.goto(origin + '/manage.html'); await page.locator('#content').waitFor({ state: 'visible' });
      for (const panel of ['apps', 'users', 'notifications', 'security', 'settings', 'transfer']) {
        await page.locator(`[data-panel="${panel}"]`).click();
        await audit(page, `admin-${panel}`, opts);
      }
      await page.locator('[data-panel="apps"]').focus(); await page.keyboard.press('Enter');
      note(await page.locator('[data-view="apps"]').isVisible(), `admin/keyboard-activate/${width}/${theme}`);
      await page.keyboard.press('Tab');
      note(await page.locator('[data-panel="users"]').evaluate(e => e === document.activeElement && getComputedStyle(e).outlineStyle === 'solid' && parseFloat(getComputedStyle(e).outlineWidth) >= 2), `admin/visible-focus/${width}/${theme}`);
      await page.locator('[data-panel="apps"]').click();
      await page.locator('#app-list .application-card').first().getByRole('button', { name: 'Edit', exact: true }).click();
      note(await page.locator('#app-name').evaluate(e => e === document.activeElement), `admin/edit-focus/${width}/${theme}`);
      await page.locator('#cancel-app').click(); await page.locator('#add-app').click();
      await page.locator('#builder-next').click();
      await page.locator('#app-name').fill('Visual draft');
      await page.locator('#app-url').fill('https://visual-draft.invalid/');
      await page.locator('#builder-next').click();
      const motion = await page.evaluate(() => matchMedia('(prefers-reduced-motion: reduce)').matches);
      note(motion, `reduced-motion/preference/${width}/${theme}`);
      note(await page.evaluate(() => window.__qaScrollCalls.every(call => call?.behavior !== 'smooth')), `reduced-motion/scroll/${width}/${theme}`);
      await audit(page, 'admin-app-form', opts);
      await page.locator('#icon-browser summary').click();
      await page.locator('#icon-search').fill('sonarr');
      await audit(page, 'admin-icon-browser', { theme });
      await page.locator('#cancel-app').click();
      await page.getByRole('button', { name: 'Discard changes', exact: true }).click();
      await page.locator('[data-panel="users"]').click(); await page.locator('#add-user').click();
      await audit(page, 'admin-user-form', opts);
      await page.goto(origin + '/account.html'); await page.locator('#profile').waitFor({ state: 'visible' });
      note(!await page.locator('#local-credentials').isVisible(), `account/admin-credentials-in-users/${width}/${theme}`);
      await audit(page, 'account', opts);
      await page.goto(origin + '/open/media?add=1'); await audit(page, 'shortcut-guide', opts);
      await page.goto(origin + '/'); await page.locator('.tile').first().waitFor();
      for (const [name, trigger, dialog] of [
        ['app-instructions', '[data-id="mobile"] button.get-app', '#app-dialog'],
        ['guide-dialog', '[data-id="guide"] .tile-link', '#guide-dialog'],
        ['report-dialog', '[data-id="media"] .report', '#report-dialog'],
        ['requests', '[data-id="media"] .req-open', '#req-dialog'],
        ['ebooks', '[data-id="books"] .tile-link', '#ebooks-dialog'],
      ]) {
        await page.locator(trigger).click();
        if (name === 'requests') await page.locator('#req-list .req-row').first().waitFor();
        if (name === 'ebooks') await page.locator('#ebooks-list .ebook-row').first().waitFor();
        await audit(page, name, { ...opts, viewportOnly: true });
        if (name === 'app-instructions') {
          const copyBefore = await page.locator('#copy-btn').boundingBox();
          await page.locator('#copy-btn').click();
          const copyAfter = await page.locator('#copy-btn').boundingBox();
          note(Math.abs(copyBefore.width - copyAfter.width) <= 1, `copy/stable-feedback-size/${width}/${theme}`, { before: copyBefore.width, after: copyAfter.width });
          note(await page.evaluate(() => window.__qaCopied === document.querySelector('#server-addr').textContent), `copy/value/${width}/${theme}`);
          await audit(page, 'copy-feedback', { theme, viewportOnly: true });
        }
        note(await page.evaluate(selector => document.querySelector(selector).contains(document.activeElement), dialog), `${name}/initial-focus/${width}/${theme}`);
        await page.keyboard.press('Tab');
        note(await page.evaluate(selector => document.querySelector(selector).contains(document.activeElement), dialog), `${name}/tab-focus/${width}/${theme}`);
        if (name === 'requests') {
          await page.locator('#req-list').focus();
          note(await page.locator('#req-list').evaluate(e => e === document.activeElement), `requests/keyboard-scroll-focus/${width}/${theme}`);
          await page.keyboard.press('PageDown');
          await page.locator('#req-tab-coming').click(); await audit(page, 'coming-up', { theme, viewportOnly: true });
          const tabScroll = await page.locator('#req-list').evaluate(e => ({ top: e.scrollTop, active: document.activeElement.id }));
          note(tabScroll.top === 0, `requests/tab-start/${width}/${theme}`, tabScroll);
        }
        if (name === 'ebooks') {
          await page.locator('#ebooks-next').click();
          note((await page.locator('#ebooks-page').textContent()).startsWith('101'), `ebooks/paging/${width}/${theme}`);
          await page.locator('#ebooks-search').fill('no-such-book'); await audit(page, 'ebook-no-results', { theme, viewportOnly: true });
        }
        await page.keyboard.press('Escape');
        // Chromium's native search field consumes Escape to clear a query.
        if (name === 'ebooks' && await page.locator(dialog).isVisible() && await page.locator('#ebooks-search').inputValue() === '') await page.keyboard.press('Escape');
        await page.locator(dialog).waitFor({ state: 'hidden' });
        const returned = await page.waitForFunction(selector => document.querySelector(selector) === document.activeElement, trigger, { timeout: 500 }).then(() => true, () => false);
        note(returned, `${name}/focus-return/${width}/${theme}`);
        await page.locator(trigger).click();
        await page.locator(dialog).getByRole('button', { name: name === 'report-dialog' ? 'Cancel' : 'Close', exact: true }).click();
        note(!await page.locator(dialog).isVisible(), `${name}/close-button/${width}/${theme}`);
      }
    }
    // Short landscape heights, reflow/zoom equivalents and text-spacing override.
    for (const [width, height, label] of [[740, 375, 'landscape'], [720, 450, 'zoom-200'], [360, 225, 'zoom-400']]) {
      await page.setViewportSize({ width, height }); await page.goto(origin + '/'); await page.locator('.tile').first().waitFor();
      await page.locator('[data-id="books"] .tile-link').click(); await page.locator('#ebooks-list .ebook-row').first().waitFor();
      await audit(page, label + '-ebook', { theme, viewportOnly: true });
      await page.locator('#ebooks-list').scrollIntoViewIfNeeded();
      const visibleList = await page.locator('#ebooks-list').evaluate(e => { const b = e.getBoundingClientRect(); return Math.max(0, Math.min(innerHeight, b.bottom) - Math.max(0, b.top)); });
      note(visibleList >= 48, `${label}/visible-catalog/${theme}`, visibleList);
      await audit(page, label + '-ebook-content', { theme, viewportOnly: true });
      await page.locator('#ebooks-dialog button[aria-label="Close"]').focus();
      await page.keyboard.press('Shift+Tab');
      note(await page.locator('#ebooks-refresh').evaluate(e => e === document.activeElement), `${label}/reverse-tab/${theme}`);
      const refreshBounds = await page.locator('#ebooks-refresh').evaluate(e => { const b = e.getBoundingClientRect(); return { top: b.top, bottom: b.bottom, viewport: innerHeight }; });
      note(refreshBounds.top >= -1 && refreshBounds.bottom <= refreshBounds.viewport + 1, `${label}/reachable-refresh/${theme}`, refreshBounds);
      await page.keyboard.press('Escape');
      await page.goto(origin + '/manage.html'); await page.locator('#content').waitFor({ state: 'visible' });
      await page.locator('[data-panel="notifications"]').click(); await audit(page, label + '-notifications', { theme });
      await page.locator('[data-panel="security"]').click(); await audit(page, label + '-connection', { theme });
    }
    await page.setViewportSize({ width: 375, height: 740 }); await page.goto(origin + '/'); await page.locator('.tile').first().waitFor();
    // Apply the accessibility spacing override through the loaded stylesheet.
    await page.evaluate(() => { document.styleSheets[0].insertRule('* { line-height:1.5 !important; letter-spacing:.12em !important; word-spacing:.16em !important; }'); });
    await audit(page, 'text-spacing-dashboard', { theme });
    await page.goto(origin + '/'); await page.locator('.tile').first().waitFor();
    for (const state of ['loading', 'empty', 'error']) {
      requestState = state; ebookState = state;
      await page.reload(); await page.locator('.tile').first().waitFor();
      await page.locator('[data-id="media"] .req-open').click();
      if (state !== 'loading') await page.waitForFunction(() => !document.querySelector('#req-refresh').disabled);
      await audit(page, `requests-${state}`, { theme, viewportOnly: true }); await page.keyboard.press('Escape');
      await page.locator('[data-id="books"] .tile-link').click();
      if (state !== 'loading') await page.waitForFunction(() => !document.querySelector('#ebooks-refresh').disabled);
      await audit(page, `ebooks-${state}`, { theme, viewportOnly: true }); await page.keyboard.press('Escape');
    }
    requestState = ebookState = 'ready';
    imagesFail = true;
    await context.route('**/icons/sonarr', r => r.fulfill({ status: 404, body: '' }));
    await page.reload();
    // The icon is lazy-loaded below the fold at narrow widths. Bring the
    // deliberately failing image into view so its fallback can actually run.
    await page.locator('[data-id="long"]').scrollIntoViewIfNeeded();
    await page.locator('[data-id="long"] .tile-icon.letter').waitFor(); await audit(page, 'missing-icon', { theme });
    await page.locator('[data-id="media"] .req-open').click(); await page.locator('#req-list .req-row').first().waitFor(); await audit(page, 'missing-posters', { theme, viewportOnly: true }); await page.keyboard.press('Escape');
    await page.locator('[data-id="books"] .tile-link').click(); await page.locator('#ebooks-list .ebook-row').first().waitFor(); await audit(page, 'missing-covers', { theme, viewportOnly: true }); await page.keyboard.press('Escape');
    imagesFail = false; await context.unroute('**/icons/sonarr');
    meState = 'loading'; await page.reload(); await audit(page, 'dashboard-loading', { theme }); await page.locator('.tile').first().waitFor(); meState = 'ready';
    meState = 'offline'; await page.evaluate(() => document.dispatchEvent(new Event('visibilitychange'))); await page.locator('#offline').waitFor({ state: 'visible' }); await audit(page, 'offline', { theme, axe: true });
    meState = 'ready'; await page.locator('#retry-btn').click(); await page.locator('#offline').waitFor({ state: 'hidden' }); note(await page.locator('.tile').count() === 6, `offline/recovery/${theme}`);
    for (const state of ['empty', 'unknown', 'long']) {
      meState = state; await page.reload(); await page.waitForFunction(() => document.querySelector('#greeting').textContent !== 'Loading…'); await audit(page, `dashboard-${state}`, { theme, axe: true });
      if (state === 'unknown') { note((await page.locator('#stranger-body').textContent()).includes('My account'), `signed-out/guidance/${theme}`); note(!await page.getByRole('link', { name: 'Administration', exact: true }).isVisible(), `signed-out/navigation/${theme}`); }
    }
    meState = 'ready'; await page.reload(); await page.locator('.tile').first().waitFor();
    await page.locator('[data-id="media"] .report').click();
    const reportBefore = await page.locator('#report-send').boundingBox();
    let releaseReport; reportHold = new Promise(resolve => { releaseReport = resolve; });
    try {
      await page.locator('#report-send').click();
      const reportBusy = await page.locator('#report-send').boundingBox();
      note(Math.abs(reportBefore.width - reportBusy.width) <= 1 && Math.abs(reportBefore.height - reportBusy.height) <= 1, `report/stable-busy-size/${theme}`, { before: reportBefore, busy: reportBusy });
      await audit(page, 'report-sending', { theme, viewportOnly: true });
    } finally { releaseReport(); reportHold = null; }
    await page.locator('#toast').waitFor({ state: 'visible' }); await audit(page, 'toast', { theme });
    note((await page.locator('#toast').boundingBox()).width >= 300, `toast/readable-width/${theme}`);
    await page.goto(origin + '/manage.html'); await page.locator('#content').waitFor({ state: 'visible' }); await page.locator('[data-panel="transfer"]').click();
    await page.locator('#import-yaml').fill('not valid dashboard YAML'); await page.locator('#import-form button[type=submit]').click(); await page.locator('#message.error').waitFor({ state: 'visible' });
    await audit(page, 'admin-error', { theme, axe: true });
    await page.locator('[data-panel="settings"]').click();
    await page.getByRole('button', { name: 'Discard changes', exact: true }).click();
    await page.locator('#settings-title').fill('Anchor visual fixture');
    let releaseSave;
    const heldSave = new Promise(resolve => { releaseSave = resolve; });
    await context.route('**/api/admin/config', async route => { await heldSave; await route.continue(); });
    try {
      await page.locator('#settings-form button[type=submit]').click();
      await page.waitForFunction(() => document.querySelector('#settings-form button[type=submit]').disabled);
      const protectedDraft = await page.locator('#settings-title').isDisabled();
      note(protectedDraft, `admin/in-flight-draft-protection/${theme}`);
      if (!protectedDraft) await page.locator('#settings-title').fill('Unsaved text entered after submission');
      await audit(page, 'admin-saving', { theme });
    } finally { releaseSave(); }
    await page.locator('#message').filter({ hasText: 'Dashboard settings saved.' }).waitFor();
    await context.unroute('**/api/admin/config');
    const state = await page.evaluate(() => fetch('/api/admin/state').then(r => r.json()));
    state.config.Title = 'Anchor';
    note((await jsonPost(page, '/api/admin/config', { revision: state.revision, config: state.config })).status === 200, `fixture/restore-title/${theme}`);
    const unauth = await contextFor(origin, theme); const up = await unauth.newPage();
    const setup = await contextFor(setupOrigin, theme); const sp = await setup.newPage();
    for (const width of selectedWidths) {
      for (const [target, url, selector, name] of [[up, origin, '#login', 'login'], [sp, setupOrigin, '#setup', 'setup']]) {
        await target.setViewportSize({ width, height: width <= 430 ? 740 : 900 }); await target.goto(url + '/account.html'); await target.locator(selector).waitFor({ state: 'visible' }); await audit(target, name, { theme, axe: width === 320 || width === 1440 });
      }
    }
    const member = await contextFor(origin, theme); const mp = await member.newPage(); await mp.setViewportSize({ width: 375, height: 740 }); await login(mp, origin, true);
    await mp.goto(origin + '/open/maintenance'); await mp.locator('.open-note').waitFor(); await audit(mp, 'unavailable-shortcut', { theme, axe: true });
    await mp.goto(origin + '/manage.html'); await mp.locator('#loading').filter({ hasText: 'Administrator access is required.' }).waitFor(); await audit(mp, 'unauthorized-admin', { theme, axe: true });
    await member.close();
    const tailscale = await contextFor(tailscaleOrigin, theme, { extraHTTPHeaders: { 'Tailscale-User-Login': 'alex@example.test' } });
    const tp = await tailscale.newPage(); await tp.setViewportSize({ width: 375, height: 740 });
    await tp.goto(tailscaleOrigin + '/account.html'); await tp.locator('#profile').waitFor({ state: 'visible' }); await audit(tp, 'tailscale-account', { theme, axe: true });
    await tailscale.close();
    await setup.close(); await unauth.close(); await context.close();
    console.log(`${theme}: ${captures.length} cumulative captures, ${issues.length} findings recorded`);
  }
  const servedAssets = {};
  for (const asset of ['style.css', 'admin.css', 'app.js', 'manage.js', 'session.js', 'index.html', 'manage.html', 'account.html']) servedAssets[asset] = createHash('sha256').update(Buffer.from(await (await fetch(origin + '/' + asset)).arrayBuffer())).digest('hex');
  const manifest = { phase, approved: false, browser: engine, version, platform: process.platform, deviceScaleFactor: 1, time: fixedTime, widths, servedAssets, captures, checks, issues, diagnostics };
  if (phase === 'verify' && process.env.ANCHOR_UI_BASELINE) {
    const baseline = JSON.parse(await fs.readFile(path.join(process.env.ANCHOR_UI_BASELINE, 'manifest.json'), 'utf8'));
    assert.equal(baseline.browser, engine, 'Use the same browser engine for pixel comparison');
    assert.equal(baseline.version, version, 'Use the same browser version for pixel comparison');
    assert.equal(baseline.platform, process.platform, 'Use the same OS for pixel comparison');
    const pixelmatch = (await import(pathToFileURL(path.join(tools, 'pixelmatch/index.js')).href)).default;
    manifest.comparisons = [];
    for (const capture of captures) {
      const a = PNG.sync.read(await fs.readFile(path.join(process.env.ANCHOR_UI_BASELINE, capture.file)));
      const b = PNG.sync.read(await fs.readFile(path.join(output, capture.file)));
      assert.equal(a.width, b.width); assert.equal(a.height, b.height);
      const diff = new PNG({ width: a.width, height: a.height });
      const changed = pixelmatch(a.data, b.data, diff.data, a.width, a.height, { threshold: 0.1, includeAA: false });
      const ratio = changed / (a.width * a.height); manifest.comparisons.push({ file: capture.file, changed, ratio });
      if (changed > 50) { await fs.writeFile(path.join(output, capture.file.replace('.png', '-diff.png')), PNG.sync.write(diff)); issues.push({ id: 'visual-diff', detail: { file: capture.file, changed, ratio } }); }
    }
  }
  await fs.writeFile(path.join(output, 'manifest.json'), JSON.stringify(manifest, null, 2));
  const escape = value => String(value).replaceAll('&', '&amp;').replaceAll('<', '&lt;').replaceAll('"', '&quot;');
  await fs.writeFile(path.join(output, 'gallery.html'), `<!doctype html><meta charset="utf-8"><title>Anchor ${escape(phase)} visual review</title><style>body{font:16px system-ui;margin:24px;background:#f6f3ee;color:#14232d}main{display:grid;grid-template-columns:repeat(auto-fill,minmax(300px,1fr));gap:20px}figure{margin:0;padding:12px;border:1px solid #7d8b93;border-radius:12px;background:white}img{width:100%;height:300px;object-fit:contain;object-position:top}a{color:#0f5f7a}figcaption{overflow-wrap:anywhere}</style><h1>Anchor ${escape(phase)} visual review</h1><p>Unapproved captures · ${escape(engine)} ${escape(version)} · ${captures.length} images. Open images at full size before approving a baseline.</p><main>${captures.map(c => `<figure><a href="${encodeURIComponent(c.file)}"><img loading="lazy" src="${encodeURIComponent(c.file)}" alt="${escape(c.name)} ${escape(c.theme)}"></a><figcaption>${escape(c.file)}</figcaption></figure>`).join('')}</main>`);
  console.log(JSON.stringify({ phase, browser: engine, version, screenshots: captures.length, checks: checks.length, issues: issues.length, exceptions: diagnostics.filter(d => d.type === 'exception').length, output }));
  const canceledFirefoxStream = d => /(Firefox can’t establish a connection.*\/api\/events|The connection to .*\/api\/events was interrupted)/.test(d.text) && diagnostics.some(r => r.type === 'request-failed' && r.url.endsWith('/api/events') && /ABORTED|CANCELLED/i.test(r.error || '') && Math.abs(r.time - d.time) < 1000);
  // WebKit delivers axe's blocked style-injection warnings asynchronously.
  // Normal browsing above is separately checked before any axe injection.
  const toolStyleWarning = d => engine === 'webkit' && checks.filter(c => c.id.startsWith('normal-browsing/csp/')).every(c => c.pass) && /^Refused to apply a stylesheet because its hash, its nonce, or 'unsafe-inline'/.test(d.text);
  const unexpectedConsole = diagnostics.filter(d => d.type === 'console' && !/Failed to load resource: (the server responded with a status of (400|404|503)|net::ERR_INTERNET_DISCONNECTED)/.test(d.text) && !canceledFirefoxStream(d) && !toolStyleWarning(d));
  manifest.unexpectedConsole = unexpectedConsole;
  await fs.writeFile(path.join(output, 'manifest.json'), JSON.stringify(manifest, null, 2));
  if (strict && (issues.length || diagnostics.some(d => d.type === 'exception') || unexpectedConsole.length)) process.exitCode = 1;
}
run().catch(async error => { console.error(error); process.exitCode = 1; await fs.mkdir(output, { recursive: true }); await fs.writeFile(path.join(output, 'incomplete.json'), JSON.stringify({ error: error.stack, captures, issues, checks, diagnostics }, null, 2)); }).finally(async () => {
  if (browser) await browser.close();
  for (const p of children) if (p.exitCode === null && p.signalCode === null) await new Promise(r => { p.once('exit', r); p.kill(); });
  for (const dir of dirs) await fs.rm(dir, { recursive: true, force: true });
});

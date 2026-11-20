// Captures the console screenshots for the landing page (docs/static/shots/).
// Drives the locally installed Chrome via playwright-core; no bundled browser.
//
//   pnpm add playwright-core          # once, anywhere; or run from a dir that has it
//   SEAMLESS_SHOT_BASE=http://127.0.0.1:8090 SEAMLESS_MCP_API_KEY=<key> \
//     node scripts/branding/console-shots.js /tmp/shots
//
// Point it at a THROWAWAY instance seeded by cmd/demoseed, never a live one.
// Convert the PNGs with `cwebp -q 84` into docs/static/shots/.
// Full recipe: scripts/branding/README.md.
const { createHash } = require('node:crypto');
const path = require('node:path');
const { chromium } = require('playwright-core');

const BASE = process.env.SEAMLESS_SHOT_BASE || 'http://127.0.0.1:8090';
const KEY = process.env.SEAMLESS_MCP_API_KEY;
if (!KEY) {
  console.error('SEAMLESS_MCP_API_KEY is required (the throwaway instance key)');
  process.exit(1);
}
// Mirrors internal/console consoleToken: the cookie holds a digest, not the key.
const COOKIE = createHash('sha256').update(`seamless-console\0${KEY}`).digest('hex');

const PAGES = [
  { name: 'overview', url: '/console/?w=30d' },
  // Cross-project by default: the Now screen's whole point is the whole fleet.
  { name: 'now', url: '/console/now' },
  {
    name: 'interactions',
    url: '/console/interactions',
    // The feed is client-rendered and starts empty by design -- it only shows
    // live arrivals until you add a history window. The `history=` URL param
    // does not help: the server hands the page an empty div and the JS fills
    // it. So drive the control the way an owner would.
    prepare: async (page) => {
      await page.selectOption('#ix-history-window', '3600');
      await page.click('#ix-history-load');
      await page.waitForFunction(() => {
        const feed = document.getElementById('ix-feed');
        return feed && feed.children.length > 0;
      }, null, { timeout: 15000 });
    },
  },
  { name: 'plans', url: '/console/plans?w=30d' },
  { name: 'retrieval', url: '/console/retrieval?w=30d' },
  // Keep the legacy asset basename so published and cached landing-page URLs
  // stay valid; the captured surface itself is the canonical Context view.
  { name: 'relations', url: '/console/context?scope=project&project=orbital' },
  // The /security/ page's two plans: the Privacy screen's machine, with a
  // compromised agent switched on, in the default setup and with Seamless in
  // its own account. Element shots, not the whole screen.
  { name: 'privacy-shared', url: '/console/privacy', element: '.pv-machine', prepare: (page) => breach(page, 'shared') },
  { name: 'privacy-dedicated', url: '/console/privacy', element: '.pv-machine', prepare: (page) => breach(page, 'dedicated') },
];

// breach picks a setup on the Privacy screen's machine plan and turns its
// Compromised agent switch on.
async function breach(page, mode) {
  await page.click(`[data-pv-mode="${mode}"]`);
  await page.waitForTimeout(400);
  const sw = page.locator('#pv-breach');
  if ((await sw.getAttribute('aria-checked')) !== 'true') await sw.click();
  await page.waitForTimeout(1600);
  await page.locator('.pv-machine').scrollIntoViewIfNeeded();
}

// A freshly seeded instance has never been welcomed, so the Overview opens
// with the first-run level-choice card, which pushes the Knowledge sky below
// the fold. Dismiss it once through its own form (the server keeps the
// choice, so both theme passes see the dismissed page).
async function dismissWelcome(browser) {
  const context = await browser.newContext();
  await context.addCookies([{ name: 'seamless_console', value: COOKIE, url: BASE }]);
  const page = await context.newPage();
  await page.goto(BASE + '/console/', { waitUntil: 'load' });
  // The card's Dismiss is a submit button posting to the welcome route through
  // formaction (its form's own action picks a level).
  const dismiss = page.locator('button[formaction="/console/settings/level/welcome"]');
  if (await dismiss.count()) {
    await Promise.all([
      page.waitForNavigation({ waitUntil: 'load' }),
      dismiss.first().click(),
    ]);
    console.log('dismissed the welcome card');
  }
  await context.close();
}

(async () => {
  const outDir = process.argv[2] || '.';
  // SEAMLESS_SHOT_CHANNEL picks the browser: the installed Chrome by default,
  // or an empty value for the Chromium build playwright-core installs itself
  // (`npx playwright-core install --only-shell chromium`).
  const channel = process.env.SEAMLESS_SHOT_CHANNEL ?? 'chrome';
  const browser = await chromium.launch(channel ? { channel, headless: true } : { headless: true });
  await dismissWelcome(browser);
  for (const theme of ['dark', 'light']) {
    const context = await browser.newContext({
      viewport: { width: 1440, height: 900 },
      deviceScaleFactor: 2,
      colorScheme: theme,
    });
    await context.addCookies([{ name: 'seamless_console', value: COOKIE, url: BASE }]);
    // The console reads its theme from localStorage before first paint.
    await context.addInitScript((t) => {
      try { localStorage.setItem('seamless-theme', t); } catch (e) {}
    }, theme);
    const page = await context.newPage();
    for (const p of PAGES) {
      // 'load', not 'networkidle': the console's SSE stream never goes idle.
      await page.goto(BASE + p.url, { waitUntil: 'load' });
      await page.waitForTimeout(1500);
      if (p.prepare) await p.prepare(page);
      await page.waitForTimeout(500);
      const shot = path.join(outDir, `${p.name}-${theme}.png`);
      if (p.element) await page.locator(p.element).screenshot({ path: shot });
      else await page.screenshot({ path: shot });
      console.log(`captured ${p.name}-${theme}.png`);
    }
    await context.close();
  }
  await browser.close();
})().catch((err) => { console.error(err); process.exit(1); });

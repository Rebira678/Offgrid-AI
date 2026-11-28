// Real-browser check of the phone app (Day 52): pair, go offline, edit, reconnect, see the edits arrive marked late.
//
//   offgrid identity init -key data/site.key; export OFFGRID_IDENTITY_KEY=$PWD/data/site.key
//   offgrid exec -db data/site.db '{"intent":"list_tasks"}'; offgrid store -db data/site.db found -label site
//   offgrid phone pair -db data/site.db -label "Test phone"           # note the code
//   offgrid phone serve -db data/site.db -addr 127.0.0.1:8443 -late-after 3s &
//   CODE=XXXX-XXXX-... PIC=some.jpg node scripts/phone-e2e.js        # needs: npm i playwright
//
// This runs desktop Chromium on localhost, which counts as a secure context. It is NOT a phone test: that is
// docs/phone-spike-report.md, and it is yours.
const { chromium } = require('playwright');
const BASE = process.env.BASE || 'http://localhost:8443';
const CODE = process.env.CODE, PIC = process.env.PIC;
if (!CODE || !PIC) { console.error('set CODE (a pairing code) and PIC (a JPEG file)'); process.exit(2); }
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const ok = (cond, what) => { console.log((cond ? 'PASS ' : 'FAIL ') + what); if (!cond) process.exitCode = 1; };

(async () => {
  const browser = await chromium.launch();
  const anon = await browser.newPage();
  await anon.goto(BASE);
  await anon.waitForSelector('#pair-go');
  ok((await anon.evaluate(() => fetch('/api/state').then((r) => r.status))) === 401, 'an unpaired browser gets 401 from /api/state');
  await anon.close();

  const ctx = await browser.newContext();
  const page = await ctx.newPage();
  await page.goto(BASE);
  await page.fill('#code', CODE);
  await page.fill('#label', 'Test phone');
  await page.click('#pair-go');
  await page.waitForSelector('#new-title');
  await page.evaluate(() => navigator.serviceWorker.ready.then(() => true));
  await sleep(500);
  await page.fill('#new-title', 'online task');
  await page.click('#add');
  await page.waitForSelector('.task:has-text("online task")');
  await page.waitForFunction(() => !document.querySelector('.tag.waiting'));
  ok(true, 'paired and a task made online reached the node');

  await ctx.setOffline(true);
  await page.reload();
  await page.waitForSelector('#new-title');
  ok(await page.locator('.task:has-text("online task")').count() === 1, 'with the node unreachable the app opens and shows the last list');
  await page.fill('#new-title', 'offline task');
  await page.click('#add');
  await page.locator('.task:has-text("online task")').click();
  await page.click('[data-set="done"]');
  await page.fill('#note', 'noted while offline');
  await page.click('#add-note');
  await page.setInputFiles('#photo', PIC);
  await sleep(2500);
  ok(/offline · 4 waiting/.test(await page.textContent('#pill')), 'four changes wait on the phone');
  await sleep(4000);
  await ctx.setOffline(false);
  await page.evaluate(() => window.dispatchEvent(new Event('online')));
  await page.waitForFunction(() => document.querySelector('#pill').textContent === 'online', null, { timeout: 15000 });
  await page.click('#back');
  await page.click('text=All');
  ok((await page.locator('.task .t').allTextContents()).sort().join() === 'offline task,online task', 'both tasks are on the node');
  await page.locator('.task:has-text("online task")').click();
  await sleep(500);
  const notes = (await page.locator('.notes li').allTextContents()).join('\n');
  ok(/noted while offline/.test(notes) && /late, made/.test(notes), 'the note arrived and is marked late');
  ok(await page.locator('.photos img').count() === 1, 'the photo arrived');
  await browser.close();
})().catch((e) => { console.error('FAILED', e); process.exit(1); });

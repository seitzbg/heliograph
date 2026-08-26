// Browser layout regression test for the live dashboard.
//
// The pure-Node tests (dashboard.test.mjs) run without a DOM, so they can only check the CSS
// *string* gridTemplateFor() produces — they cannot see how it interacts with the parent grid,
// the flex legend, or the tab bar. That gap let a real bug ship: at a phone-width viewport the
// Graphs view forced a horizontal page scroll (the mobile `.graphs-layout` column was a bare
// `1fr`, whose min-content floor outgrew the viewport before the per-graph clamp applied; the
// tab strip pushed the page too). This test renders the *actual* dashboard in a real browser,
// against the real collector serving its built-in demo targets, and asserts the observable
// symptom — no horizontal overflow at narrow widths — so a regression is caught at the seam a
// user actually sees.
//
//   run locally:  npm ci && npx playwright install chromium && node web/layout.test.mjs
//   (set SMOKED_BIN=/path/to/smoked to skip the `go run` compile; CI passes the prebuilt binary)

import { spawn, execFileSync } from 'node:child_process';
import { mkdtempSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { chromium } from 'playwright';

// Pick a fresh high port per run unless one is pinned, so a leftover collector from another run or
// session can't answer in our child's place. A fixed port previously let an unrelated process on
// that port produce a false green while our child had already died on a bind failure. Math.random
// is fine here — this is a throwaway test harness, not deterministic production code.
const PORT = process.env.SMOKED_PORT ? Number(process.env.SMOKED_PORT) : 20000 + Math.floor(Math.random() * 20000);
const BASE = `http://127.0.0.1:${PORT}`;
const SMOKED_BIN = process.env.SMOKED_BIN || null;
// Default to Playwright's bundled Chromium (what CI installs). Set PLAYWRIGHT_CHANNEL=chrome to run
// against an already-installed Google Chrome instead, skipping the browser download.
const CHANNEL = process.env.PLAYWRIGHT_CHANNEL || undefined;

// Narrow viewports where the grid/legend/tab-bar interaction previously overflowed. 320px is the
// smallest common phone; 360px is a typical Android width. Both must fit without a page scroll.
const NARROW_WIDTHS = [320, 360];

let failed = 0;
const check = (name, fn) => {
  try { fn(); console.log('ok   -', name); } catch (e) { failed++; console.error('FAIL -', name, '\n      ', e.message); }
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

// Set once we deliberately SIGTERM the collector at the end, so its expected exit is not mistaken
// for an early death.
let stopping = false;
// Records an *unexpected* child exit (failure to bind, an immediately-exiting binary like
// /bin/false, or a crash). Polled during startup AND through the browser assertions so such a death
// fails the test loudly instead of silently exercising whatever else happens to answer on PORT.
let childExit = null;
const died = () => {
  if (childExit) {
    throw new Error(`collector exited (code=${childExit.code}, signal=${childExit.signal}) — did it fail to bind :${PORT}, or crash mid-run?`);
  }
};

// Resolve the collector binary to spawn. A prebuilt one (SMOKED_BIN, what CI passes) is used as-is.
// For a bare `node web/layout.test.mjs`, compile ONCE to a temp binary up front rather than `go run`:
// `go run` keeps the *compiler* alive as the spawned child, so on an occupied port the readiness
// check can latch onto a decoy and return before the freshly compiled collector even tries (and
// fails) to bind — a false green. A prebuilt binary is the server itself, so a bind failure exits the
// child promptly and the childExit guard below catches it.
// tmpBinDir is the mkdtemp dir holding a self-compiled collector (bare-run path only); the `finally`
// below removes it. Tracked at module scope so both the success path and an early failure clean up.
let tmpBinDir = null;
function resolveBin() {
  if (SMOKED_BIN) return SMOKED_BIN;
  tmpBinDir = mkdtempSync(join(tmpdir(), 'smoked-layout-'));
  const bin = join(tmpBinDir, 'smoked');
  console.log('compiling smoked ->', bin);
  try {
    execFileSync('go', ['build', '-o', bin, './cmd/smoked'], { stdio: 'inherit' });
  } catch (e) {
    // A failed build happens BEFORE the main try/finally is entered, so clean up here too rather
    // than leak the ~19MB temp binary/dir on every bare invocation (L2).
    cleanupTmpBin();
    throw e;
  }
  return bin;
}

function cleanupTmpBin() {
  if (tmpBinDir) { rmSync(tmpBinDir, { recursive: true, force: true }); tmpBinDir = null; }
}

// Start the collector serving the demo target set (in-memory store, no -dsn).
function startSmoked(bin) {
  const args = ['-serve', '-addr', `:${PORT}`, '-webdir', 'web'];
  const proc = spawn(bin, args, { stdio: ['ignore', 'inherit', 'inherit'] });
  proc.on('error', (e) => { console.error('failed to start smoked:', e.message); process.exit(1); });
  proc.on('exit', (code, signal) => { if (!stopping) childExit = { code, signal }; });
  return proc;
}

// Poll until the collector answers AND has produced at least one demo round, so the Graphs grid
// has panels to render (an empty grid would make the overflow assertion vacuous).
async function waitForData(timeoutMs) {
  // Let an immediately-dying child (bind failure, a non-server binary like /bin/false, a crash)
  // surface its `exit` event BEFORE we trust anything answering on PORT. Without this a decoy already
  // on the port can answer our very first poll before the child's exit is delivered — the exact race
  // that made the old fixed-port test a false green.
  await sleep(300);
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    died(); // fail loudly on an early death instead of polling whatever else holds PORT
    try {
      const res = await fetch(`${BASE}/api/targets`);
      if (res.ok) {
        const body = await res.json();
        const targets = body.targets || [];
        // Require the child to still be alive to be the process that just answered — closes the
        // window where it dies mid-poll while a decoy responds.
        if (targets.some((t) => !t.no_data)) { died(); return targets.length; }
      }
    } catch { /* not up yet */ }
    await sleep(500);
  }
  throw new Error(`collector produced no demo data within ${timeoutMs}ms`);
}

const smoked = startSmoked(resolveBin());
let browser;
try {
  const targetCount = await waitForData(90_000);
  console.log(`collector ready: ${targetCount} demo targets`);

  browser = await chromium.launch(CHANNEL ? { channel: CHANNEL } : {});
  const page = await browser.newPage();

  for (const width of NARROW_WIDTHS) {
    died(); // backstop: if the collector died since startup, fail rather than test a decoy
    await page.setViewportSize({ width, height: 720 });
    await page.goto(`${BASE}/#graphs`, { waitUntil: 'load' });
    // Wait for the grid to populate from the collector; the tick loop fills it within a few seconds.
    await page.waitForSelector('#graphGrid .gpanel', { timeout: 45_000 });
    await sleep(400); // let the final layout settle after the last panel paints

    const m = await page.evaluate(() => {
      const de = document.documentElement;
      const grid = document.getElementById('graphGrid');
      const layout = document.querySelector('.graphs-layout');
      return {
        vw: window.innerWidth,
        scrollWidth: de.scrollWidth,
        panels: grid.querySelectorAll('.gpanel').length,
        gridRight: Math.round(grid.getBoundingClientRect().right),
        layoutRight: Math.round(layout.getBoundingClientRect().right),
      };
    });

    // Guard: the overflow check only means something if the grid actually rendered panels.
    check(`${width}px: Graphs grid rendered panels`, () => {
      if (m.panels < 1) throw new Error(`no .gpanel rendered (grid empty) — cannot assert layout`);
    });
    // The observable symptom: the page must not scroll horizontally (1px tolerance for sub-pixel).
    check(`${width}px: no horizontal page overflow`, () => {
      if (m.scrollWidth > m.vw + 1) throw new Error(`document scrollWidth ${m.scrollWidth} > viewport ${m.vw} (horizontal scroll)`);
    });
    // The graph grid must stay inside its own layout column, not spill past it.
    check(`${width}px: grid stays within its container`, () => {
      if (m.gridRight > m.layoutRight + 1) throw new Error(`grid right ${m.gridRight} > container right ${m.layoutRight}`);
    });
  }

  // --- CODE_REVIEW M18: only #graphs was covered before; Overview's SLA row (fixed 116px+150px
  // tracks) and the five-column Vantages table also overflowed narrow viewports. Assert neither
  // forces page-level horizontal scroll, at the same phone widths. ---
  for (const width of NARROW_WIDTHS) {
    died();
    await page.setViewportSize({ width, height: 720 });
    await page.goto(`${BASE}/#overview`, { waitUntil: 'load' });
    // Wait for the SLA leaderboard to populate (head + real rows), so the fixed availability/coverage
    // tracks are actually laid out — an empty '—' row wouldn't exercise them.
    await page.waitForSelector('#slaList li:not(.empty-row)', { timeout: 45_000 });
    await sleep(300);
    const ov = await page.evaluate(() => ({
      vw: window.innerWidth,
      sw: document.documentElement.scrollWidth,
      rows: document.querySelectorAll('#slaList li').length,
    }));
    check(`M18: ${width}px Overview rendered its SLA leaderboard`, () => {
      if (ov.rows < 2) throw new Error(`#slaList has ${ov.rows} rows (head+data expected) — cannot assert width`);
    });
    check(`M18: ${width}px Overview has no horizontal page overflow`, () => {
      if (ov.sw > ov.vw + 1) throw new Error(`overview scrollWidth ${ov.sw} > viewport ${ov.vw}`);
    });

    // The demo collector has no admin backend, so the Vantages panel renders "disabled". Drive the
    // responsive TABLE css directly: navigate to the Vantages view (so #viewVantages is actually laid
    // out — on another view it is display:none and every child measures 0), reveal the list markup, and
    // inject a realistic logged-in row (a long name + Regenerate/Revoke). The fix routes the table's
    // width into its own .vadmin-tablewrap scroller, so the document must not widen while the table's
    // content still exceeds the wrapper.
    await page.goto(`${BASE}/#vantages`, { waitUntil: 'load' });
    await page.waitForSelector('#viewVantages', { state: 'visible', timeout: 20_000 });
    const va = await page.evaluate(() => {
      document.getElementById('vantDisabled').classList.add('hidden');
      document.getElementById('vantList').classList.remove('hidden');
      document.getElementById('vantRows').innerHTML =
        '<tr><td>munro-comcast-edge</td><td>Aug 26, 2026</td><td>2m ago</td><td>11</td>' +
        '<td style="text-align:right; white-space:nowrap">' +
        '<button class="vadmin-btn">Regenerate</button><button class="vadmin-btn">Revoke</button></td></tr>';
      const wrap = document.querySelector('.vadmin-tablewrap');
      const table = document.querySelector('.vadmin-table');
      return {
        vw: window.innerWidth,
        sw: document.documentElement.scrollWidth,
        wrapClientW: wrap.clientWidth,
        tableScrollW: table.scrollWidth,
      };
    });
    check(`M18: ${width}px Vantages table does not overflow the page`, () => {
      if (va.sw > va.vw + 1) throw new Error(`vantages scrollWidth ${va.sw} > viewport ${va.vw}`);
    });
    check(`M18: ${width}px the wide Vantages table scrolls inside its wrapper`, () => {
      // The realistic row + the table's min-width make it wider than the phone viewport; the fix must
      // absorb that in the wrapper's own scroller (content wider than the wrapper), not the page.
      if (va.tableScrollW <= va.wrapClientW) throw new Error(`table content ${va.tableScrollW}px did not exceed wrapper ${va.wrapClientW}px — the scroller isn't absorbing the width`);
    });
  }

  // Columns-picker state regression (review L4 + L2): a stored preference too wide for the current
  // viewport must keep the picker showing an ACTIVE, HONESTLY-LABELLED selection — not look unset,
  // and not announce a count that contradicts the rendered layout. Reproduce with graphCols=6 at a
  // width where only ~2 columns fit.
  died();
  await page.setViewportSize({ width: 1440, height: 900 });
  await page.goto(`${BASE}/#graphs`, { waitUntil: 'load' });
  await page.evaluate(() => localStorage.setItem('graphCols', '6'));
  await page.reload({ waitUntil: 'load' }); // re-read the stored preference on a fresh module load
  await page.waitForSelector('#graphGrid .gpanel', { timeout: 45_000 });
  await sleep(400);
  const pick = await page.evaluate(() => {
    const six = document.querySelector('#colsSeg button[data-cols="6"]');
    const grid = document.getElementById('graphGrid');
    const vis = [...grid.querySelectorAll('.gpanel')].filter((e) => e.style.display !== 'none');
    const tops = vis.map((e) => Math.round(e.getBoundingClientRect().top));
    return {
      visible: six.style.display !== 'none',
      pressed: six.getAttribute('aria-pressed') === 'true',
      ariaLabel: six.getAttribute('aria-label') || '',
      effectiveCols: vis.filter((_, i) => tops[i] === tops[0]).length,
    };
  });
  check('wide stored column preference stays a visible, pressed selection', () => {
    if (!pick.visible || !pick.pressed) throw new Error(`selected '6' should stay visible+pressed when it wraps; got ${JSON.stringify(pick)}`);
  });
  check('a wrapping column preference is honestly labelled for assistive tech', () => {
    // The accessible name must convey the effective (wrapped) count, not just announce "6" while a
    // different number of columns renders.
    if (!/wrap/i.test(pick.ariaLabel) || !pick.ariaLabel.includes(String(pick.effectiveCols))) {
      throw new Error(`aria-label should state it wraps to ${pick.effectiveCols}; got "${pick.ariaLabel}"`);
    }
  });

  // Count a canvas's non-transparent pixels — a painted graph has many; a blanked "collecting data…"
  // canvas has almost none. The ratio (during/before) tells a preserved graph from a blanked one
  // without depending on exact rendering.
  const paintedPx = (sel) => page.evaluate((s) => {
    const c = document.querySelector(s);
    if (!c || !c.width) return 0;
    const d = c.getContext('2d').getImageData(0, 0, c.width, c.height).data;
    let n = 0; for (let i = 3; i < d.length; i += 4) if (d[i] > 8) n++;
    return n;
  }, sel);

  // --- CODE_REVIEW M15: a periodic stacked-detail refresh must NOT blank the canvases before the new
  // data has landed (renderStack used to clear #stackGrid at the top, so a slow refresh left it empty). ---
  died();
  await page.setViewportSize({ width: 1440, height: 900 });
  const targetName = await page.evaluate(async () => {
    const b = await (await fetch('/api/targets')).json();
    const t = (b.targets || []).find((x) => !x.no_data) || (b.targets || [])[0];
    return t && t.name;
  });
  check('M15: found a demo target for the stacked view', () => { if (!targetName) throw new Error('no demo target'); });
  await page.goto(`${BASE}/#target=${encodeURIComponent(targetName)}`, { waitUntil: 'load' });
  await page.waitForSelector('#stackGrid canvas', { timeout: 45_000 });
  await sleep(1200); // let a range canvas paint real data
  const stackBefore = await paintedPx('#stackGrid canvas');
  check('M15: the stacked view painted a range canvas', () => {
    if (stackBefore < 150) throw new Error(`no painted #stackGrid canvas (${stackBefore}px) — cannot test`);
  });
  // Stall every range fetch, then wake (visibilitychange -> renderStack(curTarget), the same-target
  // periodic refresh path). Mid-stall, the reused canvas must still show the last-known graph. One
  // regex route (not a glob — a glob `?` is a wildcard); abort is guarded so a late abort of an
  // already-superseded request can't throw an uncaught error into a later test.
  const STALL = 1500;
  const stallRoute = /\/api\/(series|rollup)\?/;
  await page.route(stallRoute, async (r) => { await sleep(STALL); await r.abort().catch(() => {}); });
  await page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
  await sleep(600); // mid-fetch: the refresh has started but not committed
  const stackDuring = await paintedPx('#stackGrid canvas');
  check('M15: stacked canvases stay painted during a slow refresh (not cleared before new data)', () => {
    if (stackDuring < stackBefore * 0.5) throw new Error(`stack canvas blanked mid-refresh: ${stackBefore}px -> ${stackDuring}px`);
  });
  await sleep(STALL); // let the stalled handlers finish before unrouting, so none leaks into the M14 test
  await page.unroute(stallRoute);

  // --- CODE_REVIEW M14: a FAILED /api/series/all refresh must not age out the last-known grid graph,
  // even when the client clock has jumped past the 3h window (the suspended-background-tab scenario). ---
  died();
  await page.goto(`${BASE}/#graphs`, { waitUntil: 'load' });
  await page.waitForSelector('#graphGrid .gpanel canvas', { timeout: 45_000 });
  await sleep(1500); // let a grid panel paint real data
  const gridBefore = await paintedPx('#graphGrid .gpanel canvas');
  check('M14: a grid panel is painted before the failure', () => {
    if (gridBefore < 200) throw new Error(`grid panel not painted (${gridBefore}px) — cannot test preservation`);
  });
  // Capture the EXACT painted bytes, not just a pixel count: the earlier fix still repainted the
  // panel against a fresh [now-3h,now] domain, collapsing the trace off the left edge while leaving
  // an opaque axes/background — which a pixel count reads as "still painted" (a false green the
  // reviewer flagged). The correct behavior is to skip the repaint entirely, so the canvas is
  // byte-identical after a fully-failed refresh. dataURL equality proves that.
  const gridDataURLBefore = await page.evaluate(() => document.querySelector('#graphGrid .gpanel canvas').toDataURL());
  // Fail every series/rollup fetch and jump the clock +4h so the 3h cutoff would drop the whole cache;
  // then wake to force a refresh. The failed fetch must leave the painted graph alone. One regex route
  // (not overlapping globs) — a glob `?` is a wildcard, so `**/api/series?**` also matches
  // `/api/series/all?…` and double-handles the request ("Route is already handled").
  await page.route(/\/api\/(series|rollup)/, (r) => r.abort().catch(() => {}));
  await page.evaluate(() => { const real = Date.now(); Date.now = () => real + 4 * 3600 * 1000; });
  await page.evaluate(() => document.dispatchEvent(new Event('visibilitychange')));
  await sleep(1800); // let the failed refresh settle
  const gridAfter = await paintedPx('#graphGrid .gpanel canvas');
  const gridDataURLAfter = await page.evaluate(() => document.querySelector('#graphGrid .gpanel canvas').toDataURL());
  const statusTxt = await page.evaluate(() => (document.getElementById('statusText') || {}).textContent || '');
  check('M14: a failed refresh keeps the last-known grid graph painted (not blanked)', () => {
    if (gridAfter < gridBefore * 0.5) throw new Error(`grid graph collapsed after a failed refresh: ${gridBefore}px -> ${gridAfter}px`);
  });
  check('M14: a failed refresh leaves the trace untouched (no off-frame repaint)', () => {
    if (gridDataURLAfter !== gridDataURLBefore) throw new Error(`grid canvas was repainted after a fully-failed refresh — the last-known trace should be left exactly as drawn, not re-rendered against a fresh time domain`);
  });
  check('M14: the status honestly reports degraded/last-known on a failed refresh', () => {
    if (!/last known/i.test(statusTxt)) throw new Error(`status did not report last-known: "${statusTxt}"`);
  });
} catch (e) {
  failed++;
  console.error('FAIL - layout test setup:', e.message);
} finally {
  if (browser) await browser.close();
  stopping = true; // our own SIGTERM below is an expected exit, not an early death
  smoked.kill('SIGTERM');
  cleanupTmpBin(); // remove the self-compiled binary + its temp dir (no-op on the SMOKED_BIN path)
}

if (failed) { console.error(`\n${failed} layout check(s) failed`); process.exit(1); }
console.log('\nall layout tests passed');

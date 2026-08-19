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

import { spawn } from 'node:child_process';
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
// /bin/false, or a crash). waitForData polls this so such a death fails the test loudly instead of
// silently exercising whatever else happens to answer on PORT.
let childExit = null;

// Start the collector serving the demo target set (in-memory store, no -dsn). Prefer a prebuilt
// binary; fall back to `go run` for a bare `node web/layout.test.mjs`.
function startSmoked() {
  const [cmd, baseArgs] = SMOKED_BIN ? [SMOKED_BIN, []] : ['go', ['run', './cmd/smoked']];
  const args = [...baseArgs, '-serve', '-addr', `:${PORT}`, '-webdir', 'web'];
  const proc = spawn(cmd, args, { stdio: ['ignore', 'inherit', 'inherit'] });
  proc.on('error', (e) => { console.error('failed to start smoked:', e.message); process.exit(1); });
  proc.on('exit', (code, signal) => { if (!stopping) childExit = { code, signal }; });
  return proc;
}

// Poll until the collector answers AND has produced at least one demo round, so the Graphs grid
// has panels to render (an empty grid would make the overflow assertion vacuous).
async function waitForData(timeoutMs) {
  const died = () => {
    if (childExit) {
      throw new Error(`collector exited before serving (code=${childExit.code}, signal=${childExit.signal}) — did it fail to bind :${PORT}?`);
    }
  };
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

const smoked = startSmoked();
let browser;
try {
  const targetCount = await waitForData(90_000);
  console.log(`collector ready: ${targetCount} demo targets`);

  browser = await chromium.launch(CHANNEL ? { channel: CHANNEL } : {});
  const page = await browser.newPage();

  for (const width of NARROW_WIDTHS) {
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
} catch (e) {
  failed++;
  console.error('FAIL - layout test setup:', e.message);
} finally {
  if (browser) await browser.close();
  stopping = true; // our own SIGTERM below is an expected exit, not an early death
  smoked.kill('SIGTERM');
}

if (failed) { console.error(`\n${failed} layout check(s) failed`); process.exit(1); }
console.log('\nall layout tests passed');

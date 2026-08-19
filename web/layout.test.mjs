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
import { mkdtempSync } from 'node:fs';
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
function resolveBin() {
  if (SMOKED_BIN) return SMOKED_BIN;
  const bin = join(mkdtempSync(join(tmpdir(), 'smoked-layout-')), 'smoked');
  console.log('compiling smoked ->', bin);
  execFileSync('go', ['build', '-o', bin, './cmd/smoked'], { stdio: 'inherit' });
  return bin;
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

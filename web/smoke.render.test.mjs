// Headless render tests for smoke.js. No browser: minimal DOM stubs let the
// module load, and a recording 2D context lets us assert what each mode draws.
//   run:  node web/smoke.render.test.mjs
import { readFileSync } from 'node:fs';
import assert from 'node:assert/strict';

// --- minimal DOM stubs so smoke.js loads and renders headless ---
globalThis.window = { devicePixelRatio: 1 };
globalThis.document = { documentElement: { getAttribute: () => null } };
globalThis.getComputedStyle = () => ({ getPropertyValue: () => '' });
globalThis.matchMedia = () => ({ matches: false });

const code = readFileSync(new URL('./smoke.js', import.meta.url), 'utf8');
(0, eval)(code); // indirect eval in global scope: sets window.Smoke
const Smoke = globalThis.window.Smoke;
assert.ok(Smoke && typeof Smoke.render === 'function', 'Smoke.render loaded');

// A recording 2D context: counts fills/strokes and every globalAlpha assigned.
function recordingCanvas() {
  const log = { alphas: [], fills: 0, strokes: 0, fillRects: 0 };
  const ctx = {
    setTransform() {}, clearRect() {}, fillRect() { log.fillRects++; },
    beginPath() {}, moveTo() {}, lineTo() {}, closePath() {},
    fill() { log.fills++; }, stroke() { log.strokes++; },
    save() {}, restore() {}, translate() {}, rotate() {}, fillText() {}, strokeRect() {},
    set globalAlpha(v) { log.alphas.push(v); }, get globalAlpha() { return 1; },
    set fillStyle(v) {}, get fillStyle() { return ''; },
    set strokeStyle(v) {}, get strokeStyle() { return ''; },
    set lineWidth(v) {}, get lineWidth() { return 1; },
    set font(v) {}, set textBaseline(v) {}, set textAlign(v) {},
  };
  const canvas = { clientWidth: 600, style: {}, width: 0, height: 0, getContext: () => ctx };
  return { canvas, log };
}

// A band series shaped like fromApiRollup output: each bucket a min->max pair
// with the avg as the median line.
function bandSeries() {
  const buckets = [];
  for (let i = 0; i < 6; i++) {
    buckets.push({ centered: [10 + i, 20 + i], samples: [10 + i, 20 + i], lost: 0, median: 15 + i, pings: 2 });
  }
  return { buckets, N: 2, resolution: '1h' };
}

// A raw per-round series (N=4 sorted samples per bucket).
function rawSeries() {
  const buckets = [];
  for (let i = 0; i < 6; i++) {
    buckets.push({ centered: [10, 12, 14, 16], samples: [10, 12, 14, 16], lost: 0, median: 13, pings: 4 });
  }
  return { buckets, N: 4 };
}

let failed = 0;
function check(name, fn) {
  try { fn(); console.log('ok   -', name); } catch (e) { failed++; console.error('FAIL -', name, '\n      ', e.message); }
}

// Band mode must draw a translucent range-area (globalAlpha < 1) — the opaque
// nested smoke stack never sets globalAlpha, so this is the discriminator.
check('band mode fills a translucent range-area', () => {
  const { canvas, log } = recordingCanvas();
  Smoke.render(canvas, bandSeries(), { height: 190, yMax: 30, band: true });
  assert.ok(log.alphas.some((a) => a > 0 && a < 1), 'expected a translucent fill (globalAlpha < 1)');
  assert.ok(log.fills > 0, 'expected the area to be filled');
  assert.ok(log.strokes > 0, 'expected the median line to still be drawn');
});

// Band mode must restore full opacity so nothing after it is faded.
check('band mode restores globalAlpha to 1', () => {
  const { canvas, log } = recordingCanvas();
  Smoke.render(canvas, bandSeries(), { height: 190, yMax: 30, band: true });
  assert.equal(log.alphas[log.alphas.length - 1], 1, 'last globalAlpha assignment should be 1');
});

// Raw mode keeps the dense opaque smoke stack (never goes translucent).
check('raw mode stays opaque', () => {
  const { canvas, log } = recordingCanvas();
  Smoke.render(canvas, rawSeries(), { height: 190, yMax: 20 });
  assert.ok(!log.alphas.some((a) => a > 0 && a < 1), 'raw smoke stack should stay opaque');
});

if (failed) { console.error(`\n${failed} test(s) failed`); process.exit(1); }
console.log('\nall smoke.render tests passed');

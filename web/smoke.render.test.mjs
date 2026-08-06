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

// A recording 2D context: counts fills/strokes and every globalAlpha assigned, and
// captures each completed sub-path's coordinates (strokePaths/fillPaths) so tests can
// assert WHERE things are drawn — needed to check wall-clock X placement + gap breaks.
function recordingCanvas() {
  const log = { alphas: [], fills: 0, strokes: 0, fillRects: 0, strokePaths: [], fillPaths: [] };
  let cur = [];
  const ctx = {
    setTransform() {}, clearRect() {}, fillRect() { log.fillRects++; },
    beginPath() { cur = []; }, moveTo(x, y) { cur.push({ op: 'M', x, y }); }, lineTo(x, y) { cur.push({ op: 'L', x, y }); }, closePath() {},
    fill() { log.fills++; log.fillPaths.push(cur.slice()); }, stroke() { log.strokes++; log.strokePaths.push(cur.slice()); },
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

// The median base line is the one long multi-point stroke (grid lines + loss segments
// are 2-point strokes); its recorded X coords reveal the time->pixel mapping + breaks.
function longestStroke(log) { return log.strokePaths.reduce((a, b) => (b.length > a.length ? b : a), []); }

// A raw per-round series (N=4) with explicit per-bucket wall-clock timestamps (ms).
function timedSeries(times) {
  return {
    buckets: times.map((t, i) => ({ centered: [10, 12, 14, 16], samples: [10, 12, 14, 16], lost: 0, median: 13 + (i % 3), pings: 4, t })),
    N: 4,
  };
}
// plot geometry for clientWidth 600: mL=48, mR=12 -> pw=540
const GEO = { mL: 48, pw: 600 - 48 - 12 };

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

// --- #3 timestamp-accurate graphs: adapters must retain wall-clock timestamps ---

// fromApiSeries must carry each round's `t` through as a numeric `.t` (epoch ms) so
// the renderer can place buckets by wall-clock time instead of array index.
check('fromApiSeries retains per-round timestamps as numeric .t', () => {
  const resp = {
    rounds: [
      { t: '2026-08-06T12:00:00Z', median_ms: 13, loss: 0, pings: 4, rtts_ms: [10, 12, 14, 16] },
      { t: '2026-08-06T12:01:00Z', median_ms: 14, loss: 0, pings: 4, rtts_ms: [11, 13, 15, 17] },
    ],
  };
  const s = Smoke.fromApiSeries(resp);
  assert.equal(s.buckets[0].t, Date.parse('2026-08-06T12:00:00Z'), 'bucket 0 .t = parsed t');
  assert.equal(s.buckets[1].t, Date.parse('2026-08-06T12:01:00Z'), 'bucket 1 .t = parsed t');
  assert.equal(typeof s.buckets[0].t, 'number');
});

// fromApiRollup must carry each bucket's `bucket` timestamp through as numeric `.t`.
check('fromApiRollup retains per-bucket timestamps as numeric .t', () => {
  const resp = {
    resolution: '1h',
    buckets: [
      { bucket: '2026-08-06T12:00:00Z', median_avg_ms: 15, median_min_ms: 10, median_max_ms: 20, loss_pct: 0, rounds: 60 },
      { bucket: '2026-08-06T13:00:00Z', median_avg_ms: 16, median_min_ms: 11, median_max_ms: 21, loss_pct: 0, rounds: 60 },
    ],
  };
  const s = Smoke.fromApiRollup(resp);
  assert.equal(s.buckets[0].t, Date.parse('2026-08-06T12:00:00Z'), 'bucket 0 .t = parsed bucket');
  assert.equal(s.buckets[1].t, Date.parse('2026-08-06T13:00:00Z'), 'bucket 1 .t = parsed bucket');
  assert.equal(typeof s.buckets[0].t, 'number');
});

// With a time domain (t0/t1) + per-bucket .t, X must map by wall-clock, so unevenly
// spaced buckets are NOT evenly spread. Buckets at 0/60s/180s in a [0,180s] domain put
// the middle one at 1/3 of the width, not the 1/2 an index would give.
check('render places buckets by wall-clock time, not array index', () => {
  const T = Date.parse('2026-08-06T00:00:00Z');
  const s = timedSeries([T, T + 60000, T + 180000]);
  const { canvas, log } = recordingCanvas();
  Smoke.render(canvas, s, { height: 190, yMax: 20, t0: T, t1: T + 180000 });
  const path = longestStroke(log);
  assert.equal(path.length, 3, 'median base line has one point per bucket');
  const [x0, x1, x2] = path.map((p) => p.x);
  assert.ok(Math.abs(x0 - GEO.mL) < 2, `left edge (got ${x0})`);
  assert.ok(Math.abs(x2 - (GEO.mL + GEO.pw)) < 2, `right edge (got ${x2})`);
  assert.ok(Math.abs(x1 - (GEO.mL + GEO.pw / 3)) < 3, `middle bucket time-placed at 1/3 (got ${x1}, index would be ~${GEO.mL + GEO.pw / 2})`);
});

// Back-compat: with no time domain (smoke-poc.html, existing callers) X stays index-based.
check('render falls back to array-index spacing when no time domain is given', () => {
  const T = Date.parse('2026-08-06T00:00:00Z');
  const s = timedSeries([T, T + 60000, T + 180000]);
  const { canvas, log } = recordingCanvas();
  Smoke.render(canvas, s, { height: 190, yMax: 20 }); // no t0/t1
  const x1 = longestStroke(log)[1].x;
  assert.ok(Math.abs(x1 - (GEO.mL + GEO.pw / 2)) < 3, `index spacing puts middle bucket at 1/2 (got ${x1})`);
});

// A time gap larger than the inferred cadence must break the median line (pen-up) so
// the graph never draws a straight line across an outage.
check('render breaks the median line across a time gap (pen-up, no bridge)', () => {
  const T = Date.parse('2026-08-06T00:00:00Z');
  const times = [T, T + 60000, T + 3660000, T + 3720000]; // 60s, then a 1h hole, then 60s
  const s = timedSeries(times);
  const { canvas, log } = recordingCanvas();
  Smoke.render(canvas, s, { height: 190, yMax: 20, t0: T, t1: T + 3720000 });
  const path = longestStroke(log);
  assert.equal(path.filter((p) => p.op === 'M').length, 2, 'a gap starts a fresh subpath (2 moveTo, not 1)');
  const dom = 3720000, xAt = (t) => GEO.mL + GEO.pw * (t - T) / dom;
  const x1 = xAt(times[1]), x2 = xAt(times[2]);
  const bridges = log.strokePaths.some((p) => p.length === 2 && Math.abs(p[0].x - x1) < 2 && Math.abs(p[1].x - x2) < 2);
  assert.ok(!bridges, 'no 2-point loss segment spans the gap');
});

// #4: a sparse series where the outage itself dominates the diffs must STILL break.
// Two diffs — 60s then 9min — would let a plain median-of-diffs cadence (which picks
// the 9-min diff) bridge the hole; a robust low-quantile cadence (~60s) breaks it.
check('render breaks a sparse outage a median cadence would bridge', () => {
  const T = Date.parse('2026-08-06T00:00:00Z');
  const times = [T, T + 60000, T + 60000 + 540000]; // 60s, then a 9-minute hole
  const s = timedSeries(times);
  const { canvas, log } = recordingCanvas();
  Smoke.render(canvas, s, { height: 190, yMax: 20, t0: T, t1: times[2] });
  const path = longestStroke(log);
  assert.equal(path.filter((p) => p.op === 'M').length, 2, 'the 9-min hole starts a fresh subpath (pen-up), not one connected line');
});

// gapThreshold: the break threshold must come from the tight cadence, not be inflated
// by a few large gaps in an otherwise-regular (or sparse) series.
check('gapThreshold uses a low quantile so large gaps do not inflate the cadence', () => {
  const bk = (t) => ({ t });
  // dense 60s cadence with one 1h hole: hole (3.6M) exceeds the ~90s threshold
  const dense = Smoke.gapThreshold([bk(0), bk(60000), bk(120000), bk(180000), bk(3780000)], 1.5);
  assert.ok(3600000 > dense, `dense hole must exceed threshold ${dense}`);
  // two diffs, 60s then 9min: cadence ~60s, so the 540s hole breaks
  const sparse = Smoke.gapThreshold([bk(0), bk(60000), bk(600000)], 1.5);
  assert.ok(540000 > sparse && sparse < 200000, `sparse threshold ${sparse} must be near the 60s cadence, not the 9-min gap`);
  // fewer than two timestamps -> no gaps possible
  assert.equal(Smoke.gapThreshold([bk(0)], 1.5), Infinity);
});

// Short data must occupy only its true fraction of a longer selected window (10 min of
// data on a 3h axis lives in the rightmost sliver, not stretched across the whole plot).
check('short data occupies only its true fraction of a longer window', () => {
  const T = Date.parse('2026-08-06T00:00:00Z');
  const times = []; for (let i = 0; i < 10; i++) times.push(T + i * 60000); // 9 min of data
  const s = timedSeries(times);
  const t1 = times[times.length - 1], t0 = t1 - 3 * 3600 * 1000; // 3h window
  const { canvas, log } = recordingCanvas();
  Smoke.render(canvas, s, { height: 190, yMax: 20, t0, t1 });
  const minX = Math.min(...longestStroke(log).map((p) => p.x));
  assert.ok(minX >= GEO.mL + GEO.pw * 0.9, `all data sits in the right 10% (min X ${minX})`);
});

// seriesStats must weight each rollup bucket by the number of rounds it aggregates, so
// a sparse bucket (few rounds) doesn't count the same as a full one. A 60-round 0%-loss
// bucket and a 2-round 100%-loss bucket average to ~3.2% loss, not the unweighted 50%.
check('seriesStats weights loss/median by each bucket round count', () => {
  const s = Smoke.fromApiRollup({
    resolution: '1h',
    buckets: [
      { bucket: '2026-08-06T00:00:00Z', median_avg_ms: 10, median_min_ms: 8, median_max_ms: 12, loss_pct: 0, rounds: 60 },
      { bucket: '2026-08-06T01:00:00Z', median_avg_ms: 20, median_min_ms: 18, median_max_ms: 22, loss_pct: 100, rounds: 2 },
    ],
  });
  const st = Smoke.seriesStats(s);
  assert.ok(st.lossAvg < 5, `loss avg ${st.lossAvg} should be round-weighted (~3.2%), not the unweighted 50%`);
  // median avg likewise weighted: (10*60 + 20*2)/62 ≈ 10.3, not the unweighted 15.
  assert.ok(st.medAvg < 12, `median avg ${st.medAvg} should be round-weighted (~10.3), not the unweighted 15`);
});

if (failed) { console.error(`\n${failed} test(s) failed`); process.exit(1); }
console.log('\nall smoke.render tests passed');

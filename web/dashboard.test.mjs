// Unit tests for dashboard.js pure helpers (routing + range mapping + slicing).
// DOM-touching code runs only on DOMContentLoaded, which never fires under Node,
// so loading the file just exposes window.Dash.
//   run:  node web/dashboard.test.mjs
import { readFileSync } from 'node:fs';
import assert from 'node:assert/strict';

globalThis.window = {};
globalThis.document = { addEventListener() {}, readyState: 'loading' };
globalThis.matchMedia = () => ({ matches: false, addEventListener() {} });
globalThis.getComputedStyle = () => ({ getPropertyValue: () => '' });
globalThis.localStorage = { getItem() { return null; }, setItem() {} };

const code = readFileSync(new URL('./dashboard.js', import.meta.url), 'utf8');
(0, eval)(code);
const D = globalThis.window.Dash;
assert.ok(D && typeof D.parseRoute === 'function', 'window.Dash.parseRoute exported');

let failed = 0;
const check = (n, f) => { try { f(); console.log('ok   -', n); } catch (e) { failed++; console.error('FAIL -', n, '\n      ', e.message); } };

check('parseRoute: overview is the default', () => {
  assert.deepEqual(D.parseRoute(''), { view: 'overview' });
  assert.deepEqual(D.parseRoute('#'), { view: 'overview' });
  assert.deepEqual(D.parseRoute('#overview'), { view: 'overview' });
});
check('parseRoute: graphs', () => { assert.deepEqual(D.parseRoute('#graphs'), { view: 'graphs' }); });
check('parseRoute: target with no range -> stack (all four ranges)', () => {
  assert.deepEqual(D.parseRoute('#target=Cloudflare%20DNS'), { view: 'stack', name: 'Cloudflare DNS' });
});
check('parseRoute: target with a valid range -> zoom', () => {
  assert.deepEqual(D.parseRoute('#target=Home&range=10d'), { view: 'zoom', name: 'Home', range: '10d' });
});
check('parseRoute: an unknown range falls back to the stack', () => {
  assert.deepEqual(D.parseRoute('#target=Home&range=zzz'), { view: 'stack', name: 'Home' });
});
check('RANGES: raw vs band tiers map to the right endpoints', () => {
  assert.equal(D.RANGES['3h'].mode, 'raw'); assert.equal(D.RANGES['3h'].window, '3h');
  assert.equal(D.RANGES['30h'].mode, 'raw'); assert.equal(D.RANGES['30h'].window, '30h');
  assert.equal(D.RANGES['10d'].mode, 'band'); assert.equal(D.RANGES['10d'].res, '1h');
  assert.equal(D.RANGES['400d'].mode, 'band'); assert.equal(D.RANGES['400d'].res, '1d');
  assert.deepEqual(D.RANGE_ORDER, ['3h', '30h', '10d', '400d']);
});
check('RANGES: each tier carries the correct wall-clock window in ms', () => {
  // Used to set the fixed X domain [now-windowMs, now] so the graph axis means
  // exactly what it says and short data floats at its true position (#3).
  assert.equal(D.RANGES['3h'].windowMs, 3 * 3600 * 1000);
  assert.equal(D.RANGES['30h'].windowMs, 30 * 3600 * 1000);
  assert.equal(D.RANGES['10d'].windowMs, 10 * 86400 * 1000);
  assert.equal(D.RANGES['400d'].windowMs, 400 * 86400 * 1000);
});
check('parseRoute: does not double-decode the target name', () => {
  // URLSearchParams already percent-decodes, so a literal %20 in the name must
  // survive as-is (not become a space), and a name with % must not throw.
  assert.deepEqual(D.parseRoute('#target=literal%2520name'), { view: 'stack', name: 'literal%20name' });
  assert.deepEqual(D.parseRoute('#target=CPU%252099%2525'), { view: 'stack', name: 'CPU%2099%25' });
});

// mergeSeries powers the incremental Graphs grid (#2): it folds the rounds newer than
// the watermark into the panel's cached series, drops rounds older than the window,
// and keeps oldest->newest — so a refresh transfers only new rounds, not the whole 3h.
const bkt = (t, med) => ({ t, median: med == null ? t / 1000 : med, centered: [1], samples: [1], lost: 0, pings: 1 });
check('mergeSeries appends only newer rounds, trims to the cutoff, dedupes the boundary', () => {
  const prev = { buckets: [bkt(1000), bkt(2000), bkt(3000)], N: 1 };
  const incoming = { buckets: [bkt(3000), bkt(4000), bkt(5000)], N: 1 }; // 3000 overlaps the boundary
  const merged = D.mergeSeries(prev, incoming, 2000); // cutoff drops t<2000
  assert.deepEqual(merged.buckets.map((b) => b.t), [2000, 3000, 4000, 5000]);
});
check('mergeSeries from empty prev takes all incoming within the window', () => {
  const merged = D.mergeSeries(null, { buckets: [bkt(100), bkt(200), bkt(300)], N: 1 }, 150);
  assert.deepEqual(merged.buckets.map((b) => b.t), [200, 300]);
});
check('mergeSeries with no new rounds keeps the previous series (trimmed)', () => {
  const prev = { buckets: [bkt(1000), bkt(2000), bkt(3000)], N: 1 };
  const merged = D.mergeSeries(prev, { buckets: [], N: 0 }, 0);
  assert.deepEqual(merged.buckets.map((b) => b.t), [1000, 2000, 3000]);
});

if (failed) { console.error(`\n${failed} test(s) failed`); process.exit(1); }
console.log('\nall dashboard tests passed');

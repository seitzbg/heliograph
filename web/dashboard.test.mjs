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
check('sliceByCutoff: keeps buckets at/after the cutoff, drops older', () => {
  const b = [{ bucket: '2026-01-01T00:00:00Z' }, { bucket: '2026-06-01T00:00:00Z' }];
  const out = D.sliceByCutoff(b, Date.parse('2026-03-01T00:00:00Z'));
  assert.equal(out.length, 1);
  assert.equal(out[0].bucket, '2026-06-01T00:00:00Z');
});

if (failed) { console.error(`\n${failed} test(s) failed`); process.exit(1); }
console.log('\nall dashboard tests passed');

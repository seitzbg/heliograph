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

if (failed) { console.error(`\n${failed} test(s) failed`); process.exit(1); }
console.log('\nall dashboard tests passed');

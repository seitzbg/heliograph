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

// gridSince (#1): the incremental watermark is the OLDEST frontier among panels that
// hold data, so the shared `since` never advances past the slowest-updating target — a
// global max would skip that target's late rounds permanently.
const panel = (ts) => ({ series: ts == null ? null : { buckets: ts.map((t) => ({ t })) } });
check('gridSince returns the oldest panel frontier, ignoring empty panels', () => {
  assert.equal(D.gridSince([panel([1000, 5000]), panel([1000, 3000]), panel([1000, 9000])]), 3000);
  assert.equal(D.gridSince([panel([1000, 5000]), panel(null)]), 5000); // empty panel ignored
  assert.equal(D.gridSince([panel(null), panel([])]), null);           // no data anywhere -> full fetch
  assert.equal(D.gridSince([]), null);
});
check('gridSince skips a non-finite frontier timestamp', () => {
  const p = { series: { buckets: [{ t: 1000 }, { t: NaN }] } }; // NaN frontier -> skip this panel
  const q = { series: { buckets: [{ t: 2000 }] } };
  assert.equal(D.gridSince([p, q]), 2000);
});

// zoomResolution: a drag-selected span maps to the fetch tier that best resolves it —
// raw for short spans, then hourly, then daily bands (so zooming a long view refetches
// finer data). Boundaries are inclusive of the finer tier.
check('zoomResolution picks the fetch tier by span', () => {
  const HOUR = 3600 * 1000, DAY = 86400 * 1000;
  assert.deepEqual(D.zoomResolution(3 * HOUR), { mode: 'raw' });
  assert.deepEqual(D.zoomResolution(30 * HOUR), { mode: 'raw' });               // <=30h -> raw
  assert.deepEqual(D.zoomResolution(31 * HOUR), { mode: 'band', res: '1h' });
  assert.deepEqual(D.zoomResolution(10 * DAY), { mode: 'band', res: '1h' });    // <=10d -> hourly
  assert.deepEqual(D.zoomResolution(11 * DAY), { mode: 'band', res: '1d' });
  assert.deepEqual(D.zoomResolution(400 * DAY), { mode: 'band', res: '1d' });
});

// pixelToTime inverts render's X mapping within the plot area (mL..width-mR), clamped.
check('pixelToTime inverts the domain mapping and clamps to the plot', () => {
  globalThis.Smoke = { MARGINS: { mL: 48, mR: 12, mT: 10, mB: 22 } };
  const W = 600; // pw = 600 - 48 - 12 = 540
  const t0 = 1000, t1 = 1000 + 540000; // 1ms per pixel
  assert.equal(D.pixelToTime(48, W, t0, t1), t0);                 // left edge -> t0
  assert.equal(D.pixelToTime(588, W, t0, t1), t1);                // right edge (48+540) -> t1
  assert.equal(D.pixelToTime(48 + 270, W, t0, t1), (t0 + t1) / 2); // middle
  assert.equal(D.pixelToTime(0, W, t0, t1), t0);                  // past left -> clamped to t0
  assert.equal(D.pixelToTime(9999, W, t0, t1), t1);              // past right -> clamped to t1
});

// sharedYMax: the Graphs grid's unison Y-axis is the max per-panel robustMax across all
// panels that hold data, so small multiples are visually comparable; undefined (per-panel
// auto-scale) when nothing has data.
check('sharedYMax is the max robustMax over panels with data', () => {
  globalThis.Smoke = { robustMax: (s) => s.buckets[0].m }; // marker: robustMax = first bucket's .m
  const s = (m) => ({ buckets: [{ m }] });
  assert.equal(D.sharedYMax([s(5), s(50), s(20)]), 50);
  assert.equal(D.sharedYMax([s(5), { buckets: [] }, null]), 5); // empty/absent panels ignored
  assert.equal(D.sharedYMax([{ buckets: [] }, null]), undefined); // no data anywhere
  assert.equal(D.sharedYMax([]), undefined);
});

// fetchJSON (#2): a non-2xx response must reject (so callers keep last-known state
// instead of decoding an error body as empty data and wiping the view). 2xx returns JSON.
async function checkAsync(n, f) { try { await f(); console.log('ok   -', n); } catch (e) { failed++; console.error('FAIL -', n, '\n      ', e.message); } }
await checkAsync('fetchJSON rejects a non-2xx response instead of decoding it', async () => {
  globalThis.fetch = async () => ({ ok: false, status: 503, json: async () => ({ error: 'store unavailable' }) });
  await assert.rejects(() => D.fetchJSON('/api/targets'));
});
await checkAsync('fetchJSON returns decoded JSON on a 2xx response', async () => {
  globalThis.fetch = async () => ({ ok: true, status: 200, json: async () => ({ targets: [{ name: 'a' }] }) });
  assert.deepEqual(await D.fetchJSON('/api/targets'), { targets: [{ name: 'a' }] });
});

// buildTree: the config-tree menu (left nav). Splits each target name on '/', nests
// folders, and marks the exact path that is itself a monitored target.
check('buildTree nests names into folders and marks targets', () => {
  const t = D.buildTree(['datacenters/us-east/edge', 'datacenters/us-west/edge', 'cdn/cf']);
  assert.equal(t.length, 2);
  const dc = t.find((n) => n.name === 'datacenters');
  assert.equal(dc.path, 'datacenters'); assert.equal(dc.target, null);
  assert.deepEqual(dc.children.map((c) => c.name), ['us-east', 'us-west']); // siblings sorted
  const east = dc.children[0];
  assert.equal(east.path, 'datacenters/us-east');
  assert.equal(east.children[0].target, 'datacenters/us-east/edge');
  const cdn = t.find((n) => n.name === 'cdn');
  assert.equal(cdn.children[0].target, 'cdn/cf');
});
check('buildTree: a node that is both a target and a folder', () => {
  const t = D.buildTree(['a', 'a/b']);
  assert.equal(t.length, 1);
  assert.equal(t[0].name, 'a'); assert.equal(t[0].target, 'a'); // 'a' is a target...
  assert.equal(t[0].children.length, 1);                        // ...and a folder
  assert.equal(t[0].children[0].target, 'a/b');
});
check('buildTree sorts siblings and tolerates empty input', () => {
  assert.deepEqual(D.buildTree([]), []);
  assert.deepEqual(D.buildTree(null), []);
  const t = D.buildTree(['b', 'a', 'a/z', 'a/a']);
  assert.deepEqual(t.map((n) => n.name), ['a', 'b']);
  assert.deepEqual(t[0].children.map((n) => n.name), ['a', 'z']);
});

// underPath: a target is within a folder scope iff it equals the path or sits beneath it
// on a '/' boundary — so 'ab' is NOT under 'a'. Empty scope means "everything".
check('underPath matches exact, descendants, and all for empty', () => {
  assert.equal(D.underPath('a/b', ''), true);      // no scope -> everything
  assert.equal(D.underPath('a/b', 'a'), true);     // descendant
  assert.equal(D.underPath('a/b', 'a/b'), true);   // exact
  assert.equal(D.underPath('a/bc', 'a/b'), false); // not a '/'-boundary prefix
  assert.equal(D.underPath('ab', 'a'), false);
});

// targetStatus: heuristic dot severity for the menu — down on a probe error or heavy
// loss, degraded on light loss, else ok. (Authoritative availability is the Overview tab.)
check('targetStatus maps a DTO to a dot severity', () => {
  assert.equal(D.targetStatus({ error: 'timeout', loss_pct: 0 }), 'down');
  assert.equal(D.targetStatus({ loss_pct: 100 }), 'down');
  assert.equal(D.targetStatus({ loss_pct: 50 }), 'down');
  assert.equal(D.targetStatus({ loss_pct: 5 }), 'degraded');
  assert.equal(D.targetStatus({ loss_pct: 0.6 }), 'degraded');
  assert.equal(D.targetStatus({ loss_pct: 0.5 }), 'ok');
  assert.equal(D.targetStatus({ loss_pct: 0 }), 'ok');
  assert.equal(D.targetStatus(null), 'ok');
  // A configured target with no stored round for this vantage is neutral, not a false green (P1-3).
  assert.equal(D.targetStatus({ no_data: true }), 'nodata');
  assert.equal(D.targetStatus({ no_data: true, loss_pct: 0 }), 'nodata');
});

// parseRoute: the Graphs view carries an optional folder scope for the config-tree menu,
// deep-linkable and decoded once (like target names).
check('parseRoute: graphs carries an optional folder path', () => {
  assert.deepEqual(D.parseRoute('#graphs'), { view: 'graphs' });
  assert.deepEqual(D.parseRoute('#graphs&path=datacenters'), { view: 'graphs', path: 'datacenters' });
  assert.deepEqual(D.parseRoute('#graphs&path=a%2Fb'), { view: 'graphs', path: 'a/b' }); // decoded once
});

// pickSeries (#5): a transient fetch failure (null) keeps the cached last-good so a detail
// graph isn't blanked; a real series is rendered and cached; the 'unsupported' sentinel is
// rendered but not cached as data.
check('pickSeries keeps last-good on failure and caches real series', () => {
  const good = { buckets: [{}, {}] };
  assert.deepEqual(D.pickSeries(good, null), { series: good, cache: good, failed: false });
  assert.deepEqual(D.pickSeries(null, good), { series: good, cache: good, failed: true });
  assert.deepEqual(D.pickSeries(null, null), { series: null, cache: null, failed: true });
  const uns = { unsupported: true };
  assert.deepEqual(D.pickSeries(uns, good), { series: uns, cache: good, failed: false });
  assert.deepEqual(D.pickSeries(uns, null), { series: uns, cache: null, failed: false });
});

// Vantage helpers (federation PR #6, Task 2): vantageList/orderVantages/defaultFocus/
// vantageColorVar are pure and feed the overlay UI (Task 3) — list defaulting, a stable
// render order (local first, then sorted, deduped), the focus default, and a stable
// per-vantage color-var assignment keyed off that order.
check('vantageList defaults to [local]', () => {
  assert.deepEqual(D.vantageList({ name: 'a' }), ['local']);
  assert.deepEqual(D.vantageList({ name: 'a', vantages: [] }), ['local']);
  assert.deepEqual(D.vantageList({ name: 'a', vantages: ['nyc', 'local'] }), ['nyc', 'local']);
});
check('orderVantages puts local first then sorts', () => {
  assert.deepEqual(D.orderVantages(['nyc', 'local', 'fra']), ['local', 'fra', 'nyc']);
  assert.deepEqual(D.orderVantages(['nyc', 'nyc']), ['nyc']);
});
check('defaultFocus prefers local', () => {
  assert.equal(D.defaultFocus(['local', 'nyc']), 'local');
  assert.equal(D.defaultFocus(['fra', 'nyc']), 'fra');
});
// keepFocus preserves a still-valid selection across a re-render (the 30s auto-refresh must not
// reset a user's chosen vantage chip), else falls back to defaultFocus (CodeRabbit #16/#17).
check('keepFocus preserves a valid current focus, else defaults', () => {
  assert.equal(D.keepFocus('nyc', ['local', 'nyc', 'fra']), 'nyc'); // kept across refresh
  assert.equal(D.keepFocus('tokyo', ['local', 'nyc']), 'local');    // stale -> default (local present)
  assert.equal(D.keepFocus(null, ['fra', 'nyc']), 'fra');           // no current -> default
  assert.equal(D.keepFocus('', ['local', 'nyc']), 'local');         // empty current -> default
});
check('vantageColorVar: local neutral, others stable palette', () => {
  const ord = D.orderVantages(['local', 'nyc', 'fra']); // [local, fra, nyc]
  assert.equal(D.vantageColorVar('local', ord), '--median-base');
  assert.notEqual(D.vantageColorVar('nyc', ord), D.vantageColorVar('fra', ord));
  assert.equal(D.vantageColorVar('nyc', ord), D.vantageColorVar('nyc', ord)); // stable
});

check('adminMode maps HTTP status -> panel mode', () => {
  assert.equal(D.adminMode(200), 'list');
  assert.equal(D.adminMode(401), 'login');
  assert.equal(D.adminMode(404), 'disabled');
  assert.equal(D.adminMode(503), 'error');
  assert.equal(D.adminMode(0), 'error');
});
check('relTime: never / just now / minutes / hours / days', () => {
  const now = Date.parse('2026-08-08T12:00:00Z');
  assert.equal(D.relTime(null, now), 'never');
  assert.equal(D.relTime('', now), 'never');
  assert.equal(D.relTime(0, now), 'never');
  assert.equal(D.relTime('not-a-date', now), 'never');
  assert.equal(D.relTime('2026-08-08T11:59:50Z', now), 'just now');
  assert.equal(D.relTime('2026-08-08T11:58:00Z', now), '2m ago');
  assert.equal(D.relTime('2026-08-08T09:00:00Z', now), '3h ago');
  assert.equal(D.relTime('2026-08-06T12:00:00Z', now), '2d ago');
});

// DB config fragment helpers (config-in-a-database slice 3, Task 1): pure
// list/add/edit/remove/buildNode over a { targets: { children: {...} } } fragment —
// the Config panel's read-modify-write core (Tasks 2-3 build the DOM around these).
check('listTargets: flattens + sorts + flags folders', () => {
  const doc = { targets: { children: {
    b: { probe: 'HTTP', host: 'b.example' },
    a: { probe: 'TCPConnect', host: 'a.example' },
    grp: { children: { x: { probe: 'HTTP', host: 'x' } } },
  } } };
  const rows = D.listTargets(doc);
  assert.deepEqual(rows.map((r) => r.name), ['a', 'b', 'grp']);
  assert.equal(rows.find((r) => r.name === 'grp').isFolder, true);
  assert.equal(rows.find((r) => r.name === 'a').isFolder, false);
});
check('listTargets: empty/absent children -> []', () => {
  assert.deepEqual(D.listTargets({}), []);
  assert.deepEqual(D.listTargets({ targets: {} }), []);
});
check('addTarget: sets child, rejects dup, does not mutate input', () => {
  const doc = { targets: { children: { a: { probe: 'HTTP', host: 'a' } } } };
  const out = D.addTarget(doc, 'b', { probe: 'HTTP', host: 'b' });
  assert.deepEqual(Object.keys(out.targets.children).sort(), ['a', 'b']);
  assert.deepEqual(Object.keys(doc.targets.children), ['a']); // input untouched
  assert.throws(() => D.addTarget(out, 'a', {}), /already exists/);
});
check('editTarget: replaces existing, throws on missing', () => {
  const doc = { targets: { children: { a: { probe: 'HTTP', host: 'a' } } } };
  const out = D.editTarget(doc, 'a', { probe: 'DNS', host: 'a2' });
  assert.equal(out.targets.children.a.probe, 'DNS');
  assert.equal(doc.targets.children.a.probe, 'HTTP'); // input untouched
  assert.throws(() => D.editTarget(doc, 'nope', {}), /no target/);
});
check('removeTarget: deletes, input untouched', () => {
  const doc = { targets: { children: { a: {}, b: {} } } };
  const out = D.removeTarget(doc, 'a');
  assert.deepEqual(Object.keys(out.targets.children), ['b']);
  assert.deepEqual(Object.keys(doc.targets.children).sort(), ['a', 'b']);
});
check('buildTargetNode: drops empties, params -> strings', () => {
  const n = D.buildTargetNode({ probe: 'TCPConnect', host: 'h', params: { port: 80, '': 'x', extra: '' }, vantages: ['nyc', '  ', ''], alerts: [] });
  assert.deepEqual(n, { probe: 'TCPConnect', host: 'h', params: { port: '80', extra: '' }, vantages: ['nyc'] });
  // empty everything -> {}
  assert.deepEqual(D.buildTargetNode({ probe: '', host: '', params: {}, vantages: [], alerts: [] }), {});
});

if (failed) { console.error(`\n${failed} test(s) failed`); process.exit(1); }
console.log('\nall dashboard tests passed');

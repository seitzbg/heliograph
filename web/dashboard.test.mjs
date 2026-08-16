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
  assert.deepEqual(dc.children.map((c) => c.name), ['us-east', 'us-west']); // first-encounter order
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
check('buildTree preserves input order and tolerates empty input', () => {
  assert.deepEqual(D.buildTree([]), []);
  assert.deepEqual(D.buildTree(null), []);
  const t = D.buildTree(['b', 'a', 'a/z', 'a/a']);
  assert.deepEqual(t.map((n) => n.name), ['b', 'a']);
  assert.deepEqual(t[1].children.map((n) => n.name), ['z', 'a']);
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
// loss, degraded on SUSTAINED (windowed) loss, else ok. `recent_loss_pct` (windowed avg)
// drives degraded so a single dropped ping in the last round doesn't flip the dot;
// `loss_pct` (last round) still drives the immediate `down`. (Overview is authoritative.)
check('targetStatus maps a DTO to a dot severity', () => {
  assert.equal(D.targetStatus({ error: 'timeout', loss_pct: 0 }), 'down');
  assert.equal(D.targetStatus({ loss_pct: 100 }), 'down');
  assert.equal(D.targetStatus({ loss_pct: 50 }), 'down');
  assert.equal(D.targetStatus(null), 'ok');
  // A configured target with no stored round for this vantage is neutral, not a false green (P1-3).
  assert.equal(D.targetStatus({ no_data: true }), 'nodata');
  assert.equal(D.targetStatus({ no_data: true, loss_pct: 0 }), 'nodata');
});
check('targetStatus: degraded reflects SUSTAINED loss, not one dropped ping', () => {
  // The reported bug: last round dropped 1/20 pings (5%) but recent windowed loss is ~0.
  // The dot must stay green, not flip orange on a single transient ping.
  assert.equal(D.targetStatus({ loss_pct: 5, recent_loss_pct: 0.1 }), 'ok');
  // Sustained recent loss trips degraded.
  assert.equal(D.targetStatus({ loss_pct: 0, recent_loss_pct: 2 }), 'degraded');
  assert.equal(D.targetStatus({ loss_pct: 0, recent_loss_pct: 0.6 }), 'degraded');
  assert.equal(D.targetStatus({ loss_pct: 5, recent_loss_pct: 0.5 }), 'ok'); // boundary: 0.5 is not > 0.5
  // A real outage is still immediate (last round), regardless of the smoothing window.
  assert.equal(D.targetStatus({ loss_pct: 100, recent_loss_pct: 100 }), 'down');
  assert.equal(D.targetStatus({ error: 'x', recent_loss_pct: 0 }), 'down');
  // No windowed figure (e.g. in-memory store) falls back to the last round.
  assert.equal(D.targetStatus({ loss_pct: 5 }), 'degraded');
  assert.equal(D.targetStatus({ loss_pct: 0 }), 'ok');
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
check('adminSessionState never treats an unknown/error response as logged out', () => {
  assert.equal(D.adminSessionState(204), 'in');
  assert.equal(D.adminSessionState(401), 'out');
  assert.equal(D.adminSessionState(404), 'disabled');
  assert.equal(D.adminSessionState(0), 'unknown');
  assert.equal(D.adminSessionState(500), 'unknown');
});
check('admin state controller ignores reordered probes and requires a confirmed logout', () => {
  const painted = [];
  const ctl = D.createAdminStateController((state) => painted.push(state));
  const beforeLogin = ctl.beginProbe();
  const afterLogin = ctl.beginProbe();
  assert.equal(ctl.resolveProbe(afterLogin, 204), true);
  assert.equal(ctl.resolveProbe(beforeLogin, 401), false, 'late pre-login 401 must be ignored');
  assert.deepEqual(painted, ['in']);

  const beforeLogout = ctl.beginProbe();
  assert.equal(ctl.confirmLogout(503), false, 'failed logout must not claim signed out');
  assert.equal(ctl.resolveProbe(beforeLogout, 500), false, 'unknown probe must preserve state');
  assert.deepEqual(painted, ['in']);
  assert.equal(ctl.confirmLogout(204), true);
  assert.equal(ctl.resolveProbe(beforeLogout, 204), false, 'pre-logout probe must be invalidated');
  assert.deepEqual(painted, ['in', 'out']);
});
check('a deferred admin-tab status probe loses ownership after navigation', () => {
  assert.equal(D.statusProbeOwnsView('config', 'config'), true);
  assert.equal(D.statusProbeOwnsView('config', 'graphs'), false);
  assert.equal(D.statusProbeOwnsView('vantages', 'overview'), false);
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
check('buildTargetNode: alerts drops empties', () => {
  const n = D.buildTargetNode({ probe: 'HTTP', host: 'h', params: {}, vantages: [], alerts: ['down', ''] });
  assert.deepEqual(n.alerts, ['down']);
});
// Parked Task-1 review finding: addTarget/editTarget used `children[name] = node`, which
// for name === '__proto__' hits Object.prototype's accessor instead of creating an own
// property — the target silently vanished from Object.keys/listTargets/JSON output and
// the hasOwnProperty dup check was bypassed on a second add. Fixed via defineProperty.
check('addTarget: "__proto__" is stored as a real own property, not lost', () => {
  const out = D.addTarget({ targets: { children: {} } }, '__proto__', { probe: 'HTTP', host: 'x' });
  const rows = D.listTargets(out);
  assert.ok(rows.some((r) => r.name === '__proto__'), '__proto__ target should appear in listTargets, not vanish');
  assert.throws(() => D.addTarget(out, '__proto__', { probe: 'HTTP', host: 'y' }), /already exists/);
});

check('labelHTML: title falls back to name, IP appended and escaped', () => {
  assert.equal(D.labelHTML('cf', 'Cloudflare DNS', '1.1.1.1'), 'Cloudflare DNS <span class="tgtip">(1.1.1.1)</span>');
  assert.equal(D.labelHTML('cf', '', '1.1.1.1'), 'cf <span class="tgtip">(1.1.1.1)</span>'); // no title -> name
  assert.equal(D.labelHTML('cf', 'Cloudflare DNS', ''), 'Cloudflare DNS');                   // no IP -> label only
  assert.equal(D.labelHTML('cf', undefined, undefined), 'cf');                                // both absent
  assert.equal(D.labelHTML('n', '<b>', '1&2'), '&lt;b&gt; <span class="tgtip">(1&amp;2)</span>'); // escapes both
});

check('collectingNote: band panels name the history they are accumulating; raw stays generic', () => {
  const daily = D.collectingNote('band', '1d');
  assert.match(daily, /daily band/);
  assert.match(daily, /2 days/);           // tells a fresh deploy it fills in, not that it is broken
  const hourly = D.collectingNote('band', '1h');
  assert.match(hourly, /hourly band/);
  assert.match(hourly, /2 hours/);
  assert.equal(D.collectingNote('raw'), 'collecting…');
  assert.equal(D.collectingNote('raw', '1h'), 'collecting…'); // res is irrelevant for raw panels
});

// --- Vantage agent artifacts: the two files the reveal modal offers (agent.yaml + compose) ---
check('agentYaml: embeds name/hub/key + spool_dir, YAML double-quoted', () => {
  const y = D.agentYaml('nyc', 'smk_abc123', 'https://hub.example');
  assert.match(y, /^# smoke-agent config for vantage "nyc"$/m);
  assert.match(y, /^hub: "https:\/\/hub\.example"/m);
  assert.match(y, /^vantage: "nyc"$/m);
  assert.match(y, /^key: "smk_abc123"$/m);
  assert.match(y, /^spool_dir: \/var\/lib\/smoke-agent\/spool$/m); // pairs with the compose volume
});
check('agentYaml: quotes are escaped so a hostile-ish name stays well-formed YAML', () => {
  // ValidName blocks this at mint time, but the builder must not emit broken YAML regardless.
  const y = D.agentYaml('a"b', 'k', 'http://h');
  assert.match(y, /vantage: "a\\"b"/);
});
check('agentCompose: runnable compose that mounts agent.yaml and carries no secret', () => {
  const c = D.agentCompose();
  assert.match(c, /image: ghcr\.io\/seitzbg\/heliograph:latest/);
  assert.match(c, /entrypoint: \["smoke-agent"\]/);
  assert.match(c, /command: \["-config", "\/etc\/heliograph\/agent\.yaml"\]/);
  assert.match(c, /\.\/agent\.yaml:\/etc\/heliograph\/agent\.yaml:ro/);   // the mount ties the two files together
  assert.match(c, /agent-spool:\/var\/lib\/smoke-agent\/spool/);
  assert.match(c, /cap_add: \[NET_RAW\]/);
  assert.match(c, /net\.ipv4\.ping_group_range: "0 10001"/);              // native Ping probe needs this
  assert.match(c, /restart: unless-stopped/);
  assert.ok(!/smk_/.test(c), 'compose must never contain key material — the key lives only in agent.yaml');
});

check('buildTree preserves input (server) order of siblings, does not re-sort A–Z', () => {
  // Server sends weight order: "zeta" before "alpha"; a folder before a later leaf.
  const nodes = D.buildTree(['zeta', 'alpha', 'Cloudflare/DNS', 'Cloudflare/ICMP', 'aaa']);
  assert.deepEqual(nodes.map((n) => n.name), ['zeta', 'alpha', 'Cloudflare', 'aaa']);
  const cf = nodes.find((n) => n.name === 'Cloudflare');
  assert.deepEqual(cf.children.map((c) => c.name), ['DNS', 'ICMP']); // sibling order preserved
});

const cfgDoc = () => ({ targets: { children: {
  Web:  { weight: 1, children: { a: { probe:'HTTP', host:'a' }, b: { probe:'HTTP', host:'b', weight:-1 } } },
  DNS:  { probe:'DNS', host:'d', weight:-5 },
} } });
check('cfgTree: nests, sorts siblings by (weight,name), carries path', () => {
  const t = D.cfgTree(cfgDoc());
  assert.deepEqual(t.map((n) => n.name), ['DNS', 'Web']);         // DNS weight -5 first, then Web weight 1
  assert.equal(t[0].isFolder, false); assert.equal(t[1].isFolder, true);
  const web = t[1];
  assert.deepEqual(web.children.map((c) => c.name), ['b', 'a']);  // b weight -1 before a (0)
  assert.equal(web.children[0].path, 'Web/b');
});
check('cfgTree: an explicit empty children map is a folder; mutations prune invalid empty groups', () => {
  const explicit = D.cfgTree({ targets: { children: { Web: { children: {} } } } })[0];
  assert.equal(explicit.isFolder, true, 'defensive rendering preserves explicit folder identity');
  const emptied = D.removeNodeAtPath({ targets: { children: {
    Web: { children: { a: { probe: 'HTTP', host: 'a' } } },
  } } }, 'Web/a');
  assert.equal(D.cfgTree(emptied).find((n) => n.name === 'Web'), undefined,
    'a hostless group is removed with its last child so the server never rejects the save');
  // A leaf (host, no children map) is never a folder.
  const leaf = D.cfgTree({ targets: { children: { x: { probe: 'HTTP', host: 'x' } } } })[0];
  assert.equal(leaf.isFolder, false);
});
check('reweightSiblings: sequential indices by given order', () => {
  assert.deepEqual(D.reweightSiblings(['x', 'y', 'z']), { x: 0, y: 1, z: 2 });
});
check('reorderSiblings: writes weights to the named parent group only', () => {
  const out = D.reorderSiblings(cfgDoc(), 'Web', ['a', 'b']);      // flip Web's children to a,b
  const web = out.targets.children.Web.children;
  assert.equal(web.a.weight, 0); assert.equal(web.b.weight, 1);
  assert.equal(out.targets.children.DNS.weight, -5);              // untouched group unchanged
});
check('reorderSiblings: top level via empty parentPath', () => {
  const out = D.reorderSiblings(cfgDoc(), '', ['Web', 'DNS']);
  assert.equal(out.targets.children.Web.weight, 0);
  assert.equal(out.targets.children.DNS.weight, 1);
});
check('editNodeAtPath: updates form fields while preserving unexposed fields, weight and children', () => {
  const doc = cfgDoc();
  Object.assign(doc.targets.children.Web.children.b, {
    title: 'Shown title', ip: '192.0.2.4', pings: 17, step: '45s', alertee: ['ops'],
    alerts: ['old'], params: { port: '80' }, custom_future_field: 'keep-me',
    children: { kid: { host: 'k' } },
  });
  const out = D.editNodeAtPath(doc, 'Web/b', { probe:'FPing', host:'b2' });
  const b = out.targets.children.Web.children.b;
  assert.equal(b.probe, 'FPing'); assert.equal(b.host, 'b2');
  assert.equal(b.weight, -1); assert.equal(b.title, 'Shown title'); assert.equal(b.ip, '192.0.2.4');
  assert.equal(b.pings, 17); assert.equal(b.step, '45s'); assert.deepEqual(b.alertee, ['ops']);
  assert.equal(b.custom_future_field, 'keep-me'); assert.ok(b.children.kid);
  assert.equal(b.alerts, undefined); assert.equal(b.params, undefined); // cleared form-owned fields
  const renamed = D.renameNodeAtPath(out, 'Web/b', 'renamed').targets.children.Web.children.renamed;
  assert.equal(renamed.title, 'Shown title'); assert.equal(renamed.ip, '192.0.2.4');
  assert.equal(renamed.pings, 17); assert.equal(renamed.step, '45s');
  assert.deepEqual(renamed.alertee, ['ops']); assert.equal(renamed.custom_future_field, 'keep-me');
  assert.ok(renamed.children.kid, 'the actual edit-then-rename workflow preserves the subtree');
});
check('removeNodeAtPath: deletes a nested node, leaves siblings', () => {
  const out = D.removeNodeAtPath(cfgDoc(), 'Web/a');
  assert.ok(!('a' in out.targets.children.Web.children));
  assert.ok('b' in out.targets.children.Web.children);
});
check('renameNodeAtPath: rekeys in place, preserves payload/weights/position, rejects collision', () => {
  const doc = { targets: { children: {
    Web: { weight: 0, children: { a: { host: 'a', weight: 0 }, b: { host: 'b', weight: 1 } } },
    DNS: { host: 'd', weight: 1 },
  } } };
  const out = D.renameNodeAtPath(doc, 'Web/a', 'alpha');
  const web = D.cfgTree(out).find((n) => n.name === 'Web');
  assert.deepEqual(web.children.map((c) => c.name), ['alpha', 'b']); // renamed; weight-0 keeps it first
  assert.equal(web.children[0].node.host, 'a');                      // value preserved
  assert.deepEqual(web.children.map((c) => c.weight), [0, 1]);      // no unnecessary reweight
  assert.throws(() => D.renameNodeAtPath(doc, 'Web/a', 'b'), /already exists/); // sibling collision
  const same = D.renameNodeAtPath(doc, 'Web/a', 'a'); // unchanged name = no-op
  assert.deepEqual(D.cfgTree(same).find((n) => n.name === 'Web').children.map((c) => c.name), ['a', 'b']);
  assert.deepEqual(D.cfgTree(D.renameNodeAtPath(doc, 'Web/zzz', 'q')).find((n) => n.name === 'Web').children.map((c) => c.name), ['a', 'b']); // stale path = no-op
});
check('renameNodeAtPath: tied weights keep the old visual position after an alphabetical crossing', () => {
  const doc = { targets: { children: {
    a: { host: 'a', weight: 0 }, b: { host: 'b', weight: 0 }, c: { host: 'c', weight: 0 },
  } } };
  const out = D.renameNodeAtPath(doc, 'b', 'z');
  assert.deepEqual(D.cfgTree(out).map((n) => n.name), ['a', 'z', 'c']);
  assert.deepEqual(D.cfgTree(out).map((n) => n.weight), [0, 1, 2]);
});
check('buildGroupNode: children map from rows + optional inherited vantages/alerts', () => {
  const n = D.buildGroupNode({
    vantages: ['local', ' nyc '], alerts: [' loss '],
    children: [
      { name: 'ICMP (FPing)', probe: 'FPing', host: '8.8.8.8' },
      { name: 'DNS query', probe: 'DNS', host: '8.8.8.8' },
      { name: '', host: '' }, // fully-blank row is skipped
    ],
  });
  assert.deepEqual(Object.keys(n.children), ['ICMP (FPing)', 'DNS query']);
  assert.equal(n.children['ICMP (FPing)'].probe, 'FPing');
  assert.equal(n.children['ICMP (FPing)'].host, '8.8.8.8');
  assert.deepEqual(n.vantages, ['local', 'nyc']);
  assert.deepEqual(n.alerts, ['loss']);
});
check('buildGroupNode: rejects empty group, hostless child, dup name, and "/"', () => {
  assert.throws(() => D.buildGroupNode({ children: [] }), /at least one target/);
  assert.throws(() => D.buildGroupNode({ children: [{ name: 'x', probe: 'Ping', host: '' }] }), /needs a host/);
  assert.throws(() => D.buildGroupNode({ children: [{ name: 'a', probe: 'Ping', host: 'h' }, { name: 'a', probe: 'DNS', host: 'h' }] }), /duplicate/);
  assert.throws(() => D.buildGroupNode({ children: [{ name: 'a/b', probe: 'Ping', host: 'h' }] }), /can't contain/);
});

// --- Config-tree follow-ups (#37): add-into-folder, cross-folder move, keyboard nav/reorder ---
// addNodeAtPath: the path-aware add. Top-level '' is exactly the old addTarget; a nested parent
// path inserts INTO that folder; adding into a leaf turns it into a folder-with-target.
check('addNodeAtPath: inserts into a nested folder, input untouched', () => {
  const doc = cfgDoc();
  const out = D.addNodeAtPath(doc, 'Web', 'c', { probe: 'HTTP', host: 'c' });
  assert.deepEqual(Object.keys(out.targets.children.Web.children).sort(), ['a', 'b', 'c']);
  assert.deepEqual(Object.keys(doc.targets.children.Web.children).sort(), ['a', 'b']); // input untouched
});
check('addNodeAtPath: top level equals addTarget; rejects dup in the same group', () => {
  const doc = { targets: { children: { a: { probe: 'HTTP', host: 'a' } } } };
  const out = D.addNodeAtPath(doc, '', 'b', { probe: 'HTTP', host: 'b' });
  assert.deepEqual(Object.keys(out.targets.children).sort(), ['a', 'b']);
  assert.throws(() => D.addNodeAtPath(out, '', 'a', {}), /already exists/);
  assert.throws(() => D.addNodeAtPath(out, 'nope', 'x', {}), /no folder/); // parent must exist
});
check('addNodeAtPath: a same-named node in a DIFFERENT folder is allowed (path-scoped dup check)', () => {
  const doc = cfgDoc(); // Web has {a,b}; there is a top-level DNS but no top-level "a"
  const out = D.addNodeAtPath(doc, '', 'a', { probe: 'HTTP', host: 'topA' }); // top-level "a" is distinct from Web/a
  assert.ok('a' in out.targets.children);
  assert.ok('a' in out.targets.children.Web.children);
});
check('addNodeAtPath: adding into a leaf turns it into a folder-with-target', () => {
  const doc = { targets: { children: { host: { probe: 'HTTP', host: 'h' } } } };
  const out = D.addNodeAtPath(doc, 'host', 'sub', { probe: 'HTTP', host: 's' });
  assert.equal(out.targets.children.host.probe, 'HTTP');              // still a target
  assert.deepEqual(Object.keys(out.targets.children.host.children), ['sub']); // and now a folder
  const t = D.cfgTree(out);
  assert.equal(t[0].isFolder, true);
});
check('addNodeAtPath: "__proto__" is a real own property, not lost to the prototype', () => {
  const out = D.addNodeAtPath({ targets: { children: {} } }, '', '__proto__', { probe: 'HTTP', host: 'x' });
  assert.ok(D.listTargets(out).some((r) => r.name === '__proto__'));
});

// moveNode: relocate a subtree between parents, reweighting BOTH sibling sets so the
// (weight,name) sort reproduces the requested order; also the same-parent reorder path.
const mvDoc = () => ({ targets: { children: {
  Web: { weight: 0, children: {
    a: { probe: 'HTTP', host: 'a', weight: 0 },
    b: { probe: 'HTTP', host: 'b', weight: 1 },
    c: { probe: 'HTTP', host: 'c', weight: 2 },
  } },
  DNS: { weight: 1, children: {
    x: { probe: 'DNS', host: 'x', weight: 0 },
  } },
} } });
check('moveNode: cross-folder move relocates the subtree and reweights both groups', () => {
  const doc = mvDoc();
  const out = D.moveNode(doc, 'Web/b', 'DNS', 0); // Web/b -> DNS at index 0
  const web = out.targets.children.Web.children;
  const dns = out.targets.children.DNS.children;
  assert.deepEqual(Object.keys(web).sort(), ['a', 'c']);            // b left Web
  assert.ok('b' in dns);                                            // b now in DNS
  // Source group re-sequenced 0..n-1 in its surviving (weight,name) order.
  assert.deepEqual(D.cfgTree(out).find((n) => n.name === 'Web').children.map((n) => n.name), ['a', 'c']);
  assert.equal(web.a.weight, 0); assert.equal(web.c.weight, 1);
  // Destination group ordered with b spliced at index 0, resequenced.
  assert.deepEqual(D.cfgTree(out).find((n) => n.name === 'DNS').children.map((n) => n.name), ['b', 'x']);
  assert.equal(dns.b.weight, 0); assert.equal(dns.x.weight, 1);
  assert.deepEqual(Object.keys(doc.targets.children.Web.children).sort(), ['a', 'b', 'c']); // input untouched
});
check('moveNode: moving the last child out prunes an invalid hostless source group', () => {
  const doc = { targets: { children: {
    EmptyMe: { children: { only: { probe: 'HTTP', host: 'x' } } },
    Dest: { children: { keep: { probe: 'HTTP', host: 'y' } } },
  } } };
  const out = D.moveNode(doc, 'EmptyMe/only', 'Dest');
  assert.equal(out.targets.children.EmptyMe, undefined);
  assert.ok(out.targets.children.Dest.children.only);
});
check('moveNode: append (index omitted / out of range) lands last in the destination', () => {
  const out = D.moveNode(mvDoc(), 'Web/a', 'DNS'); // no index -> append
  assert.deepEqual(D.cfgTree(out).find((n) => n.name === 'DNS').children.map((n) => n.name), ['x', 'a']);
});
check('moveNode: move up to the top level', () => {
  const out = D.moveNode(mvDoc(), 'Web/c', '', 0); // Web/c -> top level, first
  assert.deepEqual(D.cfgTree(out).map((n) => n.name), ['c', 'Web', 'DNS']);
  assert.deepEqual(D.cfgTree(out).find((n) => n.name === 'Web').children.map((n) => n.name), ['a', 'b']);
});
check('moveNode: same-parent move is a pure reorder', () => {
  const out = D.moveNode(mvDoc(), 'Web/c', 'Web', 0); // c to the front of Web
  assert.deepEqual(D.cfgTree(out).find((n) => n.name === 'Web').children.map((n) => n.name), ['c', 'a', 'b']);
});
check('moveNode: GUARD — cannot move a folder into itself or its own descendant', () => {
  assert.throws(() => D.moveNode(mvDoc(), 'Web', 'Web'), /own subtree/);       // into itself
  assert.throws(() => D.moveNode(mvDoc(), 'Web', 'Web/a'), /own subtree/);     // into a descendant
});
check('moveNode: name collision in the destination throws (does not clobber)', () => {
  const doc = { targets: { children: {
    G1: { children: { dup: { probe: 'HTTP', host: '1' } } },
    G2: { children: { dup: { probe: 'HTTP', host: '2' } } },
  } } };
  assert.throws(() => D.moveNode(doc, 'G1/dup', 'G2', 0), /already exists/);
});
check('moveNode: a stale srcPath is a harmless no-op', () => {
  const out = D.moveNode(mvDoc(), 'Web/zzz', 'DNS', 0);
  assert.deepEqual(D.cfgTree(out).find((n) => n.name === 'DNS').children.map((n) => n.name), ['x']);
});

check('cfgDropDestination: dropping onto a sibling folder moves inside it', () => {
  assert.deepEqual(D.cfgDropDestination('leaf', 'Folder', true), { destParent: 'Folder', kind: 'into' });
  assert.deepEqual(D.cfgDropDestination('G/a', 'G/b', false), { destParent: 'G', kind: 'before' });
  assert.equal(D.cfgDropDestination('Folder', 'Folder/child', true), null); // own descendant
});

// moveInList: the pure Alt+Up / Alt+Down reorder core (clamped, returns a new array).
check('moveInList: shifts by delta, clamps at the ends, no-op returns equal order', () => {
  assert.deepEqual(D.moveInList(['a', 'b', 'c'], 'a', 1), ['b', 'a', 'c']);   // down one
  assert.deepEqual(D.moveInList(['a', 'b', 'c'], 'c', -1), ['a', 'c', 'b']);  // up one
  assert.deepEqual(D.moveInList(['a', 'b', 'c'], 'a', -1), ['a', 'b', 'c']);  // clamp at top
  assert.deepEqual(D.moveInList(['a', 'b', 'c'], 'c', 1), ['a', 'b', 'c']);   // clamp at bottom
  assert.deepEqual(D.moveInList(['a', 'b', 'c'], 'zzz', 1), ['a', 'b', 'c']); // absent -> unchanged
  const src = ['a', 'b']; D.moveInList(src, 'a', 1); assert.deepEqual(src, ['a', 'b']); // input untouched
});

// cfgVisibleRows: flatten a cfgTree into the arrow-navigable order, honouring collapsed folders.
const vrDoc = () => ({ targets: { children: {
  Web: { weight: 0, children: { a: { host: 'a', weight: 0 }, b: { host: 'b', weight: 1 } } },
  DNS: { weight: 1, host: 'd' },
} } });
check('cfgVisibleRows: expanded folder exposes its children; collapsed hides them', () => {
  const tree = D.cfgTree(vrDoc());
  const all = D.cfgVisibleRows(tree, new Set());
  assert.deepEqual(all.map((r) => r.path), ['Web', 'Web/a', 'Web/b', 'DNS']);
  const web = all.find((r) => r.path === 'Web');
  assert.equal(web.isFolder, true); assert.equal(web.expanded, true);
  assert.equal(all.find((r) => r.path === 'Web/a').parentPath, 'Web');
  const collapsed = D.cfgVisibleRows(tree, new Set(['Web']));
  assert.deepEqual(collapsed.map((r) => r.path), ['Web', 'DNS']);   // Web's children hidden
  assert.equal(collapsed.find((r) => r.path === 'Web').expanded, false);
});

// cfgTreeKey: the pure keyboard decision. Rows come from cfgVisibleRows.
const vrRows = (collapsedPaths) => D.cfgVisibleRows(D.cfgTree(vrDoc()), new Set(collapsedPaths || []));
check('cfgTreeKey: Up/Down move focus, clamped at the ends', () => {
  const rows = vrRows();
  assert.deepEqual(D.cfgTreeKey(rows, 'Web', 'ArrowDown', false), { type: 'focus', path: 'Web/a' });
  assert.deepEqual(D.cfgTreeKey(rows, 'Web/a', 'ArrowUp', false), { type: 'focus', path: 'Web' });
  assert.equal(D.cfgTreeKey(rows, 'Web', 'ArrowUp', false), null);      // already at top
  assert.equal(D.cfgTreeKey(rows, 'DNS', 'ArrowDown', false), null);    // already at bottom
  assert.deepEqual(D.cfgTreeKey(rows, 'Web/b', 'Home', false), { type: 'focus', path: 'Web' });
  assert.deepEqual(D.cfgTreeKey(rows, 'Web', 'End', false), { type: 'focus', path: 'DNS' });
});
check('cfgTreeKey: null focus defaults to the first row', () => {
  assert.deepEqual(D.cfgTreeKey(vrRows(), null, 'ArrowDown', false), { type: 'focus', path: 'Web/a' });
});
check('cfgTreeKey: Right expands a collapsed folder, then steps into it', () => {
  assert.deepEqual(D.cfgTreeKey(vrRows(['Web']), 'Web', 'ArrowRight', false), { type: 'expand', path: 'Web' });
  assert.deepEqual(D.cfgTreeKey(vrRows(), 'Web', 'ArrowRight', false), { type: 'focus', path: 'Web/a' }); // expanded -> first child
  assert.equal(D.cfgTreeKey(vrRows(), 'Web/a', 'ArrowRight', false), null); // leaf: nothing
  assert.equal(D.cfgTreeKey(vrRows(), 'DNS', 'ArrowRight', false), null);   // leaf: nothing
});
check('cfgTreeKey: Left collapses an expanded folder, else moves to the parent', () => {
  assert.deepEqual(D.cfgTreeKey(vrRows(), 'Web', 'ArrowLeft', false), { type: 'collapse', path: 'Web' });
  assert.deepEqual(D.cfgTreeKey(vrRows(), 'Web/a', 'ArrowLeft', false), { type: 'focus', path: 'Web' }); // leaf -> parent
  assert.equal(D.cfgTreeKey(vrRows(['Web']), 'Web', 'ArrowLeft', false), null); // collapsed top-level folder, no parent
});
check('cfgTreeKey: Alt+Up / Alt+Down request a sibling reorder of the focused node', () => {
  assert.deepEqual(D.cfgTreeKey(vrRows(), 'Web/b', 'ArrowUp', true), { type: 'reorder', parentPath: 'Web', name: 'b', delta: -1, path: 'Web/b' });
  assert.deepEqual(D.cfgTreeKey(vrRows(), 'Web', 'ArrowDown', true), { type: 'reorder', parentPath: '', name: 'Web', delta: 1, path: 'Web' });
});
check('cfgTreeKey: ignores unrelated keys and empty rows', () => {
  assert.equal(D.cfgTreeKey(vrRows(), 'Web', 'Enter', false), null);
  assert.equal(D.cfgTreeKey([], 'Web', 'ArrowDown', false), null);
});

check('gridTemplateFor: auto (and non-positive) -> auto-fit tracks, floor clamped to 100%', () => {
  assert.equal(D.gridTemplateFor('auto', 360, 18), 'repeat(auto-fit, minmax(min(360px, 100%), 1fr))');
  assert.equal(D.gridTemplateFor(0, 360, 18), 'repeat(auto-fit, minmax(min(360px, 100%), 1fr))');
  assert.equal(D.gridTemplateFor('nonsense', 360, 18), 'repeat(auto-fit, minmax(min(360px, 100%), 1fr))');
});
check('gridTemplateFor: a fixed N caps columns; the floor is clamped to 100% so narrow viewports do not overflow', () => {
  // N tracks + (N-1) gaps; the max() floor makes auto-fill wrap to fewer when they will not fit,
  // and min(...,100%) collapses to a single full-width track below the minimum instead of overflowing.
  assert.equal(D.gridTemplateFor(4, 360, 18), 'repeat(auto-fill, minmax(max(min(360px, 100%), (100% - 54px) / 4), 1fr))');
  assert.equal(D.gridTemplateFor('2', 360, 18), 'repeat(auto-fill, minmax(max(min(360px, 100%), (100% - 18px) / 2), 1fr))');
  assert.equal(D.gridTemplateFor(6, 360, 18), 'repeat(auto-fill, minmax(max(min(360px, 100%), (100% - 90px) / 6), 1fr))');
});

check('maxColumnsFor: how many min-width columns fit at a given width', () => {
  assert.equal(D.maxColumnsFor(0, 360, 18), 0);
  assert.equal(D.maxColumnsFor(-100, 360, 18), 0);
  assert.equal(D.maxColumnsFor(359, 360, 18), 1);  // narrower than one min -> still one (full-width)
  assert.equal(D.maxColumnsFor(360, 360, 18), 1);
  assert.equal(D.maxColumnsFor(756, 360, 18), 2);  // 2*360 + 1*18 = 738 <= 756 < 3-col
  assert.equal(D.maxColumnsFor(1115, 360, 18), 2); // just under the 3-col threshold
  assert.equal(D.maxColumnsFor(1116, 360, 18), 3); // 3*360 + 2*18
  assert.equal(D.maxColumnsFor(2250, 360, 18), 6); // 6*360 + 5*18
});

check('rangeLabels: four labels; clock time for short spans, calendar dates for long ones', () => {
  const HR = 3600 * 1000, DAY = 86400 * 1000, t1 = 1_700_000_000_000;
  const short = D.rangeLabels(t1 - 3 * HR, t1); // 3h span
  assert.equal(short.length, 4);
  assert.ok(short.every((l) => l.includes(':')), 'short-span labels should be clock times: ' + short.join(', '));
  const long = D.rangeLabels(t1 - 10 * DAY, t1); // 10d span
  assert.equal(long.length, 4);
  assert.ok(long.every((l) => !l.includes(':') && /[A-Za-z]/.test(l)), 'long-span labels should be calendar dates: ' + long.join(', '));
  // The endpoints are the exact bounds; the middle two are the 1/3 and 2/3 marks.
  assert.notEqual(short[0], short[3]);
});

if (failed) { console.error(`\n${failed} test(s) failed`); process.exit(1); }
console.log('\nall dashboard tests passed');

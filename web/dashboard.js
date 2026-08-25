// dashboard.js — the Heliograph SPA. Reads the Go collector's JSON API and
// renders it with the shared Smoke canvas renderer (smoke.js).
//
// Views (hash-routed, Back/deep-link friendly):
//   #overview                 worst-targets + availability leaderboards (landing)
//   #graphs                   per-target smoke grid (recent 3h thumbnails)
//   #target=<name>            drill-down: all four ranges stacked (SmokePing-style)
//   #target=<name>&range=<r>  one range enlarged
//
// Pure helpers (routing/range mapping/slicing) are exposed on window.Dash for unit
// tests; everything DOM runs from init(), which only fires on DOMContentLoaded.
(function () {
  'use strict';

  // Range tiers. raw -> /api/series?window=; band -> /api/rollup?res= (client-sliced
  // to the range window, since /api/rollup returns the full history for the target).
  // windowMs sets the fixed wall-clock X domain [now-windowMs, now]: the axis means
  // exactly what its label says, short data floats at its true position, and outages
  // render as blank spans (#3). Kept in sync with the raw `window` / band `days`.
  const H = 3600 * 1000, D = 86400 * 1000;
  const RANGES = {
    '3h':  { mode: 'raw',  window: '3h',  windowMs: 3 * H,   label: 'Last 3 hours',  desc: 'per-round smoke', xl: ['-3h', '-2h', '-1h', 'now'] },
    '30h': { mode: 'raw',  window: '30h', windowMs: 30 * H,  label: 'Last 30 hours', desc: 'per-round smoke', xl: ['-30h', '-20h', '-10h', 'now'] },
    '10d': { mode: 'band', res: '1h', days: 10,  windowMs: 10 * D,  label: 'Last 10 days',  desc: 'hourly band', xl: ['-10d', '', '', 'now'] },
    '400d':{ mode: 'band', res: '1d', days: 400, windowMs: 400 * D, label: 'Last 400 days', desc: 'daily band',  xl: ['-400d', '', '', 'now'] },
  };
  const RANGE_ORDER = ['3h', '30h', '10d', '400d'];

  function parseRoute(hash) {
    const h = (hash || '').replace(/^#/, '');
    if (h.startsWith('target=')) {
      const p = new URLSearchParams(h);
      const name = p.get('target') || ''; // URLSearchParams already percent-decodes; a second decode corrupts names with % (CODE_REVIEW #10)
      const range = p.get('range');
      return (range && RANGES[range]) ? { view: 'zoom', name, range } : { view: 'stack', name };
    }
    if (h === 'graphs' || h.startsWith('graphs&')) {
      // Optional subtree scope for the config-tree menu: #graphs&path=<folder>. Decoded
      // once by URLSearchParams (a second decode would corrupt names with %, like #10).
      const path = new URLSearchParams(h).get('path') || '';
      return path ? { view: 'graphs', path } : { view: 'graphs' };
    }
    if (h === 'vantages') return { view: 'vantages' };
    if (h === 'config') return { view: 'config' };
    return { view: 'overview' };
  }

  // Fold the rounds newer than the watermark (incoming) into a panel's cached series
  // (prev), drop buckets older than cutoffMs (the window floor), keep oldest->newest.
  // Powers the incremental Graphs grid (#2): each refresh merges only the new rounds
  // instead of re-fetching the whole 3h window. Dedupes the boundary round defensively
  // (the server already returns strictly-newer rounds).
  function mergeSeries(prev, incoming, cutoffMs) {
    const prevB = (prev && prev.buckets) || [];
    const lastT = prevB.length ? prevB[prevB.length - 1].t : -Infinity;
    const fresh = ((incoming && incoming.buckets) || []).filter((b) => b.t > lastT);
    let buckets = prevB.concat(fresh);
    if (cutoffMs != null) buckets = buckets.filter((b) => !(b.t < cutoffMs)); // keep b.t >= cutoff (NaN t kept)
    let N = 0;
    for (const b of buckets) N = Math.max(N, b.pings || (b.centered ? b.centered.length : 0));
    return { buckets, N };
  }

  // gridSince is the incremental watermark for the bulk Graphs grid (#1): the OLDEST
  // round timestamp among panels that currently hold data. Using the oldest frontier
  // (not the global newest) means the shared `since` never advances past the
  // slowest-updating target — a single global max would skip that target's late/
  // out-of-order rounds permanently. Panels with no data yet are ignored (they backfill
  // separately); null when nothing holds data, so the first tick fetches the whole window.
  function gridSince(panels, pick) {
    pick = pick || ((p) => p && p.series); // per-vantage grids pass a picker for p.seriesByV[v]
    let min = null;
    for (const p of panels) {
      const s = pick(p);
      const bs = s && s.buckets;
      if (!bs || !bs.length) continue;
      const last = bs[bs.length - 1].t;
      if (!Number.isFinite(last)) continue;
      if (min === null || last < min) min = last;
    }
    return min;
  }

  // gridTemplateFor builds the Graphs grid's `grid-template-columns`. `cols` === 'auto'
  // (the default) fits as many `min`-px tracks as the container allows (auto-fit — today's
  // behavior). A fixed integer N caps the layout at N columns while never letting a track
  // fall below `min`: the max() floor means when N tracks won't fit at `min`, auto-fill
  // wraps to fewer instead of shrinking the graphs. `min`/`gap` are CSS px numbers.
  // The floor is `min(<min>px, 100%)`, not a bare `<min>px`, so a viewport narrower than the
  // minimum collapses to a single full-width track instead of overflowing horizontally.
  function gridTemplateFor(cols, min, gap) {
    // `min` is a CSS length (e.g. '22.5rem') so the floor scales with the user's font size, not a
    // fixed pixel count; clamped to 100% so a viewport narrower than the minimum collapses to one
    // full-width track instead of overflowing. `gap` stays px (layout spacing, not text-relative).
    const floor = 'min(' + min + ', 100%)';
    const n = Math.floor(Number(cols));
    if (!(n > 0)) return 'repeat(auto-fit, minmax(' + floor + ', 1fr))';
    return 'repeat(auto-fill, minmax(max(' + floor + ', (100% - ' + (n - 1) * gap + 'px) / ' + n + '), 1fr))';
  }

  // maxColumnsFor returns how many `min`-px graph columns (with `gap` between them) actually fit
  // in `width` px — n columns need n*min + (n-1)*gap <= width. The Columns picker uses it to hide
  // counts that can't fit (clicking them would just wrap to fewer) and to hide itself entirely
  // when even 3 won't fit. Always at least 1 for any positive width.
  function maxColumnsFor(width, min, gap) {
    if (!(width > 0)) return 0;
    return Math.max(1, Math.floor((width + gap) / (min + gap)));
  }

  // rangeLabels builds four absolute-time x-axis labels spanning [t0, t1]: clock time (HH:MM) for
  // spans under ~1.5 days, calendar day ("Aug 6") for longer ones. Used for a custom drag-zoom
  // range and when the Graphs "absolute time" toggle is on; the relative alternative is each
  // range's static `xl` (e.g. -3h / -2h / -1h / now).
  function rangeLabels(t0, t1) {
    const span = t1 - t0;
    const fmt = (t) => { const d = new Date(t); return span < 36 * H ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : d.toLocaleDateString([], { month: 'short', day: 'numeric' }); };
    return [fmt(t0), fmt(t0 + span / 3), fmt(t0 + 2 * span / 3), fmt(t1)];
  }

  // fetchJSON GETs a JSON endpoint and REJECTS a non-2xx response instead of decoding
  // its body (#2). The API returns JSON error bodies with HTTP 503 when storage is
  // unavailable; decoding those as ordinary data turned a transient failure into an
  // empty target/SLA/chart list, blanking the dashboard. Rejecting lets each caller
  // keep its last-known state and show a degraded indicator instead.
  async function fetchJSON(url) {
    const r = await fetch(url, { cache: 'no-store' });
    if (!r.ok) throw new Error('HTTP ' + r.status + ' from ' + url);
    return r.json();
  }

  // zoomResolution maps a drag-selected span (ms) to the fetch tier that best resolves
  // it: raw per-round for short spans, hourly then daily bands for longer ones — so
  // zooming a long band view down into a shorter span refetches finer data rather than
  // stretching the buckets it already has.
  function zoomResolution(spanMs) {
    if (spanMs <= 30 * H) return { mode: 'raw' };
    if (spanMs <= 10 * D) return { mode: 'band', res: '1h' };
    return { mode: 'band', res: '1d' };
  }

  // pixelToTime maps an x offset within a graph canvas (CSS px) to a wall-clock time,
  // given the canvas width and the rendered domain [t0,t1] — the inverse of Smoke.render's
  // X mapping, clamped to the plot area so a drag past an edge saturates at t0/t1.
  function pixelToTime(xCss, clientWidth, t0, t1) {
    const { mL, mR } = Smoke.MARGINS;
    const pw = clientWidth - mL - mR;
    let f = pw > 0 ? (xCss - mL) / pw : 0;
    f = f < 0 ? 0 : f > 1 ? 1 : f;
    return t0 + f * (t1 - t0);
  }

  // sharedYMax is the Graphs grid's unison Y-axis: the largest per-panel robustMax across
  // every panel that holds data, so a 5ms target and a 500ms target are drawn to the same
  // scale and are visually comparable. undefined when nothing has data yet — callers then
  // fall back to per-panel auto-scaling.
  function sharedYMax(seriesList) {
    let m = 0;
    for (const s of seriesList) {
      if (s && s.buckets && s.buckets.length) m = Math.max(m, Smoke.robustMax(s));
    }
    return m > 0 ? m : undefined;
  }

  // buildTree turns the flat target-name list from /api/targets into the config-tree menu
  // (left nav). Each name is a '/'-separated path (the config hierarchy); we nest a node
  // per segment, and set `target` on the node whose exact path is a monitored target — so
  // a node can be BOTH a target and a folder (a host with children, as SmokePing allows).
  // Siblings preserve first-encounter order (the server's config/weight order — see
  // /api/targets), so the menu mirrors the config author's ordering instead of A–Z.
  // Returns the top-level nodes:
  //   node = { name, path, target: <full name>|null, children: node[] }
  function buildTree(names) {
    const root = { children: [], _m: new Map() };
    for (const full of names || []) {
      if (!full) continue;
      const parts = String(full).split('/');
      let cur = root, path = '';
      for (const seg of parts) {
        path = path ? path + '/' + seg : seg;
        let next = cur._m.get(seg);
        if (!next) { next = { name: seg, path, target: null, children: [], _m: new Map() }; cur._m.set(seg, next); cur.children.push(next); }
        cur = next;
      }
      cur.target = full;
    }
    (function finish(n) { delete n._m; n.children.forEach(finish); })(root);
    return root.children;
  }

  // underPath reports whether target `name` falls within a menu subtree `path`: the whole
  // set when scope is empty, otherwise an exact match or a '/'-boundary descendant (so a
  // scope of 'a' contains 'a/b' but NOT 'ab').
  function underPath(name, path) {
    if (!path) return true;
    return name === path || name.startsWith(path + '/');
  }

  // targetStatus reduces a /api/targets DTO to a menu dot severity: down on a probe error
  // or heavy loss, degraded on light loss, else ok. It's a glanceable hint in the nav —
  // the Overview tab remains the authoritative availability/SLA view.
  function targetStatus(t) {
    if (!t) return 'ok';
    // A configured target with no stored round for this vantage (e.g. a remote-only
    // target seen from the local view) gets a neutral dot, not a false green (P1-3).
    if (t.no_data) return 'nodata';
    // Immediate outage: a hard error or a heavy-loss last round.
    if (t.error || (t.loss_pct != null && t.loss_pct >= 50)) return 'down';
    // Degraded on SUSTAINED loss: prefer the windowed recent average (recent_loss_pct) so a
    // single dropped ping in the last round doesn't flip the dot orange. Fall back to the last
    // round only when the store supplied no windowed figure.
    const recent = t.recent_loss_pct != null ? t.recent_loss_pct : t.loss_pct;
    if (recent != null && recent > 0.5) return 'degraded';
    return 'ok';
  }

  // pickSeries decides what a detail graph renders and caches for one range: a fresh non-null
  // series is rendered and cached (except the 'unsupported' sentinel, which is not real data);
  // a null fresh — a transient fetch failure (a non-2xx like a brief 503) — falls back to the
  // cached last-good so the graph keeps its data instead of blanking to "collecting…" (#5).
  function pickSeries(fresh, cached) {
    const prev = cached || null;
    if (fresh == null) return { series: prev, cache: prev, failed: true };
    return { series: fresh, cache: fresh.unsupported ? prev : fresh, failed: false };
  }

  // vantageList returns a target's vantage set from an /api/targets DTO: the array if
  // present and non-empty, else the implicit single-vantage default. Sliced defensively
  // so callers can't mutate the DTO's array through the returned list.
  function vantageList(t) {
    return (t && Array.isArray(t.vantages) && t.vantages.length) ? t.vantages.slice() : ['local'];
  }

  // orderVantages is the stable render order for a target's vantage overlay: 'local'
  // first (if present), then the rest sorted ascending, deduped. Stable given a stable
  // input set, so vantageColorVar's palette assignment stays consistent across renders.
  function orderVantages(list) {
    const uniq = [...new Set(list || [])];
    const local = uniq.filter((v) => v === 'local');
    const rest = uniq.filter((v) => v !== 'local').sort();
    return local.concat(rest);
  }

  // defaultFocus picks the vantage a detail view opens focused on: 'local' when present
  // in the ordered list, else the ordered list's first entry ('local' if the list is empty).
  function defaultFocus(list) {
    const o = orderVantages(list);
    return o.includes('local') ? 'local' : (o[0] || 'local');
  }

  // keepFocus preserves the currently-focused vantage across a re-render when it is still one
  // of the target's vantages, else falls back to defaultFocus. The detail views auto-refresh
  // (every 30s); without this a refresh would reset a user's chip selection back to the default.
  function keepFocus(current, list) {
    return current && list.includes(current) ? current : defaultFocus(list);
  }

  // VPAL is the fixed overlay color palette (CSS var names defined in dashboard.css) cycled
  // across non-local vantages by their position in the ordered list.
  const VPAL = ['--v-a', '--v-b', '--v-c', '--v-d'];
  // MAX_GRID_VANTAGES caps how many vantages the Graphs grid overlays at once. It equals the number
  // of distinct overlay colors available (local uses the neutral median color; the rest cycle VPAL),
  // so a 5th selection would reuse a color and make two overlays indistinguishable. Vantages beyond
  // this may still EXIST and be viewed — you just can't overlay more than this many simultaneously.
  const MAX_GRID_VANTAGES = 4;

  // vantageColorVar maps a vantage to a CSS var NAME: the neutral median color for 'local',
  // else a palette slot keyed by the vantage's position among `ordered`'s non-local entries
  // (mod palette length) — stable as long as `ordered` (from orderVantages) is stable.
  function vantageColorVar(vantage, ordered) {
    if (vantage === 'local') return '--median-base';
    const rest = (ordered || []).filter((v) => v !== 'local');
    const i = rest.indexOf(vantage);
    return VPAL[(i < 0 ? 0 : i) % VPAL.length];
  }

  // --- Graphs-grid multi-vantage helpers (a target measured from >1 vantage) ---
  // STAT_SEV ranks a target's dot severity. 'down' (outage/heavy loss) beats 'degraded' (light
  // loss) beats a healthy/no-data 0; among the 0s, a real 'ok' (has data) beats 'nodata'.
  const STAT_SEV = { nodata: 0, ok: 0, degraded: 1, down: 2 };
  // worstStatus reduces a target's per-vantage statuses to the single dot the grid shows, so a
  // target that is healthy on one vantage but losing packets on another still flags. Mirrors the
  // nav folder rule: highest severity wins; 'ok' outranks 'nodata' at the shared 0 severity.
  function worstStatus(list) {
    let worst = 'nodata';
    for (const s of (list || [])) {
      if (!(s in STAT_SEV)) continue;
      if (worst === 'nodata' ? s !== 'nodata' : STAT_SEV[s] > STAT_SEV[worst]) worst = s;
    }
    return worst;
  }
  // availableVantages returns the ordered union of every target's vantage set from an /api/targets
  // listing — the vantages the Graphs control offers. Always includes 'local' first; ['local'] when
  // no target declares more, so a single-vantage deployment shows no control at all.
  function availableVantages(targets) {
    const set = new Set(['local']);
    for (const t of (targets || [])) for (const v of vantageList(t)) if (v) set.add(v);
    return orderVantages([...set]);
  }
  // toggleGridVantage flips one vantage in the shown set and returns the new ordered set, keeping at
  // least one vantage selected (you can't hide everything) and ignoring vantages that no longer
  // exist. `max` (optional) caps how many can be selected at once: toggling ON beyond the cap is a
  // no-op (the click is refused, not silently dropping another selection). Toggling OFF is always
  // allowed. Pure, so the toolbar's reducer is unit-testable.
  function toggleGridVantage(shown, key, all, max) {
    const allow = new Set(all || []);
    let next = (shown || []).filter((v) => allow.has(v));
    if (next.includes(key)) { if (next.length > 1) next = next.filter((v) => v !== key); }
    else if (allow.has(key) && !(max && next.length >= max)) next.push(key);
    if (!next.length) next = ['local'];
    return orderVantages(next);
  }

  // vantageControlChips builds the Graphs-toolbar vantage toggle buttons (pure, so the cap's
  // disabled-state is unit-testable). `colorFor(v)` returns the swatch color (injected because the
  // live control resolves it from CSS vars). At the cap (`selected.length >= max`) every un-selected
  // chip is disabled with an explanatory title — deselect one to free a slot — so a 5th overlay can't
  // reuse a palette color; selected chips stay enabled so you can always toggle one off.
  function vantageControlChips(available, selected, focus, max, colorFor) {
    const sel = new Set(selected || []);
    const atCap = max && (selected || []).length >= max;
    return '<span class="vseg-lbl">Vantages</span>' + (available || []).map((v) => {
      const on = sel.has(v), band = on && v === focus;
      const role = band ? 'band' : on ? 'line' : 'off';
      const locked = !!(atCap && !on);
      const title = locked
        ? 'Max ' + max + ' vantages — deselect one to add ' + v
        : (on ? 'Hide ' : 'Show ') + v + ' on every graph';
      return '<button type="button" class="vseg-chip' + (on ? ' on' : '') + (band ? ' band' : '') + (locked ? ' locked' : '') + '" data-v="' + esc(v) +
        '" aria-pressed="' + on + '"' + (locked ? ' disabled aria-disabled="true"' : '') + ' title="' + esc(title) + '">' +
        '<i style="background:' + colorFor(v) + '"></i><span class="vn">' + esc(v) + '</span><span class="vr">' + role + '</span></button>';
    }).join('');
  }
  // bandVantageFor returns the vantage whose band+median a panel should draw: the first SELECTED
  // vantage (in shown order) that actually measures this target. null means no selected vantage
  // measures it — so the panel is hidden rather than drawn blank (the "hide unmeasured" rule).
  // Also decides which overlays a panel gets (the other selected vantages that measure it).
  function bandVantageFor(targetVantages, shown) {
    const measures = new Set((targetVantages && targetVantages.length) ? targetVantages : ['local']);
    for (const v of (shown || [])) if (measures.has(v)) return v;
    return null;
  }

  // gridShowsTarget decides whether a target is a Graphs-grid panel candidate for the current
  // vantage selection. A target with local data always qualifies (the grid's default local read).
  // A remote-only target (no local row, no_data) qualifies once a NON-local vantage that measures it
  // is selected — so the vantage selector can surface a site the hub can't reach, instead of leaving
  // it reachable only in the tree/detail view (CODE_REVIEW M10). A no-data LOCAL target stays out: it
  // would be an empty "collecting…" panel, and only a selected remote vantage can actually fill one.
  function gridShowsTarget(t, selectedVantages) {
    if (t && !t.no_data) return true;
    return vantageList(t).some((v) => v !== 'local' && (selectedVantages || []).includes(v));
  }

  // adminMode maps the status of GET /api/admin/vantages to the Vantages panel's display
  // mode: 200 authorized (show the list), 401 log in, 404 admin management disabled (no
  // SMOKED_ADMIN_PASSWORD -> routes unregistered), anything else a transient error.
  function adminMode(status) {
    if (status === 200) return 'list';
    if (status === 401) return 'login';
    if (status === 404) return 'disabled';
    return 'error';
  }

  // adminSessionState maps only authoritative session-probe responses to a visible account state.
  // Network failures and 5xx responses are UNKNOWN, not "logged out": claiming logout without a
  // confirmed 401 can leave the HttpOnly cookie active while giving the user a false security cue.
  function adminSessionState(status) {
    if (status === 204) return 'in';
    if (status === 401) return 'out';
    if (status === 404) return 'disabled';
    return 'unknown';
  }

  // createAdminStateController serializes async session probes and explicit logout transitions.
  // Only the newest probe may paint the account bar; unknown responses leave its last confirmed
  // state alone, and a logout transition is authoritative only after the clearing POST returns 204.
  // Keeping this small state machine outside init() makes request reordering/failure testable.
  function createAdminStateController(apply) {
    let generation = 0;
    return {
      beginProbe() { return ++generation; },
      resolveProbe(gen, status) {
        if (gen !== generation) return false;
        const state = adminSessionState(status);
        if (state === 'unknown') return false;
        apply(state);
        return true;
      },
      confirmLogout(status) {
        if (status !== 204) return false;
        generation++; // invalidate every session probe started before the cookie was cleared
        apply('out');
        return true;
      },
    };
  }

  // A Config/Vantages reachability probe may update the shared status line only while the view
  // that launched it still owns that line. Kept pure so a deferred-response navigation is covered.
  function statusProbeOwnsView(expectedView, activeView) {
    return expectedView === activeView;
  }

  // relTime renders a short "time ago" for a last-seen instant. `then` is ms-epoch or an
  // RFC3339 string; null/empty/zero/unparseable is "never". `now` defaults to Date.now().
  function relTime(then, now) {
    if (then == null || then === '' || then === 0) return 'never';
    const t = typeof then === 'number' ? then : Date.parse(then);
    if (isNaN(t)) return 'never';
    const s = Math.max(0, Math.round(((now == null ? Date.now() : now) - t) / 1000));
    if (s < 45) return 'just now';
    const m = Math.round(s / 60);
    if (m < 60) return m + 'm ago';
    const h = Math.round(m / 60);
    if (h < 24) return h + 'h ago';
    return Math.round(h / 24) + 'd ago';
  }

  // --- DB config fragment helpers (pure; the Config panel's read-modify-write core) ---
  // A fragment is { targets: { children: { <name>: <node> } } }. add/edit/remove return a
  // NEW deep-cloned fragment (never mutate the live doc; it's replaced only after a save).
  function cfgClone(doc) { return JSON.parse(JSON.stringify(doc || {})); }
  function cfgWithChildren(doc) {
    const d = cfgClone(doc);
    if (!d.targets) d.targets = {};
    if (!d.targets.children) d.targets.children = {};
    return d;
  }
  function listTargets(doc) {
    const ch = (doc && doc.targets && doc.targets.children) || {};
    return Object.keys(ch).sort().map((name) => {
      const node = ch[name] || {};
      const isFolder = !!(node.children && Object.keys(node.children).length);
      return { name, node, isFolder };
    });
  }
  // Both writes go through defineProperty, not `children[name] = node`: a plain
  // assignment with name === '__proto__' hits Object.prototype's __proto__ accessor
  // instead of creating an own property — the target silently vanishes (never appears
  // in Object.keys/JSON output) and the hasOwnProperty dup check above is bypassed on
  // the next add. defineProperty always creates a real own data property, __proto__
  // included, so listTargets/JSON.stringify see it like any other name (parked review
  // finding from Task 1).
  // addTarget is the top-level add — a thin alias for addNodeAtPath at parentPath '' (defined
  // in the config-tree helpers below), which keeps the defineProperty '__proto__' handling and
  // the dup guard in one place.
  function addTarget(doc, name, node) { return addNodeAtPath(doc, '', name, node); }
  function editTarget(doc, name, node) {
    const d = cfgWithChildren(doc);
    if (!Object.prototype.hasOwnProperty.call(d.targets.children, name)) throw new Error('no target named "' + name + '"');
    Object.defineProperty(d.targets.children, name, { value: node, enumerable: true, writable: true, configurable: true });
    return d;
  }
  function removeTarget(doc, name) {
    const d = cfgWithChildren(doc);
    delete d.targets.children[name];
    return d;
  }
  function buildTargetNode(f) {
    const node = {};
    if (f.probe) node.probe = f.probe;
    if (f.host) node.host = f.host;
    const p = {};
    const params = f.params || {};
    for (const k of Object.keys(params)) { const key = k.trim(); if (key) p[key] = String(params[k]); }
    if (Object.keys(p).length) node.params = p;
    const vs = (f.vantages || []).map((s) => s.trim()).filter(Boolean);
    if (vs.length) node.vantages = vs;
    const al = (f.alerts || []).map((s) => s.trim()).filter(Boolean);
    if (al.length) node.alerts = al;
    return node;
  }
  // buildGroupNode assembles a grouping (folder) node from the "Add group" form: a `children`
  // map built from one or more child target rows, plus optional group-level vantages/alerts that
  // the config inherits down to every child. A group MUST carry at least one child target — the
  // config validator rejects an empty node (no host, no children), and `children,omitempty` drops
  // an empty map on save, so a childless group could never round-trip. Each child needs a name and
  // a host (a hostless leaf is itself an empty node). Throws — with a user-facing message — on no
  // usable child, a missing host, a duplicate or reserved child name, or a "/" in a child name
  // (the structural path separator). Fully-blank rows are skipped so a stray empty row is harmless.
  function buildGroupNode(f) {
    const kids = {};
    const seen = new Set();
    let count = 0;
    for (const c of (f.children || [])) {
      const name = (c.name || '').trim();
      const host = (c.host || '').trim();
      if (!name && !host) continue; // blank row — ignore
      if (!name) throw new Error('every group target needs a name');
      if (name.includes('/')) throw new Error('target names can\'t contain "/"');
      if (['__proto__', 'constructor', 'prototype'].includes(name)) throw new Error('"' + name + '" is a reserved name');
      if (!host) throw new Error('target "' + name + '" needs a host');
      if (seen.has(name)) throw new Error('duplicate target name "' + name + '" in the group');
      seen.add(name);
      // defineProperty (not kids[name]=) so a child literally named '__proto__' would be a real own
      // property — though the reserved-name guard above already rejects it; kept for symmetry with
      // addNodeAtPath's storage.
      Object.defineProperty(kids, name, { value: buildTargetNode({ probe: c.probe, host, params: c.params }), enumerable: true, writable: true, configurable: true });
      count++;
    }
    if (!count) throw new Error('add at least one target to the group');
    const node = { children: kids };
    const vs = (f.vantages || []).map((s) => s.trim()).filter(Boolean);
    if (vs.length) node.vantages = vs;
    const al = (f.alerts || []).map((s) => s.trim()).filter(Boolean);
    if (al.length) node.alerts = al;
    return node;
  }

  // --- Config-tree helpers (pure; the Config tab's read-modify-write tree core) ---
  function cfgSortSiblings(ch) {
    return Object.keys(ch).sort((a, b) => {
      const wa = (ch[a] && ch[a].weight) || 0, wb = (ch[b] && ch[b].weight) || 0;
      return wa !== wb ? wa - wb : (a < b ? -1 : a > b ? 1 : 0);
    });
  }
  function cfgTree(doc) {
    const walk = (ch, prefix) => cfgSortSiblings(ch).map((name) => {
      const node = ch[name] || {};
      const path = prefix ? prefix + '/' + name : name;
      // A node is a folder if it carries a `children` map — even an empty one. Mutations prune an
      // invalid empty hostless group, but defensively preserving explicit folder identity here
      // avoids rendering imported/stale `children: {}` as a phantom editable host leaf.
      const isFolder = !!(node.children && typeof node.children === 'object');
      const kids = isFolder && Object.keys(node.children).length ? walk(node.children, path) : [];
      return { name, node, path, isFolder, weight: node.weight || 0, children: kids };
    });
    const ch = (doc && doc.targets && doc.targets.children) || {};
    return walk(ch, '');
  }
  function reweightSiblings(orderedNames) {
    const out = {}; orderedNames.forEach((n, i) => { out[n] = i; }); return out;
  }
  // childrenAtPath returns the children map that holds `parentPath`'s members ('' = top level),
  // in a cloned doc; returns null if the path doesn't resolve.
  function cfgChildrenAt(d, parentPath) {
    let ch = (d.targets = d.targets || {}, d.targets.children = d.targets.children || {});
    if (!parentPath) return ch;
    for (const seg of parentPath.split('/')) {
      if (!ch[seg] || !ch[seg].children) return null;
      ch = ch[seg].children;
    }
    return ch;
  }
  function reorderSiblings(doc, parentPath, orderedNames) {
    const d = cfgClone(doc); const ch = cfgChildrenAt(d, parentPath);
    if (ch) { const w = reweightSiblings(orderedNames); for (const n of orderedNames) if (ch[n]) ch[n].weight = w[n]; }
    return d;
  }
  function cfgNodeAt(d, path) { // returns {parent: childrenMap, key} for a full node path
    const segs = path.split('/'); const key = segs.pop();
    const parent = cfgChildrenAt(d, segs.join('/'));
    return parent ? { parent, key } : null;
  }
  function editNodeAtPath(doc, path, node) {
    const d = cfgClone(doc); const loc = cfgNodeAt(d, path);
    if (loc && Object.prototype.hasOwnProperty.call(loc.parent, loc.key)) {
      const old = loc.parent[loc.key] || {};
      // The form owns only these fields. Start from the complete existing node so imported fields
      // it does not expose (title/ip/pings/step/alertee and future extensions) survive an edit, but
      // delete the owned fields first so clearing one in the form really removes it / restores
      // inheritance rather than retaining the old value.
      const merged = Object.assign({}, old);
      for (const k of ['probe', 'host', 'params', 'vantages', 'alerts']) delete merged[k];
      Object.assign(merged, node);
      loc.parent[loc.key] = merged;
    }
    return d;
  }
  // Remove hostless grouping nodes that became empty after a remove/move. Such a node cannot pass
  // server validation or round-trip through Node.children,omitempty, so keeping it would make the
  // otherwise-valid tree mutation fail with 400. A node with a host remains a real target even when
  // its last child moves out.
  function cfgPruneEmptyGroups(ch) {
    for (const name of Object.keys(ch || {})) {
      const node = ch[name];
      if (!node || !node.children || typeof node.children !== 'object') continue;
      cfgPruneEmptyGroups(node.children);
      if (!node.host && Object.keys(node.children).length === 0) delete ch[name];
    }
  }
  function removeNodeAtPath(doc, path) {
    const d = cfgClone(doc); const loc = cfgNodeAt(d, path);
    if (loc) delete loc.parent[loc.key];
    cfgPruneEmptyGroups(d.targets && d.targets.children);
    return d;
  }
  // renameNodeAtPath rekeys the node at `path` to `newName` within its parent, preserving its
  // subtree, every non-order field, and visible position among siblings. Existing weights stay
  // untouched when they already preserve that position; only an alphabetical crossing within a
  // weight tie triggers sibling re-sequencing. A no-op when the name is unchanged or the path is
  // stale (returns the clone); throws on a sibling name collision. `newName` must be one segment —
  // the caller rejects "/" (the structural path separator) before calling.
  function renameNodeAtPath(doc, path, newName) {
    const d = cfgClone(doc); const loc = cfgNodeAt(d, path);
    if (!loc || !Object.prototype.hasOwnProperty.call(loc.parent, loc.key)) return d; // stale UI
    if (newName === loc.key) return d; // unchanged
    if (Object.prototype.hasOwnProperty.call(loc.parent, newName)) {
      const pp = path.split('/').slice(0, -1).join('/');
      throw new Error('a target named "' + newName + '" already exists' + (pp ? ' in "' + pp + '"' : ' at the top level'));
    }
    // Sorting is (weight,name), not insertion order. Capture the visible order before the rekey and
    // then assign unambiguous sequential weights, otherwise a tied weight lets the NEW name move the
    // row alphabetically despite this helper's position-preservation contract.
    const order = cfgSortSiblings(loc.parent).map((k) => k === loc.key ? newName : k);
    const value = loc.parent[loc.key];
    delete loc.parent[loc.key];
    Object.defineProperty(loc.parent, newName, { value, enumerable: true, writable: true, configurable: true });
    if (cfgSortSiblings(loc.parent).some((name, i) => name !== order[i])) {
      const weights = reweightSiblings(order);
      for (const name of order) loc.parent[name].weight = weights[name];
    }
    return d;
  }
  // cfgEnsureChildrenAt is like cfgChildrenAt but CREATES the `children` map on any existing
  // node along `parentPath` that lacks one — so adding/moving a node INTO a leaf turns that
  // leaf into a folder-with-target (SmokePing allows a node to be both). Still returns null if
  // an intermediate segment names a node that doesn't exist (the parent must already be there).
  function cfgEnsureChildrenAt(d, parentPath) {
    let ch = (d.targets = d.targets || {}, d.targets.children = d.targets.children || {});
    if (!parentPath) return ch;
    for (const seg of parentPath.split('/')) {
      if (!Object.prototype.hasOwnProperty.call(ch, seg)) return null;
      const node = ch[seg];
      if (!node.children) node.children = {};
      ch = node.children;
    }
    return ch;
  }
  // addNodeAtPath is the path-aware add: it inserts `node` under `parentPath` ('' = top level,
  // the plain addTarget case). Throws on a duplicate name in that group, or if `parentPath`
  // names a node that doesn't exist. Like addTarget it stores via defineProperty so a name of
  // '__proto__' round-trips as a real own property instead of vanishing into the prototype.
  function addNodeAtPath(doc, parentPath, name, node) {
    const d = cfgWithChildren(doc);
    const ch = cfgEnsureChildrenAt(d, parentPath || '');
    if (!ch) throw new Error('no folder at "' + parentPath + '"');
    if (Object.prototype.hasOwnProperty.call(ch, name)) {
      throw new Error('a target named "' + name + '" already exists' + (parentPath ? ' in "' + parentPath + '"' : ''));
    }
    Object.defineProperty(ch, name, { value: node, enumerable: true, writable: true, configurable: true });
    return d;
  }
  // moveNode relocates the node subtree at `srcPath` into `destParentPath` ('' = top level) at
  // position `index` among that group, and REWEIGHTS both sibling sets to sequential weights so
  // the (weight,name) sort in cfgTree reproduces the new order exactly. A same-parent move is a
  // pure reorder (the drag/keyboard reorder path). Throws when the destination already holds a
  // node of the same name (a real collision), or — the guard the tree DnD relies on — when the
  // destination is the node itself or one of its own descendants (which would orphan the subtree
  // and form a cycle). A `srcPath` that no longer resolves is a no-op (stale UI), returning the
  // clone unchanged. `index` is clamped into range; null/omitted appends.
  function moveNode(doc, srcPath, destParentPath, index) {
    const d = cfgClone(doc);
    const dest = destParentPath || '';
    if (dest === srcPath || dest.startsWith(srcPath + '/')) {
      throw new Error('cannot move "' + srcPath + '" into its own subtree');
    }
    const src = cfgNodeAt(d, srcPath);
    if (!src || !Object.prototype.hasOwnProperty.call(src.parent, src.key)) return d;
    const name = src.key;
    const node = src.parent[name];
    const destCh = cfgEnsureChildrenAt(d, dest);
    if (!destCh) throw new Error('no folder at "' + dest + '"');
    const srcParent = src.parent;
    const sameParent = destCh === srcParent; // same children map => this is a reorder
    if (!sameParent && Object.prototype.hasOwnProperty.call(destCh, name)) {
      throw new Error('a target named "' + name + '" already exists' + (dest ? ' in "' + dest + '"' : ' at the top level'));
    }
    delete srcParent[name]; // detach from the source group (== dest group when sameParent)
    // With the node removed, build the destination order and splice it into the requested slot.
    const destOrder = cfgSortSiblings(destCh);
    const at = Math.max(0, Math.min(index == null ? destOrder.length : index, destOrder.length));
    destOrder.splice(at, 0, name);
    Object.defineProperty(destCh, name, { value: node, enumerable: true, writable: true, configurable: true });
    const dw = reweightSiblings(destOrder);
    for (const n of destOrder) if (destCh[n]) destCh[n].weight = dw[n];
    if (!sameParent) { // re-sequence what's left of the source group too
      const srcOrder = cfgSortSiblings(srcParent);
      const sw = reweightSiblings(srcOrder);
      for (const n of srcOrder) if (srcParent[n]) srcParent[n].weight = sw[n];
    }
    cfgPruneEmptyGroups(d.targets && d.targets.children);
    return d;
  }

  // The row under the pointer determines the drag destination. Dropping ON a folder always means
  // move into it, including when source and folder are siblings; dropping on a leaf means reorder /
  // move before that leaf in its parent. Keyboard Alt+Arrow remains the unambiguous way to reorder
  // a folder itself among siblings.
  function cfgDropDestination(from, targetPath, targetIsFolder) {
    if (!from || targetPath === from) return null;
    const parentOf = (p) => p.split('/').slice(0, -1).join('/');
    const destParent = targetIsFolder ? targetPath : parentOf(targetPath);
    if (destParent === from || destParent.startsWith(from + '/')) return null;
    return { destParent, kind: targetIsFolder ? 'into' : 'before' };
  }
  // moveInList returns a NEW array with `name` shifted by `delta` slots (clamped to the ends) —
  // the pure core of the Alt+Up / Alt+Down keyboard reorder. The caller feeds the result to
  // reorderSiblings. A `name` not in the list, or a no-op shift, returns an equal-order copy.
  function moveInList(names, name, delta) {
    const out = (names || []).slice();
    const i = out.indexOf(name);
    if (i < 0) return out;
    const j = Math.max(0, Math.min(i + (delta || 0), out.length - 1));
    if (i === j) return out;
    out.splice(i, 1); out.splice(j, 0, name);
    return out;
  }
  // cfgVisibleRows flattens a cfgTree into the ordered list of rows a user can actually see
  // and arrow between, honouring collapsed folders (`collapsed` is a Set of collapsed paths, or
  // anything with a .has()). Each row carries what the keyboard decision needs: whether it is a
  // folder, whether it is currently expanded, and its parent path. Pure — the DOM keyboard
  // handler builds its Set from local collapse state and passes it straight through.
  function cfgVisibleRows(tree, collapsed) {
    const has = (p) => !!(collapsed && collapsed.has && collapsed.has(p));
    const out = [];
    (function walk(nodes) {
      for (const n of nodes) {
        const isCollapsed = n.isFolder && has(n.path);
        out.push({
          path: n.path, name: n.name, isFolder: n.isFolder,
          parentPath: n.path.split('/').slice(0, -1).join('/'),
          expanded: n.isFolder ? !isCollapsed : null,
        });
        if (n.isFolder && !isCollapsed) walk(n.children);
      }
    })(tree || []);
    return out;
  }
  // cfgTreeKey is the pure WAI-ARIA tree keyboard decision: given the visible rows, the focused
  // path, the pressed key and whether Alt is held, it returns the action the DOM layer should
  // apply — or null to ignore the key. Up/Down move focus; Home/End jump to the ends; Right
  // expands a collapsed folder then steps into it, Left collapses an expanded folder then steps
  // to the parent (the standard tree pattern); Alt+Up / Alt+Down reorder the focused node among
  // its siblings. Keeping every branch here (not in the handler) is what keeps the DOM thin.
  function cfgTreeKey(rows, focusPath, key, alt) {
    if (!rows || !rows.length) return null;
    let i = rows.findIndex((r) => r.path === focusPath);
    if (i < 0) i = 0;
    const row = rows[i];
    const parentOf = (p) => p.split('/').slice(0, -1).join('/');
    if (alt && (key === 'ArrowUp' || key === 'ArrowDown')) {
      return { type: 'reorder', parentPath: parentOf(row.path), name: row.path.split('/').pop(), delta: key === 'ArrowUp' ? -1 : 1, path: row.path };
    }
    switch (key) {
      case 'ArrowDown': return i < rows.length - 1 ? { type: 'focus', path: rows[i + 1].path } : null;
      case 'ArrowUp':   return i > 0 ? { type: 'focus', path: rows[i - 1].path } : null;
      case 'Home':      return { type: 'focus', path: rows[0].path };
      case 'End':       return { type: 'focus', path: rows[rows.length - 1].path };
      case 'ArrowRight':
        if (row.isFolder && row.expanded === false) return { type: 'expand', path: row.path };
        if (row.isFolder && row.expanded === true && i < rows.length - 1 && rows[i + 1].path.startsWith(row.path + '/')) return { type: 'focus', path: rows[i + 1].path };
        return null;
      case 'ArrowLeft':
        if (row.isFolder && row.expanded === true) return { type: 'collapse', path: row.path };
        { const pp = parentOf(row.path); return pp ? { type: 'focus', path: pp } : null; }
      default: return null;
    }
  }

  const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  // labelHTML renders a target's graph-title label: the title (falling back to the target
  // name) plus, when present, its IP in parentheses. Pure; exported for tests and reused by
  // every title render site.
  function labelHTML(name, title, ip) {
    const base = esc(title || name);
    return ip ? base + ' <span class="tgtip">(' + esc(ip) + ')</span>' : base;
  }

  // collectingNote picks the placeholder shown when a panel has fewer than the 2 buckets it needs
  // to draw a line/band. A band panel names the history it is still accumulating (daily appears
  // once data spans 2 day-buckets, hourly once it spans 2 hour-buckets) so a fresh deployment
  // reads as "filling in" rather than "broken" — the long-range daily band is empty for up to a
  // day after first launch purely because one UTC day is a single bucket. Raw panels, which are
  // just waiting for the first couple of rounds, keep the bare "collecting…".
  function collectingNote(mode, res) {
    if (mode === 'band') {
      return res === '1d'
        ? 'collecting… — daily band appears once history spans 2 days'
        : 'collecting… — hourly band appears once history spans 2 hours';
    }
    return 'collecting…';
  }

  // --- Vantage bundle download (pure) ---

  // vantageBundleFilename builds the download filename for a freshly minted vantage's
  // onboarding bundle, mirroring the server's Content-Disposition (`<name>-vantage.tar.gz`,
  // see addVantage in internal/api/api.go). The server's vantage.ValidName already restricts
  // real vantage names, but this is building a FILENAME, not validating a vantage name — it
  // sanitizes independently so an odd/hostile `name` (whatever reaches this helper) can never
  // produce a path separator or other unsafe character in a browser download. Any character
  // outside the safe set becomes '-'; an all-unsafe (or empty) name falls back to "vantage"
  // rather than emitting a bare "-vantage.tar.gz".
  function vantageBundleFilename(name) {
    const safe = String(name == null ? '' : name).replace(/[^A-Za-z0-9._-]/g, '-') || 'vantage';
    return safe + '-vantage.tar.gz';
  }

  // tkey is a target's STABLE routing/identity key from an /api/targets (or /api/series/all) DTO: its
  // server-assigned id, falling back to the display path when no id is set (a bare setup, or the
  // series/all join, which predate ids). The dashboard routes, caches, keys panels, and builds deep
  // links by this — NOT the mutable display path — so an open tab / bookmark survives the target
  // being moved in the config tree (CODE_REVIEW L8). The human path is shown via nameByKey, not this.
  function tkey(t) { return (t && t.id != null && t.id !== '') ? t.id : (t && t.name); }

  // ntpStatHtml renders the NTP companion clock stat (offset + stratum) shown beside median/loss.
  // `signed` (an offset-graphing panel) hides the redundant offset — it is already the graphed series
  // and the median stat — and shows only stratum. Empty string when there is no reading. Pure, so the
  // grid, the per-vantage detail stat (CODE_REVIEW M2), and tests all share one formatter.
  function ntpStatHtml(stat, signed) {
    if (!stat) return '';
    let html = '';
    if (!signed) {
      const a = Math.abs(stat.off), mag = a >= 100 ? a.toFixed(0) : a >= 1 ? a.toFixed(1) : a.toFixed(2);
      const off = (stat.off >= 0 ? '+' : '−') + mag + ' ms'; // U+2212 minus, matching the axis labels
      html += '<span class="stat"><span class="k">offset</span><span class="v">' + off + '</span></span>';
    }
    return html + '<span class="stat"><span class="k">stratum</span><span class="v">' + stat.stratum + '</span></span>';
  }

  // ntpStatOf extracts a target DTO's live NTP clock stat (offset/stratum/measure), or null when the
  // target has no synced reading. Shared by every registry writer (the hub-local ntpByName and the
  // per-vantage ntpByVantage) so the "is there a reading" rule lives in exactly one place.
  function ntpStatOf(t) {
    return (t && typeof t.ntp_offset_ms === 'number') ? { off: t.ntp_offset_ms, stratum: t.stratum, measure: t.ntp_measure } : null;
  }

  // ntpStatSelect chooses which clock reading a panel shows for a vantage: the hub-local reading for
  // the local/default vantage, the per-vantage reading for a remote one. Exported (not merely used
  // internally) so the Graphs-grid M2 attribution — a remote-focused panel must show THAT vantage's
  // offset/stratum, not the hub's — is unit-testable; the DOM path that selects the registry needs a
  // real browser to run, but this rule is where the regression lived.
  function ntpStatSelect(vantage, local, remote) {
    return (!vantage || vantage === 'local') ? local : remote;
  }

  window.Dash = { RANGES, RANGE_ORDER, parseRoute, mergeSeries, gridSince, gridTemplateFor, maxColumnsFor, rangeLabels, fetchJSON, zoomResolution, pixelToTime, sharedYMax, buildTree, underPath, targetStatus, pickSeries, vantageList, orderVantages, defaultFocus, keepFocus, vantageColorVar, worstStatus, availableVantages, toggleGridVantage, vantageControlChips, bandVantageFor, gridShowsTarget, adminMode, adminSessionState, createAdminStateController, statusProbeOwnsView, relTime, listTargets, addTarget, editTarget, removeTarget, buildTargetNode, buildGroupNode, labelHTML, collectingNote, vantageBundleFilename, cfgTree, reweightSiblings, reorderSiblings, editNodeAtPath, removeNodeAtPath, renameNodeAtPath, addNodeAtPath, moveNode, moveInList, cfgDropDestination, cfgVisibleRows, cfgTreeKey, tkey, ntpStatHtml, ntpStatOf, ntpStatSelect };

  // ---------------------------------------------------------------- init (DOM) --
  function init() {
    const $ = (id) => document.getElementById(id);
    const fmt = (v, d) => (v == null || isNaN(v)) ? '--' : v.toFixed(d);
    // fmtMs formats a millisecond median stat. Latency keeps one decimal (unchanged). A signed
    // (offset) panel scales its decimals to the magnitude so a sub-0.1ms clock offset doesn't
    // collapse to "0.0 ms" / "-0.0 ms" the way a fixed toFixed(1) does (L5).
    const fmtMs = (v, signed) => {
      if (v == null || isNaN(v)) return '--';
      if (!signed) return v.toFixed(1);
      const a = Math.abs(v);
      return v.toFixed(a >= 1 ? 1 : a >= 0.1 ? 2 : 3);
    };
    const enc = encodeURIComponent;

    // ---- data fetch for one range (raw series, or a server-windowed rollup band) ----
    async function fetchRange(name, key, vantage) {
      const vq = vantage ? '&vantage=' + enc(vantage) : '';
      const R = RANGES[key];
      if (R.mode === 'raw') {
        const r = await fetch('/api/series?target=' + enc(name) + '&window=' + R.window + vq, { cache: 'no-store' });
        if (!r.ok) return null;
        return Smoke.fromApiSeries(await r.json());
      }
      // Bound the rollup to the range window server-side (Go duration, e.g. 240h for
      // 10 days) so we don't fetch the target's full retained history each refresh.
      const r = await fetch('/api/rollup?target=' + enc(name) + '&res=' + R.res + '&window=' + (R.days * 24) + 'h' + vq, { cache: 'no-store' });
      if (r.status === 501) return { unsupported: true };
      if (!r.ok) return null;
      return Smoke.fromApiRollup(await r.json());
    }

    function drawNote(canvas, text, height) {
      canvas.style.height = height + 'px'; canvas.width = canvas.clientWidth; canvas.height = height;
      const ctx = canvas.getContext('2d'); ctx.clearRect(0, 0, canvas.width, canvas.height);
      ctx.fillStyle = getComputedStyle(document.documentElement).getPropertyValue('--ink-faint');
      ctx.font = '12px ui-monospace, monospace'; ctx.textAlign = 'center';
      ctx.fillText(text, canvas.clientWidth / 2, height / 2);
    }
    // yMax (optional) forces a shared Y-axis maximum — the Graphs grid passes one so its
    // small multiples share a scale (unison); omitted, each graph auto-scales to its own data.
    // overlays (optional) — extra per-vantage median-only context lines (Task 3); see
    // buildOverlays. Absent/undefined ⇒ identical output to before overlays existed.
    function renderInto(canvas, s, R, height, yMax, overlays, signed) {
      if (s && s.unsupported) { drawNote(canvas, 'needs the TimescaleDB store (-dsn -downsample)', height); return; }
      if (!s || s.buckets.length < 2) { drawNote(canvas, collectingNote(R.mode, R.res), height); return; }
      // Fixed wall-clock domain [now-windowMs, now]. t1 extends to the newest sample if
      // the client clock lags the server, so a fresh sample never clamps to the edge;
      // t0 anchors to the selected range so the axis labels stay literally correct.
      const lastT = s.buckets[s.buckets.length - 1].t;
      const t1 = Math.max(Date.now(), Number.isFinite(lastT) ? lastT : 0);
      const t0 = R.windowMs ? t1 - R.windowMs : undefined;
      // Relative labels (each range's static xl, e.g. -3h/now) by default; absolute wall-clock
      // (rangeLabels) when the Graphs "absolute time" toggle is on and we have a real window.
      const xlabels = timeAbsolute && t0 != null ? rangeLabels(t0, t1) : R.xl;
      Smoke.render(canvas, s, { height, band: R.mode === 'band', xlabels, t0, t1: t0 == null ? undefined : t1, yMax, overlays, signed });
    }
    // An NTP target graphing clock offset needs the signed (zero-centered, no-floor) y-axis. The
    // metric comes from /api/targets' config-derived `metric` (metricByName), NOT the live offset
    // stat — so the signed axis survives a restart or a target that hasn't synced yet (M6).
    function ntpSigned(name) { return metricByName.get(name) === 'offset'; }
    // metaHtml renders a detail/zoom cell's stat band. `vantage` is the currently-focused vantage:
    // its NTP offset/stratum is appended (via the per-vantage stat, CODE_REVIEW M2) so a remote
    // vantage's clock reading — or a remote-only NTP target's — is visible in the detail view, not
    // just the local Graphs grid. curTarget is the focused target for every metaHtml caller.
    function metaHtml(s, vantage) {
      const st = Smoke.seriesStats(s); const lcls = st.lossAvg > 2 ? 'bad' : st.lossAvg > 0.5 ? 'warn' : '';
      const signed = ntpSigned(curTarget);
      return '<span class="stat"><span class="k">median avg</span><span class="v">' + fmtMs(st.medAvg, signed) + ' ms</span></span>' +
             '<span class="stat"><span class="k">median max</span><span class="v">' + fmtMs(st.medMax, signed) + ' ms</span></span>' +
             '<span class="stat"><span class="k">loss avg</span><span class="v ' + lcls + '">' + fmt(st.lossAvg, 2) + ' %</span></span>' +
             ntpStatsHtmlForVantage(curTarget, vantage);
    }

    // ---- per-vantage overlay helpers (Task 3) ----
    // cssVar resolves a CSS var NAME (e.g. '--v-a') to its current value, read fresh from
    // computed style each call so overlay colors track the live theme (light/dark toggle)
    // without any extra re-render hook — callers just re-invoke this at render time.
    function cssVar(name) { return getComputedStyle(document.documentElement).getPropertyValue(name).trim(); }
    // buildOverlays turns a per-vantage series map into Smoke.render's opts.overlays: every
    // vantage in `vs` except the focused one, dropping any that failed to fetch (null) or
    // are the 'unsupported' sentinel (not real data) — never pass a bucket-less object.
    function buildOverlays(byV, vs, focus) {
      return (vs || []).filter((v) => v !== focus && byV[v] && !byV[v].unsupported)
        .map((v) => ({ series: byV[v], color: cssVar(Dash.vantageColorVar(v, vs)) }));
    }
    // lastMedian returns the newest non-NaN median in a series, for a chip's at-a-glance
    // value; null when there's no series or every bucket is NaN (fully lost/no data yet).
    function lastMedian(s) {
      if (!s || !s.buckets) return null;
      for (let i = s.buckets.length - 1; i >= 0; i--) { const m = s.buckets[i].median; if (!isNaN(m)) return m; }
      return null;
    }
    // vchipsHtml renders the legend/selector chips for an ordered vantage list: a colored
    // swatch (the resolved overlay color), the vantage name, and its last value (via
    // valueFor). aria-pressed marks the focused chip, doubling the legend as a toggle.
    function vchipsHtml(vs, focus, valueFor) {
      return vs.map((v) => {
        const color = cssVar(Dash.vantageColorVar(v, vs));
        const lv = valueFor(v);
        return '<button type="button" class="vchip" aria-pressed="' + (v === focus) + '" data-v="' + esc(v) + '">' +
          '<i style="background:' + color + '"></i><span class="vname">' + esc(v) + '</span>' +
          '<span class="vval">' + (lv == null ? '—' : fmt(lv, 1) + ' ms') + '</span></button>';
      }).join('');
    }

    // ---- Overview: worst-targets + availability ----
    let worstBy = 'loss';
    async function refreshWorst() {
      const list = $('worstList');
      let rows;
      try { rows = (await fetchJSON('/api/charts?by=' + worstBy + '&n=8')).charts || []; }
      catch (e) { return false; } // transient failure: keep the last-known list, report degraded
      if (!rows.length) { list.innerHTML = '<li class="empty-row">no data yet</li>'; return true; }
      list.innerHTML = rows.map((c, i) => {
        let val, cls = '';
        if (worstBy === 'loss') { val = fmt(c.loss_pct, 1) + ' %'; cls = c.loss_pct > 2 ? 'bad' : c.loss_pct > 0.5 ? 'warn' : ''; }
        else if (worstBy === 'median') { val = fmt(c.median_ms, 1) + ' ms'; }
        else { val = '± ' + fmt(c.stddev_ms, 1) + ' ms'; }
        // Route by the stable id the charts DTO now carries, so a click deep-links durably even on a
        // fresh page load before the target catalog is fetched; fall back to the seeded map or the
        // display path (CODE_REVIEW L8).
        return '<li><span class="n">' + (i + 1) + '</span><span class="who" data-target="' + esc(c.id || idByName.get(c.name) || c.name) + '">' + esc(c.name) +
          '<span class="pk">' + esc(c.probe) + '</span></span><span class="val ' + cls + '">' + val + '</span></li>';
      }).join('');
      return true;
    }
    const SLA_WINDOW = '24h';
    function humanSpan(sec) { if (sec < 5400) return Math.max(1, Math.round(sec / 60)) + 'm'; if (sec < 172800) return (sec / 3600).toFixed(sec < 36000 ? 1 : 0) + 'h'; return Math.round(sec / 86400) + 'd'; }
    async function refreshSLA() {
      const list = $('slaList');
      let rows;
      try { rows = (await fetchJSON('/api/sla?window=' + SLA_WINDOW)).targets || []; }
      catch (e) { return false; } // transient failure: keep the last-known list, report degraded
      const sub = $('slaWindow'); if (sub) sub.textContent = 'last ' + SLA_WINDOW;
      if (!rows.length) { list.innerHTML = '<li class="empty-row">no data yet</li>'; return true; }
      list.innerHTML =
        '<li class="head"><span class="n">#</span><span class="who">target</span><span class="avail">availability</span><span>coverage</span></li>' +
        rows.slice(0, 8).map((s, i) => {
          const cov = s.coverage_pct; const thin = cov != null && cov < 50;
          const acls = thin ? 'provisional' : s.availability >= 99.5 ? 'ok' : s.availability >= 95 ? 'warn' : 'bad';
          let covCell;
          if (cov == null) { covCell = '<span class="cov"><span class="lbl"><span>coverage</span><b>—</b></span></span>'; }
          else {
            let left = 'covered';
            if (thin) { const t = Date.parse(s.covered_from); if (!isNaN(t)) left = 'watched ' + humanSpan((Date.now() - t) / 1000); }
            covCell = '<span class="cov' + (thin ? ' thin' : '') + '"><span class="lbl"><span>' + left + '</span><b>' + Math.round(cov) + '%</b></span>' +
              '<span class="track"><span class="fill" style="width:' + Math.min(100, cov) + '%"></span></span></span>';
          }
          return '<li><span class="n">' + (i + 1) + '</span>' +
            '<span class="who" data-target="' + esc(s.id || idByName.get(s.name) || s.name) + '">' + esc(s.name) + '<span class="pk">' + esc(s.probe) + '</span>' + (thin ? '<span class="chip">thin data</span>' : '') + '</span>' +
            '<span class="avail ' + acls + '"><span class="pct">' + fmt(s.availability, 2) + ' %</span><span class="frac">' + s.up_rounds + ' / ' + s.measured + ' up</span></span>' +
            covCell + '</li>';
        }).join('');
      return true;
    }
    let overviewBusy = false;
    async function refreshOverview() {
      if (overviewBusy) return; overviewBusy = true;
      try {
        // Only claim "live" when both reads authoritatively succeeded; if either failed
        // its list still shows its last-known data, so flag the view as degraded (#2).
        const [okW, okS] = await Promise.all([refreshWorst(), refreshSLA()]);
        const t = new Date().toLocaleTimeString();
        $('statusText').textContent = (okW && okS) ? 'live · updated ' + t : 'degraded — showing last known · ' + t;
      } finally { overviewBusy = false; }
    }

    // ---- Graphs grid (recent 3h thumbnails) ----
    const panels = new Map();
    // Multi-vantage Graphs grid: gridVantages is the set of vantages drawn on every panel — the first
    // (in orderVantages order) owns the band+median, the rest draw as overlay lines. availVantages is
    // the union offered by the toolbar control (only shown when >1 exists). Default: local only, so a
    // single-vantage deployment is unchanged. Remembered per browser.
    let availVantages = ['local'];
    function loadGridVantages() {
      try { const s = JSON.parse(localStorage.getItem('grid-vantages')); if (Array.isArray(s) && s.length) return s; } catch (e) {}
      return ['local'];
    }
    let gridVantages = loadGridVantages();
    function saveGridVantages() { try { localStorage.setItem('grid-vantages', JSON.stringify(gridVantages)); } catch (e) {} }
    // The focused vantage owns the band; it's the first shown in canonical order.
    function gridFocus() { return orderVantages(gridVantages)[0] || 'local'; }
    // Config-tree menu (left nav) state: the current subtree scope, the filter query,
    // collapsed folders, the target-name set the tree is built from, and per-target status
    // for the menu dots. `treeSig` lets a periodic refresh skip a rebuild when nothing that
    // affects the tree changed, so scroll position and focus survive the 5s cadence.
    let gridScope = '', navQuery = '', treeNames = [], treeSig = null;
    const collapsed = new Set();
    const statusByTarget = new Map();
    // vantagesByTarget mirrors statusByTarget (also fed from /api/targets in refreshGrid):
    // each target's vantage set, so the overlay UI (Task 3) knows what it can show without
    // a per-render fetch. vantagesFor defaults to ['local'] before the first grid refresh.
    const vantagesByTarget = new Map();
    function vantagesFor(name) { return vantagesByTarget.get(name) || ['local']; }
    // probeByName: each target's probe kind, fed from the same /api/targets responses as the
    // maps above, so the detail (stack) and zoom titles can show the probe badge reliably —
    // not just when the grid DOM panel happens to be cached (which a deep link doesn't have).
    const probeByName = new Map();
    function probeBadge(name) { const pk = probeByName.get(name); return pk ? '<span class="probe">' + esc(pk) + '</span> ' : ''; }
    // titleByName/ipByName mirror probeByName (fed from the same /api/targets responses), so a
    // detail/zoom view reached by deep link can render the display label (title + IP) without
    // the grid DOM panel. displayLabel builds it via the shared labelHTML.
    const titleByName = new Map(), ipByName = new Map();
    // The per-target maps above are DUAL-KEYED (see indexTarget): every value is stored under both the
    // stable routing token tkey(t) and the display path t.name, so a lookup by either resolves — a
    // new id-keyed deep link and a legacy path-keyed bookmark both work (CODE_REVIEW L8). nameByKey
    // maps a token back to its current display path (so a UUID-keyed view shows the human path, never
    // the raw id); idByName maps a display path to its stable id (so the path-built tree can put the
    // durable id on each leaf's data-target).
    const nameByKey = new Map(), idByName = new Map();
    function displayLabel(token) { const path = nameByKey.get(token) || token; return labelHTML(path, titleByName.get(token), ipByName.get(token)); }
    // ntpByName mirrors the maps above (same /api/targets responses): NTP targets carry a clock
    // offset (ms) + stratum, which the smoke graph can't show (a signed near-zero value has no place
    // on a min→median→max latency plot), so they render as stats beside median/loss. Absent = a
    // non-NTP target or one with no NTP reading yet.
    const ntpByName = new Map();
    // ntpByVantage holds a target's NTP stat for a REMOTE vantage, keyed by `${vantage}|${token}`,
    // fed by ensureVantageStats from /api/targets?vantage=. The hub-local reading stays in ntpByName
    // (CODE_REVIEW M2 — surface a remote vantage's clock reading in the detail view).
    const ntpByVantage = new Map();
    // metricByName holds every target's config-derived metric ('rtt' | 'offset') from /api/targets,
    // present even with no live NTP reading — the source of truth for the signed-axis choice.
    const metricByName = new Map();
    // ntpStatFor returns a target's NTP clock stat for a specific vantage: the hub-local reading
    // (ntpByName) for the local/default vantage, or the per-vantage reading (ntpByVantage) for a
    // remote one (CODE_REVIEW M2). The local/remote pick is the pure, exported ntpStatSelect.
    function ntpStatFor(token, vantage) {
      return ntpStatSelect(vantage, ntpByName.get(token), ntpByVantage.get(vantage + '|' + token));
    }
    function ntpStatsHtmlForVantage(token, vantage) { return ntpStatHtml(ntpStatFor(token, vantage), ntpSigned(token)); }
    // setNtpVantage records one remote vantage's NTP clock stat for a target into ntpByVantage,
    // dual-keyed by stable id and display path (like ntpByName). Fed from a /api/targets?vantage=
    // list — by ensureVantageStats (detail views) and by refreshGrid, which already fetches every
    // remote vantage's list for the worst-status roll-up, so the grid attributes each panel's clock
    // stat to its own band vantage (M2) at no extra request.
    function setNtpVantage(vantage, t) {
      const stat = ntpStatOf(t);
      for (const key of [vantage + '|' + tkey(t), vantage + '|' + t.name]) {
        if (stat) ntpByVantage.set(key, stat); else ntpByVantage.delete(key);
      }
    }
    function setNtp(t) {
      const k = tkey(t), nm = t.name;
      // Config-derived metric drives the signed axis (present for every target, survives restart).
      const metric = t.metric === 'offset' ? 'offset' : 'rtt';
      metricByName.set(k, metric); metricByName.set(nm, metric);
      // The live offset/stratum stat is a separate, best-effort display value (absent until synced).
      const v = ntpStatOf(t);
      if (v) { ntpByName.set(k, v); ntpByName.set(nm, v); }
      else { ntpByName.delete(k); ntpByName.delete(nm); }
    }
    // indexTarget records one /api/targets DTO into every per-target map under BOTH its stable token
    // and its display path (dual-keyed, CODE_REVIEW L8), and seeds nameByKey/idByName. Status is set
    // by refreshGrid (its authoritative full-list scan), not here, so a deep-link seed doesn't paint
    // dots from a partial view.
    //
    // Known narrow limitation: these are ONE flat namespace, so if one target's display path happens
    // to equal a DIFFERENT target's stable id (a birth-path id another target was later created/moved
    // onto — the same overlap the hub de-collides for ingest), the two collide and the display-only
    // side data (status dot, signed-axis choice, title, NTP stat) for one of them can be wrong. The
    // per-panel series is unaffected (it joins on the unique id via tkey), and routing stays correct
    // (idByName is keyed by distinct paths). Fully separating the id/name namespaces would remove the
    // glitch; it isn't worth the churn for a collision this pathological.
    function indexTarget(t) {
      const k = tkey(t), nm = t.name;
      nameByKey.set(k, nm); nameByKey.set(nm, nm);
      idByName.set(nm, k);
      const vs = vantageList(t);
      vantagesByTarget.set(k, vs); vantagesByTarget.set(nm, vs);
      probeByName.set(k, t.probe); probeByName.set(nm, t.probe);
      titleByName.set(k, t.title); titleByName.set(nm, t.title);
      ipByName.set(k, t.ip); ipByName.set(nm, t.ip);
      setNtp(t);
    }
    // canonicalizeTargetHash rewrites a #target=<display-path> route to the target's stable id once
    // the catalog is loaded (idByName seeded), so a pre-move bookmark/tab — or an Overview link that
    // predates the id-bearing DTOs — becomes a durable id link without adding a history entry or a
    // reload (CODE_REVIEW L8). No-op when the token is already an id, unknown, or its own id.
    function canonicalizeTargetHash() {
      const h = location.hash.replace(/^#/, '');
      if (!h.startsWith('target=')) return;
      const p = new URLSearchParams(h);
      const tok = p.get('target') || '';
      const id = idByName.get(tok);
      if (!id || id === tok) return; // not a known display path, or already the stable id
      const range = p.get('range');
      const canon = 'target=' + enc(id) + (range && RANGES[range] ? '&range=' + enc(range) : '');
      if (canon !== h) history.replaceState(null, '', '#' + canon);
    }
    // ensureVantageStats fetches /api/targets?vantage=<v> for each REMOTE vantage in the set and fills
    // ntpByVantage, so a detail view can show that vantage's NTP offset/stratum. Best-effort: a failed
    // fetch just leaves no stat. The local vantage's reading already arrives via the main /api/targets
    // fetch (ntpByName), so it is skipped. A short per-vantage TTL coalesces the redundant fetches a
    // detail view triggers within one refresh cadence (chip clicks, zoom transitions, re-renders):
    // each remote vantage is fetched at most once per TTL, and the 30s auto-refresh (> TTL) still
    // refreshes the stat (CODE_REVIEW efficiency finding).
    const vantageStatsAt = new Map(); // vantage -> ms of its last successful stats fetch
    const VANTAGE_STAT_TTL = 25000;   // just under the 30s detail refresh cadence
    async function ensureVantageStats(vantages) {
      await Promise.all([...new Set(vantages || [])].map(async (v) => {
        if (!v || v === 'local') return;
        const at = vantageStatsAt.get(v);
        if (at != null && (Date.now() - at) < VANTAGE_STAT_TTL) return; // reuse the recently-fetched stats
        let targets;
        try { targets = (await fetchJSON('/api/targets?vantage=' + enc(v))).targets || []; }
        catch (e) { return; }
        vantageStatsAt.set(v, Date.now());
        for (const t of targets) setNtpVantage(v, t);
      }));
    }
    // ensureVantages backfills the per-target maps for a detail view reached before refreshGrid has
    // populated them (e.g. a deep link to #target=...): a no-op once the grid has run, otherwise one
    // /api/targets fetch to seed them. `name` is accepted so the detail views can call it uniformly
    // with the target about to render, even though the seed fetches every target at once.
    async function ensureVantages(name) {
      if (vantagesByTarget.size) return;
      try {
        const targets = (await fetchJSON('/api/targets')).targets || [];
        for (const t of targets) indexTarget(t);
        canonicalizeTargetHash(); // a deep-linked #target=<path> becomes its stable id (L8)
      } catch (e) { /* transient: vantagesFor falls back to ['local'] */ }
    }
    function ensurePanel(t) {
      const key = tkey(t);
      let p = panels.get(key);
      if (p) {
        // Refresh the heading: title/ip (and probe) can change on a SIGHUP reload, so a
        // cached panel must not keep a stale label. Keep data-path current for grid scoping.
        p.el.querySelector('h2').innerHTML = '<span class="probe">' + esc(t.probe) + '</span> ' + labelHTML(t.name, t.title, t.ip);
        p.el.dataset.path = t.name;
        return p;
      }
      const grid = $('graphGrid'); if (panels.size === 0) grid.innerHTML = '';
      // data-target is the STABLE token (so a click deep-links by id, surviving a move); data-path is
      // the display path (so grid scoping, which matches on '/', still works) — CODE_REVIEW L8.
      const el = document.createElement('div'); el.className = 'panel gpanel'; el.dataset.target = key; el.dataset.path = t.name;
      el.innerHTML = '<h2><span class="probe">' + esc(t.probe) + '</span> ' + labelHTML(t.name, t.title, t.ip) + '</h2><div class="meta"></div><canvas></canvas>';
      grid.appendChild(el);
      // series is the FOCUSED vantage's series (band + median + unison + meta); seriesByV holds each
      // shown vantage's series so the others render as overlay lines (multi-vantage Graphs grid).
      p = { el, canvas: el.querySelector('canvas'), meta: el.querySelector('.meta'), series: null, seriesByV: {} };
      panels.set(key, p); return p;
    }
    // One bulk, incremental read for the whole grid: /api/series/all returns every
    // target's rounds newer than the watermark (or the full 3h window on the first
    // tick, when sinceMs is null), so a refresh is one request + one store query
    // regardless of target count — replacing the old one-fetch-per-target fan-out
    // (CODE_REVIEW #2). Response: { cutoff, targets: { name: { rounds:[...] } } }.
    async function fetchGridSeries(sinceMs, vantage) {
      const since = sinceMs != null ? '&since=' + sinceMs : '';
      // 'local' (the hub) is the default vantage — no param; a remote vantage is requested explicitly.
      const vq = (vantage && vantage !== 'local') ? '&vantage=' + enc(vantage) : '';
      const r = await fetch('/api/series/all?window=' + RANGES['3h'].window + since + vq, { cache: 'no-store' });
      if (!r.ok) return null;
      return r.json();
    }
    function gridMeta(p, s) {
      const st = Smoke.seriesStats(s); const lcls = st.lossAvg > 2 ? 'bad' : st.lossAvg > 0.5 ? 'warn' : '';
      p.meta.innerHTML = '<span class="stat"><span class="k">median</span><span class="v">' + fmtMs(st.medAvg, ntpSigned(p.el.dataset.target)) + ' ms</span></span>' +
        '<span class="stat"><span class="k">loss</span><span class="v ' + lcls + '">' + fmt(st.lossAvg, 2) + ' %</span></span>' +
        // The clock stat must follow the panel's BAND vantage (its plotted series), not the hub-local
        // reading — otherwise a remote-focused NTP panel shows the hub's offset/stratum (M2 regression).
        ntpStatsHtmlForVantage(p.el.dataset.target, p.band);
    }
    let gridBusy = false, gridLoaded = false; // gridLoaded: the first full-window fetch has landed
    async function refreshGrid() {
      if (gridBusy) return; gridBusy = true;
      try {
        let targets;
        try { targets = (await fetchJSON('/api/targets')).targets || []; }
        catch (e) { $('statusText').textContent = 'collector unreachable — showing last known'; return; } // keep panels (#2)
        $('statusText').textContent = targets.length + ' targets · updated ' + new Date().toLocaleTimeString();
        // Feed the config-tree menu: the name set it's built from and per-target dot status.
        // ALL configured targets go in the tree (so a remote-only target is navigable). A
        // remote-only target also gets a grid thumbnail once a NON-local vantage that measures it is
        // selected (the gridTargets filter below) — the grid then reads that vantage's series, so it
        // is no longer stranded in the tree/detail view alone (CODE_REVIEW M10, extending #3 / P1-3).
        statusByTarget.clear(); vantagesByTarget.clear();
        // The vantages this deployment measures from (union across targets). Reconcile the shown set
        // against it and (re)render the toolbar control — which hides itself entirely when there's
        // only one vantage, so a non-federated deployment looks exactly as before.
        availVantages = availableVantages(targets);
        const prunedV = gridVantages.filter((v) => availVantages.includes(v));
        gridVantages = prunedV.length ? prunedV : ['local'];
        renderVantageControl();
        // Status dot = the WORST across ALL vantages, not just the shown/local one — so a target that
        // is healthy on fiber but losing packets from another vantage still flags on the overview.
        // Local status comes from `targets`; each other vantage is one extra /api/targets read, merged
        // by stable id. Best-effort: a vantage that fails to answer simply doesn't contribute.
        const statById = new Map(), nameById = new Map();
        for (const t of targets) {
          indexTarget(t); const id = tkey(t); nameById.set(id, t.name);
          if (!statById.has(id)) statById.set(id, []);
          statById.get(id).push(targetStatus(t));
        }
        const otherV = availVantages.filter((v) => v !== 'local');
        if (otherV.length) {
          const lists = await Promise.all(otherV.map((v) =>
            fetchJSON('/api/targets?vantage=' + enc(v)).then((r) => r.targets || []).catch(() => null)));
          otherV.forEach((v, i) => {
            const list = lists[i];
            if (!list) return;
            for (const t of list) {
              const arr = statById.get(tkey(t)); if (arr) arr.push(targetStatus(t));
              // Reuse the list already fetched here to record this vantage's NTP clock stat, so a
              // remote-focused grid panel's offset/stratum matches its plotted series (M2). Keyed by
              // vantage, so the local reading (ntpByName) is untouched.
              setNtpVantage(v, t);
            }
          });
        }
        for (const [id, arr] of statById) { const w = worstStatus(arr); statusByTarget.set(id, w); statusByTarget.set(nameById.get(id), w); }
        canonicalizeTargetHash(); // an open #target=<path> view becomes its stable id (L8)
        treeNames = targets.map((t) => t.name);
        // Grid candidates: every target with local data (as before), PLUS a remote-only target
        // (no local row) when a NON-local vantage that measures it is currently selected — so the
        // vantage selector can surface a site the hub can't reach, not just hide it in the tree
        // (CODE_REVIEW M10). The per-panel bandVantageFor rule below still hides a candidate that
        // none of the selected vantages measures. A no-data LOCAL target stays excluded (it would be
        // an empty "collecting…" panel); only a selected remote vantage can actually fill one.
        const gridTargets = targets.filter((t) => gridShowsTarget(t, gridVantages));
        // Reconcile ONLY against an authoritative target list (the fetch above succeeded):
        // drop panels for targets no longer reported (e.g. removed on a SIGHUP reload, or a
        // target that became no-data). A failed /api/targets returned early, so a transient
        // 503 never blanks the grid (#2).
        const live = new Set(gridTargets.map((t) => tkey(t)));
        for (const [key, p] of panels) {
          if (!live.has(key)) { p.el.remove(); panels.delete(key); }
        }
        const cutoffMs = Date.now() - RANGES['3h'].windowMs;
        // Fetch each SHOWN vantage's bulk series (incrementally, with its own watermark) into
        // p.seriesByV[v]. The focused vantage becomes p.series — band + median + unison + meta — and
        // the other shown vantages render as overlay lines (renderGridPanels). A single-vantage grid
        // is one fetch, exactly as before; each vantage is still one bulk /api/series/all query.
        let anyBulk = false;
        for (const v of gridVantages) {
          // Incremental watermark = the oldest frontier among panels holding THIS vantage's data, so
          // the shared `since` never advances past a slow target (#1). null (first tick, or a
          // just-toggled-on vantage with no cached data) means fetch the whole window.
          const sinceV = gridLoaded ? gridSince([...panels.values()], (p) => p.seriesByV[v]) : null;
          let bulk = null;
          try { bulk = await fetchGridSeries(sinceV, v); } catch (e) { /* transient: keep panels */ }
          if (bulk) anyBulk = true;
          await Promise.all(gridTargets.map(async (t) => {
            const p = ensurePanel(t);
            let incoming = null;
            // /api/series/all is keyed by the stable id (not the display path), so join on the stable
            // token; a moved target (id != name) would otherwise miss its own series and render blank.
            const raw = bulk && bulk.targets && bulk.targets[tkey(t)];
            if (raw) {
              incoming = Smoke.fromApiSeries(raw);
            } else if (!gridLoaded || !p.seriesByV[v]) {
              // First load, or a panel the incremental read didn't cover: backfill this vantage's full
              // window once (the server resolves the stable token; 'local' takes the default vantage).
              try { const s = await fetchRange(tkey(t), '3h', v === 'local' ? undefined : v); if (s && !s.unsupported) incoming = s; } catch (e) { /* transient */ }
            }
            if (incoming) p.seriesByV[v] = mergeSeries(p.seriesByV[v], incoming, cutoffMs);
            else if (p.seriesByV[v]) p.seriesByV[v] = mergeSeries(p.seriesByV[v], null, cutoffMs); // age out
          }));
        }
        // The band owner (p.series) + stat line are chosen PER PANEL in renderGridPanels: each panel
        // follows the first selected vantage that measures its target, so a target a vantage doesn't
        // measure is hidden rather than drawn blank (bandVantageFor). Nothing to set here.
        if (anyBulk) gridLoaded = true;
        // Don't claim "updated" when every vantage's series fetch failed (network/non-2xx): the panels
        // are showing last-known data, so say so instead of lying (#5).
        if (!anyBulk) $('statusText').textContent = targets.length + ' targets · graph data degraded (last known) · ' + new Date().toLocaleTimeString();
        renderGridPanels();     // render the visible (scoped) panels, sharing a Y-axis
        renderTreeIfChanged();  // refresh the menu dots when a target's status changed
      } finally { gridBusy = false; }
    }
    // Render every VISIBLE grid panel — those under the current subtree scope (gridScope).
    // In unison mode the visible set shares one Y-axis max (sharedYMax) so the small
    // multiples are comparable; scoping to a subtree rescales to just that subtree.
    let unisonScale = false; // default: each panel auto-scales to its own data; the toggle shares a Y-axis
    // Graph x-axis time labels: true = absolute wall-clock (rangeLabels), false = relative (each
    // range's static xl, e.g. -3h/now). Read by renderInto (grid + detail stack) and drawZoom; a
    // drag-zoom is always absolute regardless. Server-configured (SMOKED_ABSOLUTE_TIME, default
    // true) and applied uniformly — the value arrives with the /api/version boot fetch below.
    let timeAbsolute = true;
    // Graphs-per-row: 'auto' fits as many as the min width allows; a fixed N caps columns but never
    // shrinks a graph below the minimum (wraps to fewer instead). The minimum is expressed in rem
    // (font-relative) so graphs scale with the user's text size, not a fixed pixel count; graphMinPx
    // resolves it to CSS px for the how-many-fit math. Persisted per browser.
    const GRAPH_MIN_REM = 22.5, GRAPH_GAP = 18; // 22.5rem == 360px at the default 16px root
    const graphMinPx = () => GRAPH_MIN_REM * (parseFloat(getComputedStyle(document.documentElement).fontSize) || 16);
    // Only the values the seg buttons offer are valid; a stale or hand-edited localStorage entry
    // (e.g. "5" -> a layout no button represents, "Infinity" -> invalid CSS) falls back to 'auto'.
    const GRID_COL_OPTIONS = new Set(['auto', '1', '2', '3', '4', '6']);
    let gridCols = 'auto';
    try { const s = localStorage.getItem('graphCols'); if (GRID_COL_OPTIONS.has(s)) gridCols = s; } catch (e) {}
    function applyGridCols() {
      const g = $('graphGrid'); if (g) g.style.gridTemplateColumns = gridTemplateFor(gridCols, GRAPH_MIN_REM + 'rem', GRAPH_GAP);
      updateColsPicker();
    }
    // updateColsPicker keeps the Columns picker honest at the current browser width: it hides the
    // count buttons that can't fit (clicking them does nothing — the grid just wraps to fewer), and
    // hides the picker entirely when even 2 columns won't fit, since 'Auto' is then the only layout
    // and there is no distinct choice to offer. When 2 fit, 'Auto' (== 2) and '1' (one big graph)
    // are genuinely different, so the picker stays. It also re-syncs aria-pressed so a selection
    // hidden by a resize re-presses when it fits again. No-op while the Graphs view is hidden.
    function updateColsPicker() {
      const g = $('graphGrid'), seg = $('colsSeg'), label = $('colsLabel');
      if (!g || !seg || !label) return;
      const w = g.clientWidth;
      if (!w) return;
      const maxFit = maxColumnsFor(w, graphMinPx(), GRAPH_GAP);
      const show = maxFit >= 2; // no picker unless at least 2 columns fit (Auto vs 1 is a real choice)
      seg.style.display = show ? '' : 'none';
      label.style.display = show ? '' : 'none';
      if (!show) return;
      seg.querySelectorAll('button[data-cols]').forEach((b) => {
        const c = b.dataset.cols;
        const fits = c === 'auto' || Number(c) <= maxFit;
        const selected = c === gridCols;
        // Hide counts that can't fit — EXCEPT the current selection, which stays visible and pressed
        // so the picker always shows the active choice. A fixed selection wider than fits wraps to
        // maxFit columns (auto-fill); annotate that so the control never looks unset or misleading.
        b.style.display = fits || selected ? '' : 'none';
        b.setAttribute('aria-pressed', String(selected));
        if (selected && !fits) {
          b.dataset.wraps = '1';
          const note = `wraps to ${maxFit} column${maxFit === 1 ? '' : 's'} at this width`;
          b.title = note[0].toUpperCase() + note.slice(1);
          // The dashed outline + title are visual-only; give assistive tech an accessible name that
          // states the effective (wrapped) count, so a screen reader isn't told "6" while 2 render.
          b.setAttribute('aria-label', `${c} columns — ${note}`);
        } else {
          delete b.dataset.wraps;
          b.removeAttribute('title');
          b.removeAttribute('aria-label');
        }
      });
    }
    function renderGridPanels() {
      const vis = [];
      let hidden = 0; // targets in scope that NO selected vantage measures — hidden, not drawn blank
      for (const p of panels.values()) {
        // Scope by the display PATH (data-path), not data-target — the latter is now the stable id,
        // which for a UUID target has no '/'-path relationship to the scope (CODE_REVIEW L8).
        const inScope = underPath(p.el.dataset.path, gridScope);
        // Band owner = the first SELECTED vantage that measures this target (per panel, so a target a
        // vantage doesn't measure is hidden rather than drawn blank). null => not measured by the
        // selection => hide it (it stays in the nav tree).
        const band = bandVantageFor(vantagesByTarget.get(p.el.dataset.target), gridVantages);
        const show = inScope && band != null;
        p.el.style.display = show ? '' : 'none';
        if (inScope && band == null) hidden++;
        if (show) {
          p.band = band;
          p.series = p.seriesByV[band] || null; // the band owner's series
          if (p.series && p.series.buckets.length) gridMeta(p, p.series);
          vis.push(p);
        }
      }
      updateHiddenNote(hidden);
      // Which vantages overlay on each panel: every OTHER selected vantage that measures this target
      // (not its band owner). Filter by the SHOWN set + what the target measures, but color by the
      // full availVantages so a vantage keeps its color as others are toggled. Skip series a panel
      // hasn't loaded / can't support.
      const overlaysFor = (p) => {
        if (gridVantages.length < 2) return undefined;
        const byV = p.seriesByV || {};
        const measures = new Set(vantagesByTarget.get(p.el.dataset.target) || ['local']);
        return gridVantages.filter((v) => v !== p.band && measures.has(v) && byV[v] && !byV[v].unsupported)
          .map((v) => ({ series: byV[v], color: cssVar(vantageColorVar(v, availVantages)) }));
      };
      // Unison shares one latency scale — but only across rtt panels. A signed offset panel uses
      // its own zero-centered scale and would otherwise blow up the shared max (a +5s offset =>
      // ~5000ms), flattening every real latency panel (M4). The shared max spans the focused series
      // AND every overlay series, so an overlay vantage with higher latency isn't clipped.
      let yMax;
      if (unisonScale) {
        const scaleSeries = [];
        for (const p of vis) if (!ntpSigned(p.el.dataset.target)) {
          if (p.series) scaleSeries.push(p.series);
          (overlaysFor(p) || []).forEach((o) => o && o.series && scaleSeries.push(o.series));
        }
        yMax = sharedYMax(scaleSeries);
      }
      for (const p of vis) renderInto(p.canvas, p.series, RANGES['3h'], 170, yMax, overlaysFor(p), ntpSigned(p.el.dataset.target));
      updateColsPicker(); // the grid now has a measurable width (e.g. first paint on view entry)
    }
    // updateHiddenNote reports how many in-scope targets the current vantage selection doesn't
    // measure (hidden rather than shown blank). Silent when nothing is hidden, or single-vantage.
    function updateHiddenNote(n) {
      const el = $('gridHiddenNote'); if (!el) return;
      if (n <= 0 || availVantages.length < 2) { el.hidden = true; el.textContent = ''; return; }
      el.hidden = false;
      el.textContent = n + (n === 1 ? ' target' : ' targets') + ' not measured from ' +
        orderVantages(gridVantages).join(' + ') + ' — hidden (still in the tree)';
    }
    // renderVantageControl draws the Graphs-toolbar vantage toggles. Hidden entirely for a
    // single-vantage deployment. Each chip toggles whether that vantage is drawn on every panel; the
    // focused one (first shown, marked "band") owns the band+median, the rest draw as overlay lines.
    function renderVantageControl() {
      const bar = $('gridVantageBar'); if (!bar) return;
      if (availVantages.length < 2) { bar.hidden = true; bar.innerHTML = ''; return; }
      bar.hidden = false;
      bar.innerHTML = vantageControlChips(availVantages, gridVantages, gridFocus(), MAX_GRID_VANTAGES,
        (v) => cssVar(vantageColorVar(v, availVantages)));
    }

    // ---- config-tree menu (left nav) ----
    // The nav is a pure function of (treeNames, statusByTarget, collapsed, gridScope,
    // navQuery): buildTree nests the names, each folder shows its worst-descendant dot and
    // a subtree count, each leaf its own dot. Rebuilt only when that signature changes.
    const SEV = { nodata: 0, ok: 0, degraded: 1, down: 2 };
    function countLeaves(n) { let c = n.target ? 1 : 0; for (const ch of n.children) c += countLeaves(ch); return c; }
    // A folder's dot is its worst descendant. Init 'nodata' so a folder whose leaves are all
    // no-data reads as no-data too (matching the leaf dots), not a false green; but 'ok' (a
    // healthy target WITH data) outranks 'nodata' even though both sort at severity 0.
    function folderStatus(n) {
      let worst = 'nodata';
      (function visit(m) {
        if (m.target) {
          const s = statusByTarget.get(m.target) || 'ok';
          if (worst === 'nodata' ? s !== 'nodata' : SEV[s] > SEV[worst]) worst = s;
        }
        m.children.forEach(visit);
      })(n);
      return worst;
    }
    function nodeHtml(n, depth, filtering) {
      if (n.children.length) {
        const isCollapsed = !filtering && collapsed.has(n.path);
        const sel = n.path === gridScope ? ' sel' : '';
        const kids = isCollapsed ? '' : '<div class="kids">' + n.children.map((c) => nodeHtml(c, depth + 1, filtering)).join('') + '</div>';
        return '<div class="node"><div class="row folder' + (isCollapsed ? ' collapsed' : '') + sel + '" data-path="' + esc(n.path) +
          '" style="--d:' + depth + '" tabindex="0" role="treeitem" aria-expanded="' + !isCollapsed + '">' +
          '<span class="twist" data-twist="1">▾</span><span class="tdot ' + folderStatus(n) + '"></span>' +
          '<span class="label">' + esc(n.name) + '</span><span class="tcount">' + countLeaves(n) + '</span></div>' + kids + '</div>';
      }
      // The tree is path-structured (n.target/n.path are paths), but the leaf's data-target carries
      // the stable id so a click deep-links by id and survives a move (CODE_REVIEW L8). Status is
      // looked up by path — the maps are dual-keyed, so the path key resolves.
      return '<div class="row leaf" data-target="' + esc(idByName.get(n.target) || n.target) + '" style="--d:' + depth + '" tabindex="0" role="treeitem">' +
        '<span class="twist" aria-hidden="true"></span><span class="tdot ' + (statusByTarget.get(n.target) || 'ok') + '"></span>' +
        '<span class="label">' + esc(n.name) + '</span></div>';
    }
    function treeSignature() {
      return [navQuery, gridScope, [...collapsed].sort().join('~'),
        treeNames.map((n) => n + ':' + (statusByTarget.get(n) || 'ok')).join(',')].join('|');
    }
    function renderTree() {
      const host = $('navTree'); if (!host) return;
      treeSig = treeSignature();
      const q = navQuery.trim().toLowerCase();
      // Filtering keeps only matching targets (buildTree then drops empty branches) and
      // force-expands so every match is visible regardless of collapse state.
      const names = q ? treeNames.filter((n) => n.toLowerCase().includes(q)) : treeNames;
      const nodes = buildTree(names);
      host.innerHTML = nodes.length ? nodes.map((n) => nodeHtml(n, 0, !!q)).join('')
        : '<div class="tree-empty">' + (q ? 'no matches' : 'no targets yet') + '</div>';
    }
    function renderTreeIfChanged() { if (treeSignature() !== treeSig) renderTree(); }
    // Breadcrumb above the grid mirroring the scope, each ancestor a shortcut back up.
    function renderScope() {
      const el = $('graphScope'); if (!el) return;
      if (!gridScope) { el.innerHTML = '<span class="crumb-cur">All targets</span>'; return; }
      const parts = gridScope.split('/'); let acc = '';
      const bits = ['<span class="crumb-link" data-path="">All</span>'];
      parts.forEach((seg, i) => {
        acc = acc ? acc + '/' + seg : seg;
        bits.push('<span class="crumb-sep">›</span>' + (i === parts.length - 1
          ? '<span class="crumb-cur">' + esc(seg) + '</span>'
          : '<span class="crumb-link" data-path="' + esc(acc) + '">' + esc(seg) + '</span>'));
      });
      el.innerHTML = bits.join('');
    }
    function navScope(path) { nav(path ? 'graphs&path=' + enc(path) : 'graphs'); }
    // A folder's chevron toggles collapse in place; the folder row scopes the grid (and
    // expands so its children show); a leaf opens the target's stacked drill-down.
    function activateRow(row, viaTwist) {
      if (row.classList.contains('folder')) {
        const path = row.dataset.path;
        if (viaTwist) { collapsed.has(path) ? collapsed.delete(path) : collapsed.add(path); renderTree(); return; }
        collapsed.delete(path); navScope(path);
      } else if (row.dataset.target) { nav('target=' + enc(row.dataset.target)); }
    }

    // ---- Drill-down: stack (all four) + zoom (one) ----
    let curTarget = null, curRange = null;
    // stackGen/zoomGen: per-call generation counters. curTarget/curRange are still the
    // source of truth for "what's open" (nav, click handlers, zoomReset) — but comparing an
    // in-flight resume to them by NAME can't tell "still this call" from "a newer call that
    // happens to target the same name" (A -> B -> A: the third call resets curTarget back to
    // A, so a stale first call's `curTarget !== name` check passes and it appends anyway).
    // Every renderStack/renderZoom/zoomTo call instead captures `const gen = ++stackGen` (or
    // zoomGen) BEFORE its first await; any resume point that would mutate shared DOM/state
    // checks `gen !== stackGen` (or zoomGen) — true for ANY newer call, same-name or not.
    let stackGen = 0;
    let zoomGen = 0;
    // Last successful series per `${target}|${rangeKey}` (single-vantage) or
    // `${target}|${rangeKey}|${vantage}` (multi-vantage), so a transient fetch failure on a
    // detail/zoom refresh keeps the graph instead of blanking it to "collecting…" (#5).
    const lastGood = new Map();
    // stackCanvases: one entry per open range cell. Single-vantage cell: {canvas, meta, R,
    // key, series, failed}. Multi-vantage cell: {canvas, meta, R, key, byV, failed} (no
    // `series` — the rendered series is derived from byV[stackFocus] at render time so a
    // chip click or theme toggle can re-render from cache without refetching).
    const stackCanvases = [];
    let stackVantages = [];  // ordered vantage list for the open stack (Dash.orderVantages); length<=1 => single-vantage path, no overlay/chips
    let stackFocus = null;   // focused vantage for the open stack (meaningful only when stackVantages.length>1)
    // zoomState: {canvas, series, band, t0, t1, xlabels, custom} plus, in multi-vantage mode,
    // {byV, vantages, focus} — series is always byV[focus] so the single-vantage draw path
    // (drawZoom) and its null/unsupported/too-short guards stay exactly as before.
    let zoomState = null;
    let zoomVantages = [];   // ordered vantage list for the open zoom view
    let zoomFocus = null;    // focused vantage for the open zoom view

    // renderStackCell (re)draws one range cell from its stored state — used for the initial
    // fetch-driven render, a chip click (re-render from cache, no refetch), and a theme
    // toggle / resize (rerender()); the same function for all three keeps them consistent.
    function renderStackCell(c) {
      // The focused vantage sources the meta's NTP stat (M2): the live stackFocus for a multi-vantage
      // cell, or the cell's own fetched vantage for a single-vantage one.
      const focusV = c.byV ? stackFocus : c.vantage;
      if (c.byV) {
        const focused = c.byV[stackFocus];
        const overlays = buildOverlays(c.byV, stackVantages, stackFocus);
        if (focused && !focused.unsupported && focused.buckets.length >= 2) {
          c.meta.innerHTML = metaHtml(focused, focusV) + (c.failed ? ' <span class="reslabel">· last known</span>' : '');
        } else { c.meta.innerHTML = ''; }
        renderInto(c.canvas, focused, c.R, 170, undefined, overlays, ntpSigned(curTarget));
      } else {
        const s = c.series;
        if (s && !s.unsupported && s.buckets.length >= 2) c.meta.innerHTML = metaHtml(s, focusV) + (c.failed ? ' <span class="reslabel">· last known</span>' : '');
        renderInto(c.canvas, s, c.R, 170, undefined, undefined, ntpSigned(curTarget));
      }
    }
    // renderStackChips renders (or, for a single-vantage target, clears) the #stackVantages
    // legend/selector. Chip values come from the '3h' cell (the freshest data) for each
    // vantage's last median — independent of which vantage is currently focused.
    function renderStackChips() {
      const host = $('stackVantages'); if (!host) return;
      if (stackVantages.length <= 1) { host.innerHTML = ''; return; }
      const ref = stackCanvases.find((c) => c.key === '3h' && c.byV);
      host.innerHTML = vchipsHtml(stackVantages, stackFocus, (v) => lastMedian(ref && ref.byV[v]));
    }
    async function renderStack(name) {
      const gen = ++stackGen; // captured before any await — invalidates any earlier in-flight renderStack, same name or not
      curTarget = name; stackCanvases.length = 0;
      $('stackTitle').innerHTML = probeBadge(name) + displayLabel(name);
      const grid = $('stackGrid'); grid.innerHTML = '';

      await ensureVantages(name);
      if (gen !== stackGen) return; // a newer renderStack call superseded this one — don't append its panels
      const vs = Dash.orderVantages(vantagesFor(name));
      stackVantages = vs;
      await ensureVantageStats(vs); // remote vantages' NTP stats for the meta (M2); local uses ntpByName
      if (gen !== stackGen) return;

      const cells = RANGE_ORDER.map((key) => {
        const R = RANGES[key];
        const el = document.createElement('div'); el.className = 'panel spanel'; el.dataset.range = key;
        el.innerHTML = '<div class="charts-head"><h3>' + R.label + ' <span class="reslabel">' + R.desc + '</span></h3><span class="reslabel">click to zoom ⤢</span></div>' +
          '<div class="meta"></div><canvas></canvas>';
        grid.appendChild(el);
        return { key, R, el, canvas: el.querySelector('canvas'), meta: el.querySelector('.meta') };
      });
      // Re-set with the probe badge now that ensureVantages() has seeded probeByName (covers a
      // deep link, where the grid panel isn't cached yet at the first title paint above).
      $('stackTitle').innerHTML = probeBadge(name) + displayLabel(name);

      if (vs.length <= 1) {
        // Single-vantage: no fetch fan-out, no overlays, no chips (renderStackChips() clears
        // #stackVantages via the length<=1 check). Fetch the target's OWN vantage — which may
        // be a remote one (a `vantages: [nyc]` target has no local data) — not the implicit
        // local default, else a remote-only target's graphs would be blank (CODE_REVIEW #3 / P1-3).
        renderStackChips();
        const focus = vs[0] && vs[0] !== 'local' ? vs[0] : '';
        await Promise.all(cells.map(async (c) => {
          let s = null; try { s = await fetchRange(name, c.key, focus); } catch (e) { /* transient */ }
          if (gen !== stackGen) return; // superseded mid-fetch — don't push/render into a detached/reused cell
          const k = name + '|' + c.key;
          const pick = pickSeries(s, lastGood.get(k)); lastGood.set(k, pick.cache); s = pick.series;
          const entry = { canvas: c.canvas, meta: c.meta, R: c.R, key: c.key, series: s, failed: pick.failed, vantage: focus };
          stackCanvases.push(entry);
          renderStackCell(entry);
        }));
        return;
      }

      stackFocus = Dash.keepFocus(stackFocus, vs); // preserve the user's chip across the 30s refresh
      await Promise.all(cells.map(async (c) => {
        const fetched = await Promise.all(vs.map((v) => fetchRange(name, c.key, v).catch(() => null)));
        if (gen !== stackGen) return; // superseded mid-fetch — don't push/render into a detached/reused cell
        const byV = {}; let failed = false;
        vs.forEach((v, i) => {
          const k = name + '|' + c.key + '|' + v;
          const pick = pickSeries(fetched[i], lastGood.get(k)); lastGood.set(k, pick.cache);
          byV[v] = pick.series;
          if (pick.failed) failed = true;
        });
        const entry = { canvas: c.canvas, meta: c.meta, R: c.R, key: c.key, byV, failed };
        stackCanvases.push(entry);
        renderStackCell(entry);
      }));
      if (gen !== stackGen) return; // a newer renderStack call superseded this one — don't render its chips
      renderStackChips();
    }
    // drawZoom renders the current zoomState onto the zoom canvas with its explicit
    // wall-clock domain [t0,t1] (a tier default, or a dragged sub-range). In multi-vantage
    // mode zoomState.series is already byV[focus], so the null/unsupported/too-short guards
    // below are unchanged from the single-vantage path — only the extra overlays differ.
    function drawZoom() {
      const z = zoomState; if (!z) return;
      if (z.series && z.series.unsupported) { drawNote(z.canvas, 'needs the TimescaleDB store (-dsn -downsample)', 360); return; }
      if (!z.series || !z.series.buckets || z.series.buckets.length < 2) { drawNote(z.canvas, collectingNote(z.band ? 'band' : 'raw', z.res), 360); return; }
      const overlays = z.byV ? buildOverlays(z.byV, z.vantages, z.focus) : undefined;
      // A custom drag-zoom range always shows absolute times (z.xlabels is already rangeLabels).
      // A fixed range honors the "absolute time" toggle, same as the grid/stack via renderInto.
      const xlabels = !z.custom && timeAbsolute && z.t0 != null ? rangeLabels(z.t0, z.t1) : z.xlabels;
      Smoke.render(z.canvas, z.series, { height: 360, band: z.band, xlabels, t0: z.t0, t1: z.t1, overlays, signed: ntpSigned(curTarget) });
    }
    // renderZoomChips renders (or clears) the #zoomVantages legend/selector from the
    // currently-open zoomState's byV (no refetch — mirrors renderStackChips).
    function renderZoomChips() {
      const host = $('zoomVantages'); if (!host) return;
      if (zoomVantages.length <= 1) { host.innerHTML = ''; return; }
      const byV = zoomState && zoomState.byV;
      host.innerHTML = vchipsHtml(zoomVantages, zoomFocus, (v) => lastMedian(byV && byV[v]));
    }
    // Four axis labels for an arbitrary [t0,t1]: clock times for short spans, dates for long.
    // rangeLabels is defined at module scope (shared with the grid/stack absolute-time toggle).
    async function renderZoom(name, range) {
      const gen = ++zoomGen; // captured before any await — invalidates any earlier in-flight zoom call (renderZoom or zoomTo), same name/range or not
      curTarget = name; curRange = range; const R = RANGES[range];
      $('zoomTitle').innerHTML = probeBadge(name) + displayLabel(name) + ' <span class="reslabel">· ' + R.label + '</span>';
      $('zoomMeta').innerHTML = ''; $('zoomReset').hidden = true;
      $('zoomRes').textContent = 'drag on the graph to zoom into a time range';
      const canvas = $('zoomCanvas');

      await ensureVantages(name);
      if (gen !== zoomGen) return; // a newer zoom call superseded this one
      // Re-set the title now that ensureVantages() has seeded the display maps — a direct
      // zoom deep link renders above before they were populated, so it would show the bare
      // name (matching renderStack's two-phase title set).
      $('zoomTitle').innerHTML = probeBadge(name) + displayLabel(name) + ' <span class="reslabel">· ' + R.label + '</span>';
      const vs = Dash.orderVantages(vantagesFor(name));
      zoomVantages = vs;
      await ensureVantageStats(vs); // remote vantages' NTP stats for the meta (M2); local uses ntpByName
      if (gen !== zoomGen) return;

      if (vs.length <= 1) {
        // Single-vantage: no overlays, no chips. Fetch the target's own vantage (may be a
        // remote one for a remote-only target), not the implicit local default (P1-3).
        renderZoomChips();
        const focus = vs[0] && vs[0] !== 'local' ? vs[0] : '';
        let s = null; try { s = await fetchRange(name, range, focus); } catch (e) { /* transient */ }
        if (gen !== zoomGen) return; // a newer zoom call superseded this one
        const k = name + '|' + range;
        const pick = pickSeries(s, lastGood.get(k)); lastGood.set(k, pick.cache); s = pick.series; // keep last-good on a transient failure (#5)
        // Fixed tier domain [now-windowMs, now], matching the stacked detail view.
        const lastT = s && s.buckets && s.buckets.length ? s.buckets[s.buckets.length - 1].t : NaN;
        const t1 = Math.max(Date.now(), Number.isFinite(lastT) ? lastT : 0);
        zoomState = { canvas, series: s, band: R.mode === 'band', res: R.res, t0: t1 - R.windowMs, t1, xlabels: R.xl, custom: false };
        if (s && !s.unsupported && s.buckets.length >= 2) {
          $('zoomMeta').innerHTML = metaHtml(s, focus);
          $('zoomRes').textContent = 'resolution: ' + R.desc + ' · ' + s.buckets.length + (R.mode === 'raw' ? ' rounds' : ' buckets') + (pick.failed ? ' · last known (refresh failed)' : ' · drag to zoom');
        }
        drawZoom();
        return;
      }

      zoomFocus = Dash.keepFocus(zoomFocus, vs); // preserve the user's chip across the 30s refresh
      const fetched = await Promise.all(vs.map((v) => fetchRange(name, range, v).catch(() => null)));
      if (gen !== zoomGen) return; // a newer zoom call superseded this one
      const byV = {}; let failed = false;
      vs.forEach((v, i) => {
        const k = name + '|' + range + '|' + v;
        const pick = pickSeries(fetched[i], lastGood.get(k)); lastGood.set(k, pick.cache);
        byV[v] = pick.series;
        if (pick.failed) failed = true;
      });
      const s = byV[zoomFocus];
      // Fixed tier domain [now-windowMs, now], anchored to the focused vantage's data.
      const lastT = s && s.buckets && s.buckets.length ? s.buckets[s.buckets.length - 1].t : NaN;
      const t1 = Math.max(Date.now(), Number.isFinite(lastT) ? lastT : 0);
      zoomState = { canvas, series: s, band: R.mode === 'band', res: R.res, t0: t1 - R.windowMs, t1, xlabels: R.xl, custom: false, byV, vantages: vs, focus: zoomFocus };
      if (s && !s.unsupported && s.buckets.length >= 2) {
        $('zoomMeta').innerHTML = metaHtml(s, zoomFocus);
        $('zoomRes').textContent = 'resolution: ' + R.desc + ' · ' + s.buckets.length + (R.mode === 'raw' ? ' rounds' : ' buckets') + (failed ? ' · last known (refresh failed)' : ' · drag to zoom');
      }
      drawZoom();
      renderZoomChips();
    }
    // fetchCustomRange fetches one vantage's series for an arbitrary dragged [fromMs,toMs]
    // span at resolution `zr` (zoomResolution's pick) — the zoomTo counterpart of fetchRange,
    // which only knows the fixed range tiers. Returns null on any fetch/parse failure.
    async function fetchCustomRange(name, zr, fromMs, toMs, vantage) {
      const vq = vantage ? '&vantage=' + enc(vantage) : '';
      const qs = '&from=' + Math.round(fromMs) + '&to=' + Math.round(toMs) + vq;
      try {
        if (zr.mode === 'raw') {
          const r = await fetch('/api/series?target=' + enc(name) + qs, { cache: 'no-store' });
          return r.ok ? Smoke.fromApiSeries(await r.json()) : null;
        }
        const r = await fetch('/api/rollup?target=' + enc(name) + '&res=' + zr.res + qs, { cache: 'no-store' });
        if (r.status === 501) return { unsupported: true };
        return r.ok ? Smoke.fromApiRollup(await r.json()) : null;
      } catch (e) { return null; }
    }
    // zoomTo refetches an arbitrary dragged sub-range [fromMs,toMs] at the resolution best
    // for its span, then renders it (refetch, not image swap). A reset restores the tier.
    // Multi-vantage: refetches ALL vantages for the new span and keeps the current focus.
    async function zoomTo(fromMs, toMs) {
      const gen = ++zoomGen; // captured before any await — a drag also invalidates any earlier in-flight zoom call (renderZoom or a prior zoomTo)
      const name = curTarget; if (!name) return;
      const zr = zoomResolution(toMs - fromMs);
      const canvas = $('zoomCanvas');
      $('zoomRes').textContent = 'loading zoom…';
      const vs = zoomVantages.length ? zoomVantages : ['local'];
      let s, byV;
      if (vs.length <= 1) {
        // Fetch the single vantage's own data — remote for a remote-only target (P1-3).
        const focus = vs[0] && vs[0] !== 'local' ? vs[0] : '';
        s = await fetchCustomRange(name, zr, fromMs, toMs, focus);
      } else {
        const fetched = await Promise.all(vs.map((v) => fetchCustomRange(name, zr, fromMs, toMs, v)));
        if (gen !== zoomGen) return; // superseded mid-fetch — don't touch the shared zoomFocus
        byV = {};
        vs.forEach((v, i) => { byV[v] = fetched[i]; });
        zoomFocus = Dash.keepFocus(zoomFocus, vs);
        s = byV[zoomFocus];
      }
      if (gen !== zoomGen) return; // a newer zoom call superseded this drag (also covers the single-vantage branch's await)
      zoomState = {
        canvas, series: s, band: zr.mode === 'band', res: zr.res, t0: fromMs, t1: toMs, xlabels: rangeLabels(fromMs, toMs), custom: true,
        byV, vantages: vs.length > 1 ? vs : undefined, focus: vs.length > 1 ? zoomFocus : undefined,
      };
      $('zoomReset').hidden = false;
      const desc = zr.mode === 'raw' ? 'per-round' : (zr.res === '1h' ? 'hourly band' : 'daily band');
      const focusV = vs.length > 1 ? zoomFocus : (vs[0] && vs[0] !== 'local' ? vs[0] : '');
      if (s && !s.unsupported && s.buckets && s.buckets.length >= 2) {
        $('zoomMeta').innerHTML = metaHtml(s, focusV);
        $('zoomRes').textContent = 'zoomed · ' + desc + ' · ' + s.buckets.length + (zr.mode === 'raw' ? ' rounds' : ' buckets') + ' · reset to exit';
      } else if (s && s.unsupported) {
        $('zoomMeta').innerHTML = ''; $('zoomRes').textContent = 'zoom to this resolution needs the TimescaleDB store';
      } else {
        $('zoomMeta').innerHTML = ''; $('zoomRes').textContent = 'no data in the selected range · reset to exit';
      }
      drawZoom();
      if (vs.length > 1) renderZoomChips();
    }

    // ---- Vantages admin panel (federation): thin client over /api/admin/*. The first
    // GET decides the mode (disabled / login / list / error) via Dash.adminMode. ----
    const vadmin = { rows: [] };
    function vShow(id) {
      for (const s of ['vantDisabled', 'vantLogin', 'vantList', 'vantError']) $(s).classList.toggle('hidden', s !== id);
    }
    function renderVantageRows() {
      const now = Date.now();
      if (!vadmin.rows.length) {
        $('vantRows').innerHTML = '<tr><td colspan="5" class="vadmin-empty">No vantages yet — add one below.</td></tr>';
        return;
      }
      $('vantRows').innerHTML = vadmin.rows.map((v) => {
        const nm = esc(v.name);
        const created = v.created ? new Date(v.created).toLocaleDateString([], { year: 'numeric', month: 'short', day: 'numeric' }) : '—';
        const seen = Dash.relTime(v.last_seen, now);
        const tgts = (v.targets == null) ? '—' : String(v.targets);
        return '<tr><td>' + nm + '</td><td>' + created + '</td><td>' + seen + '</td><td>' + tgts +
          '</td><td style="text-align:right; white-space:nowrap">' +
          '<button class="vadmin-btn" data-regen="' + nm + '">Regenerate</button>' +
          '<button class="vadmin-btn" data-revoke="' + nm + '">Revoke</button></td></tr>';
      }).join('');
    }
    async function renderVantages() {
      refreshAdminState(); // keep the edit-affordance gate (body.admin-can-edit) current on tab entry
      let r;
      try { r = await fetch('/api/admin/vantages', { cache: 'no-store' }); }
      catch (e) { vShow('vantError'); return; }
      const mode = Dash.adminMode(r.status);
      if (mode === 'disabled') { vShow('vantDisabled'); return; }
      if (mode === 'error') { vShow('vantError'); return; }
      // Auth is handled by the top-bar Log in control; keep the bar in sync and point there.
      if (mode === 'login') { refreshAdminState(); vShow('vantLogin'); return; }
      // mode === 'list'
      let data;
      try { data = await r.json(); } catch (e) { vShow('vantError'); return; }
      vadmin.rows = data.vantages || [];
      renderVantageRows();
      vShow('vantList');
    }
    $('vantRetry').addEventListener('click', () => renderVantages());
    function reportMintError(isRegen, msg) {
      if (isRegen) window.alert('Regenerate failed: ' + msg);
      else { $('vantAddNote').textContent = ''; $('vantAddErr').textContent = msg; }
    }
    // reportMintSuccess mirrors reportMintError's isRegen split: regenerate is a table-row
    // action (no dedicated status line nearby) so it gets an alert; add gets the inline note
    // beside the form so it doesn't interrupt the flow of adding several vantages in a row.
    function reportMintSuccess(isRegen, filename) {
      const msg = 'Downloaded ' + filename + ' — run `docker compose up -d` in the extracted folder.';
      if (isRegen) window.alert(msg);
      else $('vantAddNote').textContent = msg;
    }
    // mintVantage POSTs a name; the store registers it (no-op if already registered) and always
    // issues a FRESH client certificate for it (regenerate == re-POST the same name). There is no
    // CRL and no per-certificate revocation: the hub authorizes purely by the presented
    // certificate's CommonName against the active vantage registry (requireAgent), so a
    // regenerate does NOT invalidate any certificate issued earlier for the same name — both
    // remain valid until the vantage itself is revoked (removed from the registry). The hub mints
    // the vantage's mTLS client identity server-side and, via `?format=bundle` +
    // `Accept: application/gzip`, hands back a ready-to-run tar.gz (agent.yaml +
    // docker-compose.yml + README) instead of the old copy-paste key reveal — there is no
    // client-side key material to build files from anymore, the server already assembled them.
    async function mintVantage(name, isRegen) {
      let r;
      try {
        r = await fetch('/api/admin/vantages?format=bundle', {
          method: 'POST',
          headers: { 'Content-Type': 'application/json', 'Accept': 'application/gzip' },
          body: JSON.stringify({ name }),
        });
      } catch (err) { reportMintError(isRegen, 'Network error.'); return; }
      if (r.status === 401) { renderVantages(); return; } // session expired -> back to login
      if (!r.ok) {
        let msg = 'HTTP ' + r.status;
        try { msg = (await r.json()).error || msg; } catch (e) { /* keep default */ }
        reportMintError(isRegen, msg);
        return;
      }
      let blob;
      try { blob = await r.blob(); } catch (e) { reportMintError(isRegen, 'Malformed server response.'); return; }
      const filename = Dash.vantageBundleFilename(name);
      const url = URL.createObjectURL(blob);
      const a = document.createElement('a');
      a.href = url; a.download = filename;
      document.body.appendChild(a); a.click(); a.remove();
      URL.revokeObjectURL(url);
      $('vantName').value = '';
      $('vantAddErr').textContent = '';
      reportMintSuccess(isRegen, filename);
      renderVantages(); // refresh the list (new/rotated row, updated counts)
    }
    $('vantAdd').addEventListener('submit', (e) => {
      e.preventDefault();
      $('vantAddErr').textContent = '';
      $('vantAddNote').textContent = '';
      const name = $('vantName').value.trim();
      if (!name) { $('vantAddErr').textContent = 'Name required.'; return; }
      if (!/^[A-Za-z0-9._-]+$/.test(name)) { $('vantAddErr').textContent = 'Use letters, digits, . _ - only.'; return; }
      // "local" is the hub's own vantage — the backend always rejects it (409). Block it here with
      // the reason rather than a misleading "proceed anyway" confirm that just leads to an error
      // (CODE_REVIEW L5).
      if (name === 'local') { $('vantAddErr').textContent = '"local" is the hub’s own vantage and can’t be used for an agent key.'; return; }
      mintVantage(name, false);
    });
    $('vantRows').addEventListener('click', async (e) => {
      const regen = e.target.closest('[data-regen]');
      if (regen) {
        const name = regen.getAttribute('data-regen');
        if (window.confirm('Issue a fresh certificate bundle for "' + name + '"? The current certificate keeps working until you revoke and re-add the vantage.')) {
          mintVantage(name, true);
        }
        return;
      }
      const revoke = e.target.closest('[data-revoke]');
      if (revoke) {
        const name = revoke.getAttribute('data-revoke');
        if (!window.confirm('Revoke "' + name + '"? Its agent will no longer be able to submit results.')) return;
        let r;
        try { r = await fetch('/api/admin/vantages/' + enc(name), { method: 'DELETE' }); }
        catch (err) { window.alert('Network error revoking.'); return; }
        if (r.status === 401) { renderVantages(); return; }
        renderVantages(); // 200 removed or 404 already-gone -> refresh either way
      }
    });

    // ---- Config admin panel (DB-backed targets): thin client over /api/admin/config.
    // Mirrors the Vantages panel above — the first GET decides the mode (disabled /
    // login / list / error) via Dash.adminMode. ----
    let cfg = { version: 0, doc: { targets: { children: {} } } };
    // The full node path being edited (e.g. "Web/b"), set by openCfgModal('edit', path) and
    // read back on submit — the Name field only ever shows/edits the leaf segment.
    let cfgEditPath = null;
    // The destination parent path for an ADD ('' = top level); set by openCfgModal('add', parent)
    // so "+ Add target" adds at the top and a folder's "+" adds INTO that folder (Dash.addNodeAtPath).
    let cfgAddParent = '';
    // Config-tree interaction state (the WAI-ARIA tree pattern layered on #cfgTree):
    //   cfgCollapsed — set of collapsed folder paths (Left/Right + the twist chevron toggle it).
    //   cfgFocusPath — the single roving-tabindex / aria-selected node; Up/Down/Home/End move it.
    // Both are pure view state; every structural decision is delegated to the tested Dash.* helpers
    // (cfgVisibleRows / cfgTreeKey / moveInList / moveNode), keeping this DOM layer thin.
    const cfgCollapsed = new Set();
    let cfgFocusPath = null;
    function cShow(id) {
      for (const s of ['cfgDisabled', 'cfgLogin', 'cfgList', 'cfgError']) $(s).classList.toggle('hidden', s !== id);
    }
    // findCfgNode looks up a Dash.cfgTree(cfg.doc) entry by its full path, for prefilling the
    // edit modal (folders included — depth-first search of the already-built tree).
    function findCfgNode(path) {
      const walk = (nodes) => {
        for (const n of nodes) {
          if (n.path === path) return n;
          if (n.children.length) { const f = walk(n.children); if (f) return f; }
        }
        return null;
      };
      return walk(Dash.cfgTree(cfg.doc));
    }
    // cfgRowHtml renders one node of Dash.cfgTree(cfg.doc) recursively as a WAI-ARIA treeitem:
    // a collapse twist (folders) + drag handle, the name (folders get a trailing "/"), a
    // probe/host meta line for leaves, a folder-only "+" (add INTO this folder), and path-aware
    // Edit/Remove. `--d` drives the CSS indent; a folder nests its kids in a role="group" .kids
    // box, omitted when collapsed. Exactly the cfgFocusPath row gets tabindex 0 + aria-selected
    // (roving tabindex); folders carry aria-expanded reflecting cfgCollapsed.
    function cfgRowHtml(n, depth) {
      const d = depth || 0;
      const collapsed = n.isFolder && cfgCollapsed.has(n.path);
      let meta = '';
      if (!n.isFolder) {
        const nd = n.node;
        const paramsStr = nd.params ? Object.entries(nd.params).map(([k, v]) => k + '=' + v).join(' ') : '';
        const vantStr = (nd.vantages && nd.vantages.length) ? '@' + nd.vantages.join(',') : '';
        meta = esc([nd.probe || '', nd.host || '', paramsStr, vantStr].filter(Boolean).join(' · '));
      }
      // A stable, unique id for the child group so the folder treeitem can aria-owns it — the group
      // is a DOM sibling of the folder row (nesting it inside .crow would break the flex layout), so
      // aria-owns is what conveys the folder→children ownership the WAI-ARIA tree pattern expects.
      // encodeURIComponent keeps the id unique per path and free of whitespace.
      const gid = n.isFolder ? 'cfg-grp-' + encodeURIComponent(n.path) : '';
      const kids = (n.isFolder && !collapsed)
        ? '<div class="kids" role="group" id="' + esc(gid) + '">' + n.children.map((c) => cfgRowHtml(c, d + 1)).join('') + '</div>' : '';
      const roving = n.path === cfgFocusPath ? '0' : '-1';
      const selected = n.path === cfgFocusPath ? 'true' : 'false';
      const expanded = n.isFolder ? ' aria-expanded="' + (!collapsed) + '"' : '';
      const owns = (n.isFolder && !collapsed) ? ' aria-owns="' + esc(gid) + '"' : '';
      const twist = n.isFolder ? '<span class="ctwist" data-twist="1" aria-hidden="true">▾</span>' : '<span class="ctwist" aria-hidden="true"></span>';
      const addChild = n.isFolder
        ? '<button type="button" class="vadmin-btn cfg-addchild" data-add-child="' + esc(n.path) + '" aria-label="Add a target into ' + esc(n.path) + '" title="Add into this folder">+</button>' : '';
      return '<div class="crow' + (n.isFolder ? ' folder' : '') + (collapsed ? ' collapsed' : '') + '" draggable="true" data-path="' + esc(n.path) +
        '" style="--d:' + d + '" role="treeitem" tabindex="' + roving + '" aria-selected="' + selected + '"' + expanded + owns + '>' +
        twist +
        '<span class="chandle" aria-hidden="true">⠿</span>' +
        '<span class="cname">' + esc(n.name) + (n.isFolder ? '/' : '') + '</span>' +
        '<span class="cmeta">' + meta + '</span>' +
        addChild +
        '<button type="button" class="vadmin-btn" data-edit="' + esc(n.path) + '">Edit</button>' +
        '<button type="button" class="vadmin-btn" data-remove="' + esc(n.path) + '">Remove</button>' +
      '</div>' + kids;
    }
    function renderCfgTree() {
      const tree = Dash.cfgTree(cfg.doc);
      // Prune collapse state for folders that no longer exist: a removed / renamed / moved node
      // otherwise leaves its old path in cfgCollapsed forever, and a future folder that reuses the
      // path would silently start collapsed. Keep every path that is still a folder, visible or not.
      const folderPaths = new Set();
      (function walk(ns) { for (const n of ns) if (n.isFolder) { folderPaths.add(n.path); walk(n.children); } })(tree);
      for (const p of Array.from(cfgCollapsed)) if (!folderPaths.has(p)) cfgCollapsed.delete(p);
      $('cfgVersion').textContent = 'v' + cfg.version;
      if (!tree.length) { cfgFocusPath = null; $('cfgTree').innerHTML = '<div class="tree-empty">' + (cfg.readonly ? 'No targets configured.' : 'No DB targets yet — add one.') + '</div>'; return; }
      // Keep the roving-focus anchor on a still-visible row; a save/collapse can retire the old
      // one (removed node, or hidden under a now-collapsed parent) — fall back to the first row.
      const rows = Dash.cfgVisibleRows(tree, cfgCollapsed);
      const visible = new Set(rows.map((r) => r.path));
      if (!cfgFocusPath || !visible.has(cfgFocusPath)) cfgFocusPath = rows[0].path;
      $('cfgTree').innerHTML = tree.map((n) => cfgRowHtml(n, 0)).join('');
    }
    // parentOf / cfgSiblingNames: the (weight,name)-sorted sibling order of a parent path
    // ('' = top level), read fresh from Dash.cfgTree each time — shared by keyboard reorder,
    // the twist toggle, and drag-and-drop below.
    const cfgParentOf = (p) => p.split('/').slice(0, -1).join('/');
    function cfgSiblingNames(parent) {
      const top = Dash.cfgTree(cfg.doc);
      if (!parent) return top.map((n) => n.name);
      const grp = findCfgNode(parent);
      return grp ? grp.children.map((n) => n.name) : [];
    }
    // applyRoving updates the roving tabindex + aria-selected in place (no re-render) — used
    // when focus moves between existing rows (Up/Down, click) without a structural change.
    function applyRoving(path) {
      for (const el of $('cfgTree').querySelectorAll('.crow')) {
        const on = el.getAttribute('data-path') === path;
        el.tabIndex = on ? 0 : -1;
        el.setAttribute('aria-selected', on ? 'true' : 'false');
      }
    }
    // focusCfgRow moves DOM focus (and thus roving state, via focusin) to a row by path.
    function focusCfgRow(path) {
      cfgFocusPath = path;
      for (const el of $('cfgTree').querySelectorAll('.crow')) {
        if (el.getAttribute('data-path') === path) { el.focus(); return; }
      }
    }
    async function renderConfig() {
      refreshAdminState(); // keep the edit-affordance gate (body.admin-can-edit) current on tab entry
      let r;
      try { r = await fetch('/api/admin/config', { cache: 'no-store' }); }
      catch (e) { cShow('cfgError'); return; }
      const mode = Dash.adminMode(r.status);
      if (mode === 'disabled') { cShow('cfgDisabled'); return; }
      if (mode === 'error') { cShow('cfgError'); return; }
      // Auth is handled by the top-bar Log in control; keep the bar in sync and point there.
      if (mode === 'login') { refreshAdminState(); cShow('cfgLogin'); return; }
      // The probe above only resolves the disabled/login/list state; applyCfgView loads the actual
      // data for whichever source+view is active (Effective tree by default).
      cShow('cfgList');
      applyCfgView();
    }
    $('cfgRetry').addEventListener('click', () => renderConfig());

    // ---- Config source (Effective|DB) + view (Tree|YAML), both applying to the whole tab.
    // Effective = the file+DB merged config the collector runs, read-only (tree edit controls hidden);
    // DB = the editable stored fragment. Default is Effective so the tab opens on the running config.
    // The reads are open (the config holds no secrets); every write endpoint stays admin-gated. ----
    let cfgView = 'tree';      // 'tree' | 'yaml'
    let cfgSrc = 'effective';  // 'effective' (read-only merged) | 'db' (editable fragment)
    function segPress(segId, key, val) {
      for (const b of $(segId).querySelectorAll('button')) b.setAttribute('aria-pressed', String(b.dataset[key] === val));
    }
    function applyCfgView() {
      segPress('cfgViewSeg', 'cfgview', cfgView);
      segPress('cfgSrcSeg', 'cfgsrc', cfgSrc);
      const yaml = cfgView === 'yaml';
      const db = cfgSrc === 'db';
      $('cfgYaml').classList.toggle('hidden', !yaml);
      $('cfgTree').classList.toggle('hidden', yaml);
      $('cfgTreeActions').classList.toggle('hidden', yaml || !db); // add/import: editable DB tree only
      $('cfgLabel').textContent = db ? 'DB targets' : 'Effective config';
      $('cfgVersion').classList.toggle('hidden', !db);             // the version applies to the DB fragment
      if (yaml) loadCfgYaml(); else loadCfgTree();
    }
    // loadCfgTree renders the target tree for the active source. Effective is read-only — the .readonly
    // class hides every edit affordance and the drag/click handlers bail; DB is the editable fragment.
    let cfgTreeReq = 0;
    async function loadCfgTree() {
      const req = ++cfgTreeReq;
      const source = cfgSrc; // pin the requested source: a switch mid-flight (even via the YAML view,
      // which doesn't bump cfgTreeReq) must invalidate this response, never leave effective data editable.
      const url = source === 'effective' ? '/api/admin/config?source=effective' : '/api/admin/config';
      const current = () => req === cfgTreeReq && source === cfgSrc;
      const fail = (m) => { if (current()) { cfg.doc = { targets: { children: {} } }; cfg.readonly = true; $('cfgTree').innerHTML = '<div class="tree-empty">' + m + '</div>'; } };
      let r;
      try { r = await fetch(url, { cache: 'no-store' }); }
      catch (e) { fail("Couldn't reach the admin API."); return; }
      if (!current()) return;
      if (!r.ok) { fail('Could not load the ' + source + ' config (HTTP ' + r.status + ').'); return; }
      let data; try { data = await r.json(); } catch (e) { fail('Could not load the ' + source + ' config.'); return; }
      if (!current()) return;
      cfg.readonly = data.readonly === true; // trust the response, not the (mutable) current source
      cfg.version = data.version || 0;
      cfg.doc = (data.doc && typeof data.doc === 'object') ? data.doc : { targets: { children: {} } };
      $('cfgTree').classList.toggle('readonly', cfg.readonly);
      renderCfgTree();
    }
    let cfgYamlReq = 0; // generation guard: a slower superseded fetch must never overwrite the newer source
    async function loadCfgYaml() {
      const req = ++cfgYamlReq;
      const stale = () => req !== cfgYamlReq;
      const pre = $('cfgYaml');
      const msg = (t) => { if (!stale()) { pre.classList.add('cfg-yaml-msg'); pre.textContent = t; } };
      pre.classList.remove('cfg-yaml-msg');
      pre.textContent = 'Loading…';
      let r;
      try { r = await fetch('/api/admin/config.yaml?source=' + cfgSrc, { cache: 'no-store' }); }
      catch (e) { msg("Couldn't reach the admin API."); return; }
      if (stale()) return;
      if (!r.ok) { msg('Could not load the ' + cfgSrc + ' config (HTTP ' + r.status + ').'); return; }
      const text = await r.text();
      if (!stale()) pre.textContent = text;
    }
    $('cfgViewSeg').addEventListener('click', (e) => {
      const b = e.target.closest('button[data-cfgview]'); if (!b) return;
      cfgView = b.dataset.cfgview; applyCfgView();
    });
    $('cfgSrcSeg').addEventListener('click', (e) => {
      const b = e.target.closest('button[data-cfgsrc]'); if (!b) return;
      cfgSrc = b.dataset.cfgsrc; applyCfgView();
    });
    // ---- Top-bar admin auth: one shared login modal + a session probe driving the bar's
    // Log in / Admin·Log out / hidden(disabled) state on every tab. The session endpoint is read
    // by raw status (204 authed, 401 logged out, 404 admin disabled), not Dash.adminMode. ----
    function setAdminBar(state) { // 'in' | 'out' | 'disabled' (disabled hides both controls)
      $('adminAcct').classList.toggle('hidden', state !== 'in');
      $('adminLoginBtn').classList.toggle('hidden', state !== 'out');
      // Gate every edit affordance on the Vantages/Config tabs by a single body class: the read-only
      // views render for everyone (the GETs are open), and the add/edit/remove/save/import controls +
      // drag are shown only while an admin session is active. The backend still enforces every write,
      // so this is UX, not the security boundary.
      document.body.classList.toggle('admin-can-edit', state === 'in');
      adminEditor = state === 'in';
    }
    let adminEditor = false; // mirrors the body class; read by the drag handlers (attribute, not CSS)
    const adminState = Dash.createAdminStateController(setAdminBar);
    async function refreshAdminState() {
      const gen = adminState.beginProbe();
      let st;
      try { st = (await fetch('/api/admin/session', { cache: 'no-store' })).status; }
      catch (e) { return 0; } // unknown: preserve the last confirmed state
      // A newer probe/login/logout invalidates this result for callers as well as for the bar.
      // Returning its stale 204/401 would let the login flow act on a state the controller rejected.
      return adminState.resolveProbe(gen, st) ? st : 0;
    }
    function rerenderActiveAdminTab() {
      if (!$('viewConfig').classList.contains('hidden')) renderConfig();
      else if (!$('viewVantages').classList.contains('hidden')) renderVantages();
    }
    function openAdminLogin() { $('adminLoginErr').textContent = ''; $('adminPass').value = ''; $('adminLoginModal').classList.remove('hidden'); $('adminPass').focus(); }
    function closeAdminLogin() { $('adminLoginModal').classList.add('hidden'); $('adminPass').value = ''; $('adminLoginErr').textContent = ''; }
    $('adminLoginBtn').addEventListener('click', openAdminLogin);
    $('adminLoginCancel').addEventListener('click', closeAdminLogin);
    $('adminLoginModal').addEventListener('click', (e) => { if (e.target === $('adminLoginModal')) closeAdminLogin(); });
    $('adminLoginForm').addEventListener('submit', async (e) => {
      e.preventDefault();
      $('adminLoginErr').textContent = '';
      const pass = $('adminPass').value;
      $('adminPass').value = ''; // don't leave the password in the field
      let lr;
      try { lr = await fetch('/api/admin/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password: pass }) }); }
      catch (err) { $('adminLoginErr').textContent = 'Network error.'; return; }
      if (lr.status === 401) { $('adminLoginErr').textContent = 'Invalid password.'; return; }
      if (lr.status !== 204) { $('adminLoginErr').textContent = 'Login failed (HTTP ' + lr.status + ').'; return; }
      // Secure-cookie probe: on plain-HTTP LAN the Secure cookie won't stick, so a 204 login is
      // followed by a 401 session — surface that rather than silently staying logged out.
      const sessionStatus = await refreshAdminState();
      if (sessionStatus === 401) {
        $('adminLoginErr').textContent = "Login didn't persist — the admin session needs a secure context (HTTPS via the proxy, or localhost). You are on " + location.origin + '.';
        return;
      }
      if (sessionStatus !== 204) { $('adminLoginErr').textContent = 'Signed in, but the session could not be verified. Retry when the collector is reachable.'; return; }
      closeAdminLogin();
      rerenderActiveAdminTab();
    });
    $('adminLogoutBtn').addEventListener('click', async () => {
      let lr;
      try { lr = await fetch('/api/admin/logout', { method: 'POST' }); }
      catch (e) { window.alert('Log out failed — the admin session may still be active.'); return; }
      if (!adminState.confirmLogout(lr.status)) { window.alert('Log out failed (HTTP ' + lr.status + ') — the admin session may still be active.'); return; }
      rerenderActiveAdminTab();
    });
    refreshAdminState(); // set the bar's admin control on load, independent of the current tab

    // Probe kinds for the modal's dropdown (fetched once, lazily).
    let cfgProbeKinds = null;
    async function ensureProbeKinds() {
      if (cfgProbeKinds && cfgProbeKinds.length) return cfgProbeKinds;
      try {
        const r = await fetch('/api/probes', { cache: 'no-store' });
        const d = await r.json();
        cfgProbeKinds = Array.isArray(d) ? d.map((p) => (typeof p === 'string' ? p : p.name)).filter(Boolean)
          : (d && Array.isArray(d.probes) ? d.probes.map((p) => (typeof p === 'string' ? p : p.name || p.kind)).filter(Boolean) : []);
      } catch (e) { cfgProbeKinds = []; }
      return cfgProbeKinds;
    }
    function cfgParamRow(k, v) {
      const row = document.createElement('div');
      row.className = 'vadmin-row';
      row.innerHTML = '<input class="vadmin-input cfg-pk" type="text" placeholder="key" style="max-width:140px"> ' +
        '<input class="vadmin-input cfg-pv" type="text" placeholder="value"> ' +
        '<button type="button" class="vadmin-btn cfg-pdel">×</button>';
      row.querySelector('.cfg-pk').value = k || '';
      row.querySelector('.cfg-pv').value = v || '';
      row.querySelector('.cfg-pdel').addEventListener('click', () => row.remove());
      return row;
    }
    // openCfgModal('edit', fullPath) prefills the node at that path; openCfgModal('add', parent)
    // opens a blank form whose Save inserts a NEW child under `parent` ('' / omitted = top level).
    async function openCfgModal(mode, path) {
      const kinds = await ensureProbeKinds();
      $('cfgMode').value = mode;
      cfgEditPath = mode === 'edit' ? path : null;
      cfgAddParent = mode === 'add' ? (path || '') : '';
      const leaf = mode === 'edit' ? path.split('/').pop() : '';
      $('cfgModalTitle').textContent = mode === 'edit'
        ? ('Edit ' + path)
        : (cfgAddParent ? ('Add target into ' + cfgAddParent) : 'Add target');
      $('cfgFormErr').textContent = '';
      const found = mode === 'edit' ? findCfgNode(path) : null;
      const node = found ? found.node : {};
      $('cfgName').value = mode === 'edit' ? leaf : '';
      $('cfgName').disabled = false; // editable in both modes — a changed name renames the node
      $('cfgProbe').innerHTML = kinds.map((k) => '<option value="' + esc(k) + '"' + (k === node.probe ? ' selected' : '') + '>' + esc(k) + '</option>').join('');
      $('cfgHost').value = node.host || '';
      const pc = $('cfgParams'); pc.innerHTML = '';
      const params = node.params || {};
      const keys = Object.keys(params);
      if (!keys.length) pc.appendChild(cfgParamRow('', ''));
      else for (const k of keys) pc.appendChild(cfgParamRow(k, params[k]));
      $('cfgVantages').value = (node.vantages || []).join(', ');
      $('cfgAlerts').value = (node.alerts || []).join(', ');
      $('cfgModal').classList.remove('hidden');
      $('cfgName').focus();
    }
    function closeCfgModal() { $('cfgModal').classList.add('hidden'); $('cfgFormErr').textContent = ''; cfgEditPath = null; cfgAddParent = ''; }
    function readCfgForm() {
      const params = {};
      for (const row of $('cfgParams').querySelectorAll('.vadmin-row')) {
        const k = row.querySelector('.cfg-pk').value; const v = row.querySelector('.cfg-pv').value;
        if (k.trim()) params[k] = v;
      }
      return {
        name: $('cfgName').value.trim(),
        node: Dash.buildTargetNode({
          probe: $('cfgProbe').value, host: $('cfgHost').value.trim(), params,
          vantages: ($('cfgVantages').value || '').split(','), alerts: ($('cfgAlerts').value || '').split(','),
        }),
      };
    }
    // saveDoc PUTs the whole mutated fragment with the version we last read (optimistic
    // concurrency). 200 -> adopt; 400 -> show the validation error in the modal (keep input);
    // 409 -> someone else changed it, reload; 401 -> back to login.
    async function saveDoc(mutated, onOk) {
      // Surface the error inline in whichever config modal is open (target or group); fall back
      // to an alert only if neither is (e.g. an inline Remove from the tree).
      const showErr = (msg) => {
        if (!$('cfgModal').classList.contains('hidden')) $('cfgFormErr').textContent = msg;
        else if (!$('cfgGroupModal').classList.contains('hidden')) $('cfgGroupErr').textContent = msg;
        else window.alert(msg);
      };
      let r;
      try {
        r = await fetch('/api/admin/config', {
          method: 'PUT', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ version: cfg.version, doc: mutated }),
        });
      } catch (e) { showErr('Network error.'); return; }
      if (r.status === 200) {
        let body = {}; try { body = await r.json(); } catch (e) { /* ignore */ }
        cfg.version = body.version || (cfg.version + 1);
        cfg.doc = mutated;
        renderCfgTree();
        if (onOk) onOk();
        return;
      }
      if (r.status === 401) { closeCfgModal(); closeCfgGroupModal(); renderConfig(); return; }
      if (r.status === 409) { closeCfgModal(); closeCfgGroupModal(); window.alert('Config changed elsewhere — reloading the latest.'); renderConfig(); return; }
      // 400 or other: show the detail
      let msg = 'HTTP ' + r.status;
      try { msg = (await r.json()).error || msg; } catch (e) { /* keep */ }
      showErr(msg);
    }
    $('cfgAddBtn').addEventListener('click', () => { if (cfg.readonly) return; openCfgModal('add'); });
    $('cfgParamAdd').addEventListener('click', () => $('cfgParams').appendChild(cfgParamRow('', '')));
    $('cfgCancel').addEventListener('click', closeCfgModal);
    $('cfgModal').addEventListener('click', (e) => { if (e.target === $('cfgModal')) closeCfgModal(); });
    $('cfgForm').addEventListener('submit', (e) => {
      e.preventDefault();
      $('cfgFormErr').textContent = '';
      const { name, node } = readCfgForm();
      if (!name) { $('cfgFormErr').textContent = 'Name required.'; return; }
      // Reserved-name guard (parked Task-1 review finding): addTarget/editTarget now store
      // via defineProperty so '__proto__' round-trips correctly instead of vanishing — but
      // a target literally named '__proto__'/'constructor'/'prototype' is still a footgun
      // (e.g. downstream JSON tooling, YAML export) worth rejecting up front in the UI.
      if (['__proto__', 'constructor', 'prototype'].includes(name)) { $('cfgFormErr').textContent = '"' + name + '" is a reserved name.'; return; }
      const isEdit = $('cfgMode').value === 'edit';
      // "/" is the structural path separator (cfgTree joins segments with it) — a node name
      // is a single segment. Without this guard, a top-level target named e.g. "Web/a" would
      // collide with the cfgTree path of an existing nested "a" under a "Web" folder, and
      // findCfgNode's depth-first search would silently resolve Edit/Remove/drag to the wrong
      // node. Only the add path can introduce a new name; edit reuses an existing path.
      // A name is a single path segment in both modes now (edit can rename), so "/" is always out.
      if (name.includes('/')) { $('cfgFormErr').textContent = 'Names can\'t contain "/".'; return; }
      let mutated;
      try {
        if (isEdit) {
          // Update the node's value at its current path, then — if the name changed — rekey it.
          // renameNodeAtPath preserves the node's subtree, weight and sibling position.
          mutated = Dash.editNodeAtPath(cfg.doc, cfgEditPath, node);
          if (name !== cfgEditPath.split('/').pop()) {
            mutated = Dash.renameNodeAtPath(mutated, cfgEditPath, name);
            cfgFocusPath = cfgEditPath.split('/').slice(0, -1).concat(name).join('/'); // keep focus on the renamed row
          }
        } else {
          // Add inserts under cfgAddParent ('' = top level, else the folder whose "+" opened it).
          mutated = Dash.addNodeAtPath(cfg.doc, cfgAddParent, name, node);
        }
      } catch (err) { $('cfgFormErr').textContent = err.message; return; }
      saveDoc(mutated, closeCfgModal);
    });
    // --- Add group modal: create a folder (site) plus one or more child targets in one step.
    // A group can't be empty (the validator rejects a node with no host and no children, and
    // children,omitempty drops an empty map on save), so ≥1 child is required; more can be added
    // later via the folder's "+". Validation lives in Dash.buildGroupNode; this is DOM glue.
    function cfgGroupChildRow(kinds, sel) {
      const row = document.createElement('div');
      row.className = 'cfg-childrow';
      row.innerHTML =
        '<input class="vadmin-input cfg-cname" type="text" placeholder="name e.g. ICMP (FPing)"> ' +
        '<select class="vadmin-input cfg-cprobe" aria-label="Probe"></select> ' +
        '<input class="vadmin-input cfg-chost" type="text" placeholder="host e.g. 8.8.8.8"> ' +
        '<button type="button" class="vadmin-btn cfg-cdel" aria-label="Remove this target">×</button>';
      row.querySelector('.cfg-cprobe').innerHTML = kinds.map((k) => '<option value="' + esc(k) + '"' + (k === sel ? ' selected' : '') + '>' + esc(k) + '</option>').join('');
      row.querySelector('.cfg-cdel').addEventListener('click', () => {
        const box = $('cfgGroupChildren');
        row.remove();
        if (!box.querySelector('.cfg-childrow')) box.appendChild(cfgGroupChildRow(kinds)); // always keep ≥1 row
      });
      return row;
    }
    async function openCfgGroupModal() {
      const kinds = await ensureProbeKinds();
      $('cfgGroupName').value = '';
      $('cfgGroupVantages').value = '';
      $('cfgGroupAlerts').value = '';
      $('cfgGroupErr').textContent = '';
      const box = $('cfgGroupChildren'); box.innerHTML = '';
      box.appendChild(cfgGroupChildRow(kinds));
      $('cfgGroupChildAdd').onclick = () => box.appendChild(cfgGroupChildRow(kinds));
      $('cfgGroupModal').classList.remove('hidden');
      $('cfgGroupName').focus();
    }
    function closeCfgGroupModal() { $('cfgGroupModal').classList.add('hidden'); $('cfgGroupErr').textContent = ''; }
    function readCfgGroupForm() {
      const children = Array.from($('cfgGroupChildren').querySelectorAll('.cfg-childrow')).map((r) => ({
        name: r.querySelector('.cfg-cname').value,
        probe: r.querySelector('.cfg-cprobe').value,
        host: r.querySelector('.cfg-chost').value,
      }));
      return {
        name: $('cfgGroupName').value.trim(),
        vantages: ($('cfgGroupVantages').value || '').split(','),
        alerts: ($('cfgGroupAlerts').value || '').split(','),
        children,
      };
    }
    $('cfgAddGroupBtn').addEventListener('click', () => { if (cfg.readonly) return; openCfgGroupModal(); });
    $('cfgGroupCancel').addEventListener('click', closeCfgGroupModal);
    $('cfgGroupModal').addEventListener('click', (e) => { if (e.target === $('cfgGroupModal')) closeCfgGroupModal(); });
    $('cfgGroupForm').addEventListener('submit', (e) => {
      e.preventDefault();
      $('cfgGroupErr').textContent = '';
      const f = readCfgGroupForm();
      if (!f.name) { $('cfgGroupErr').textContent = 'Group name required.'; return; }
      if (f.name.includes('/')) { $('cfgGroupErr').textContent = 'Names can\'t contain "/".'; return; }
      if (['__proto__', 'constructor', 'prototype'].includes(f.name)) { $('cfgGroupErr').textContent = '"' + f.name + '" is a reserved name.'; return; }
      let mutated;
      try {
        mutated = Dash.addNodeAtPath(cfg.doc, '', f.name, Dash.buildGroupNode(f));
      } catch (err) { $('cfgGroupErr').textContent = err.message; return; }
      // saveDoc reports server-side (400) errors against the target modal / window.alert; the
      // common client-side errors are already surfaced inline above via buildGroupNode.
      saveDoc(mutated, closeCfgGroupModal);
    });
    $('cfgTree').addEventListener('click', (e) => {
      // Twist chevron toggles a folder's collapse (mouse counterpart of Left/Right); keep focus
      // on that folder so the roving anchor stays put.
      const tw = e.target.closest('[data-twist]');
      if (tw) {
        const row = e.target.closest('.crow'); const path = row && row.getAttribute('data-path');
        if (path) { cfgCollapsed.has(path) ? cfgCollapsed.delete(path) : cfgCollapsed.add(path); renderCfgTree(); focusCfgRow(path); }
        return;
      }
      if (cfg.readonly) return; // Effective tree: navigation/collapse only — no add/edit/remove
      const ac = e.target.closest('[data-add-child]');
      if (ac) { openCfgModal('add', ac.getAttribute('data-add-child')); return; }
      const ed = e.target.closest('[data-edit]');
      if (ed) { openCfgModal('edit', ed.getAttribute('data-edit')); return; }
      const rm = e.target.closest('[data-remove]');
      if (rm) {
        const path = rm.getAttribute('data-remove');
        const found = findCfgNode(path);
        const prompt = (found && found.isFolder)
          ? 'Remove folder "' + path + '" and everything under it?'
          : 'Remove "' + path + '"?';
        if (!window.confirm(prompt)) return;
        saveDoc(Dash.removeNodeAtPath(cfg.doc, path));
      }
    });
    // Roving focus follows real DOM focus: focusing a treeitem (Tab in, click, .focus()) makes
    // it the single tabbable + aria-selected row. Only .crow itself counts — focusing an inner
    // button must not move the tree's selection.
    $('cfgTree').addEventListener('focusin', (e) => {
      const t = e.target;
      if (!t.classList || !t.classList.contains('crow')) return;
      cfgFocusPath = t.getAttribute('data-path');
      applyRoving(cfgFocusPath);
    });
    // Keyboard (WAI-ARIA tree): every decision comes from the pure Dash.cfgTreeKey over the
    // pure Dash.cfgVisibleRows; this handler only APPLIES the returned action (move focus,
    // toggle collapse, or reorder siblings via moveInList + reorderSiblings, keeping focus on
    // the moved node). Alt+Up / Alt+Down is the documented reorder chord.
    $('cfgTree').addEventListener('keydown', (e) => {
      const NAVKEYS = ['ArrowUp', 'ArrowDown', 'ArrowLeft', 'ArrowRight', 'Home', 'End'];
      if (!NAVKEYS.includes(e.key)) return;
      // Act only when the treeitem row ITSELF holds focus (roving tabindex is on the .crow). Using
      // closest('.crow') would walk UP from a focused inner control (Edit/Remove/+) to the row and
      // fire tree nav — including an Alt+Arrow reorder — while the button is focused; guard on the
      // event target directly so keys on an inner control fall through to it.
      const row = e.target;
      if (!row.classList || !row.classList.contains('crow')) return;
      const rows = Dash.cfgVisibleRows(Dash.cfgTree(cfg.doc), cfgCollapsed);
      const action = Dash.cfgTreeKey(rows, row.getAttribute('data-path'), e.key, e.altKey);
      if (!action) return;
      e.preventDefault();
      if (action.type === 'focus') { focusCfgRow(action.path); return; }
      if (action.type === 'expand') { cfgCollapsed.delete(action.path); renderCfgTree(); focusCfgRow(action.path); return; }
      if (action.type === 'collapse') { cfgCollapsed.add(action.path); renderCfgTree(); focusCfgRow(action.path); return; }
      if (action.type === 'reorder') {
        const order = cfgSiblingNames(action.parentPath);
        const next = Dash.moveInList(order, action.name, action.delta);
        if (JSON.stringify(next) === JSON.stringify(order)) { focusCfgRow(action.path); return; } // clamped at an end
        cfgFocusPath = action.path; // re-establish focus on the moved node after the save re-renders
        saveDoc(Dash.reorderSiblings(cfg.doc, action.parentPath, next), () => focusCfgRow(action.path));
      }
    });
    // Drag-and-drop on #cfgTree — now cross-folder capable. dropPlan (pure-ish glue over the
    // tested Dash.cfgTree ordering) turns a (drag, target-row) pair into a destination parent +
    // index, then the mutation itself goes through the tested Dash.moveNode, which reweights
    // BOTH affected sibling groups and refuses to drop a folder into its own subtree. Rules:
    //   • target is a folder (same group or another) -> move INTO it (appended),
    //   • target is a leaf -> reorder/move into the leaf's parent, before the leaf.
    // Saves through the same optimistic-version saveDoc PUT used everywhere else.
    (function cfgDnd() {
      const host = $('cfgTree');
      let dragPath = null;
      function dropPlan(from, targetPath, targetIsFolder) {
        const dest = Dash.cfgDropDestination(from, targetPath, targetIsFolder);
        if (!dest) return null;
        const dragParent = cfgParentOf(from);
        const dragName = from.split('/').pop();
        const { destParent, kind } = dest;
        let index, noop = false;
        if (kind === 'into') {
          // Dropping a node onto the folder it is already directly in changes nothing — no-op
          // rather than pointlessly re-append (and PUT) it to the end of its own group.
          if (destParent === dragParent) noop = true;
          index = cfgSiblingNames(destParent).length; // append into the folder
        } else {
          let order = cfgSiblingNames(destParent);
          if (destParent === dragParent) order = order.filter((n) => n !== dragName);
          const ti = order.indexOf(targetPath.split('/').pop());
          index = ti < 0 ? order.length : ti;
          if (destParent === dragParent) { // detect a drop that changes nothing
            const after = order.slice(); after.splice(index, 0, dragName);
            noop = JSON.stringify(after) === JSON.stringify(cfgSiblingNames(destParent));
          }
        }
        const newPath = destParent ? destParent + '/' + dragName : dragName;
        return { destParent, index, kind, newPath, noop };
      }
      const clearDropMarks = () => { for (const el of host.querySelectorAll('.cfg-drop, .cfg-drop-into')) el.classList.remove('cfg-drop', 'cfg-drop-into'); };
      host.addEventListener('dragstart', (e) => {
        if (!adminEditor || cfg.readonly) { e.preventDefault(); return; } // reordering is admin-only, and never in the read-only Effective tree
        const row = e.target.closest('.crow'); if (!row) return;
        dragPath = row.getAttribute('data-path');
        e.dataTransfer.effectAllowed = 'move';
        // Firefox refuses to start a drag at all without data on the DataTransfer (Chromium/
        // WebKit don't require it) — the payload itself is unused; dragPath above carries it.
        e.dataTransfer.setData('text/plain', dragPath);
      });
      host.addEventListener('dragover', (e) => {
        const row = e.target.closest('.crow'); if (!row || dragPath == null) return;
        const plan = dropPlan(dragPath, row.getAttribute('data-path'), row.classList.contains('folder'));
        if (!plan) return;
        e.preventDefault();
        clearDropMarks();
        row.classList.add(plan.kind === 'into' ? 'cfg-drop-into' : 'cfg-drop');
      });
      host.addEventListener('dragleave', (e) => { const row = e.target.closest('.crow'); if (row) row.classList.remove('cfg-drop', 'cfg-drop-into'); });
      host.addEventListener('drop', (e) => {
        const row = e.target.closest('.crow'); const from = dragPath; dragPath = null;
        clearDropMarks();
        if (!row || from == null) return;
        e.preventDefault();
        const plan = dropPlan(from, row.getAttribute('data-path'), row.classList.contains('folder'));
        if (!plan || plan.noop) return;
        let mutated;
        try { mutated = Dash.moveNode(cfg.doc, from, plan.destParent, plan.index); }
        catch (err) { window.alert(err.message); return; }
        if (plan.kind === 'into') cfgCollapsed.delete(plan.destParent); // reveal the node we just moved in
        cfgFocusPath = plan.newPath; // the node's path changes on a cross-folder move
        saveDoc(mutated, () => focusCfgRow(plan.newPath));
      });
      host.addEventListener('dragend', () => { dragPath = null; clearDropMarks(); });
    })();
    $('cfgImportBtn').addEventListener('click', () => {
      if (cfg.readonly) return; // never import into a read-only (effective) view
      $('cfgImportText').value = '';
      $('cfgImportErr').textContent = '';
      $('cfgImportModal').classList.remove('hidden');
      $('cfgImportText').focus();
    });
    $('cfgImportCancel').addEventListener('click', () => $('cfgImportModal').classList.add('hidden'));
    $('cfgImportModal').addEventListener('click', (e) => { if (e.target === $('cfgImportModal')) $('cfgImportModal').classList.add('hidden'); });
    $('cfgImportForm').addEventListener('submit', async (e) => {
      e.preventDefault();
      $('cfgImportErr').textContent = '';
      const text = $('cfgImportText').value;
      if (!text.trim()) { $('cfgImportErr').textContent = 'Paste a config first.'; return; }
      let r;
      try {
        r = await fetch('/api/admin/config/import', { method: 'POST', headers: { 'Content-Type': 'text/plain' }, body: text });
      } catch (err) { $('cfgImportErr').textContent = 'Network error.'; return; }
      if (r.status === 200) {
        let body = {}; try { body = await r.json(); } catch (e) { /* ignore */ }
        $('cfgImportModal').classList.add('hidden');
        window.alert('Imported ' + (body.added || 0) + ' new, ' + (body.unchanged || 0) + ' unchanged.');
        renderConfig();
        return;
      }
      if (r.status === 401) { $('cfgImportModal').classList.add('hidden'); renderConfig(); return; }
      if (r.status === 409) { $('cfgImportModal').classList.add('hidden'); window.alert('Config changed elsewhere — reloading the latest.'); renderConfig(); return; }
      let msg = 'HTTP ' + r.status;
      try { msg = (await r.json()).error || msg; } catch (e) { /* keep */ }
      $('cfgImportErr').textContent = msg;
    });

    // ---- routing ----
    function show(id) { for (const v of ['viewOverview', 'viewGraphs', 'viewStack', 'viewZoom', 'viewVantages', 'viewConfig']) $(v).classList.toggle('hidden', v !== id); }
    function setTabs(view) {
      const g = (view === 'graphs' || view === 'stack' || view === 'zoom');
      $('tabOverview').setAttribute('aria-selected', String(view === 'overview'));
      $('tabGraphs').setAttribute('aria-selected', String(g));
      $('tabVantages').setAttribute('aria-selected', String(view === 'vantages'));
      $('tabConfig').setAttribute('aria-selected', String(view === 'config'));
    }
    function currentView() { return parseRoute(location.hash).view; }
    // Status-line owner for the admin tabs. Overview/Graphs update #statusText inside their own
    // refresh loops; Config/Vantages don't, so without this the line stays stuck on the initial
    // "connecting…". A light /api/targets probe reflects collector reachability there.
    async function pingStatus(expectedView) {
      try {
        const targets = (await fetchJSON('/api/targets')).targets || [];
        if (!Dash.statusProbeOwnsView(expectedView, currentView())) return;
        $('statusText').textContent = targets.length + ' targets · updated ' + new Date().toLocaleTimeString();
      } catch (e) { if (Dash.statusProbeOwnsView(expectedView, currentView())) $('statusText').textContent = 'collector unreachable — showing last known'; }
    }
    function route() {
      { const cm = $('cfgModal'); if (cm && !cm.classList.contains('hidden')) cm.classList.add('hidden'); }
      { const cim = $('cfgImportModal'); if (cim && !cim.classList.contains('hidden')) cim.classList.add('hidden'); }
      const r = parseRoute(location.hash);
      if (r.view === 'overview') { setTabs('overview'); show('viewOverview'); refreshOverview(); }
      else if (r.view === 'graphs') { gridScope = r.path || ''; setTabs('graphs'); show('viewGraphs'); renderScope(); renderTree(); renderGridPanels(); refreshGrid(); }
      else if (r.view === 'stack') { setTabs('stack'); show('viewStack'); renderStack(r.name); }
      else if (r.view === 'zoom') { setTabs('zoom'); show('viewZoom'); renderZoom(r.name, r.range); }
      else if (r.view === 'vantages') { setTabs('vantages'); show('viewVantages'); renderVantages(); pingStatus('vantages'); }
      else if (r.view === 'config') { setTabs('config'); show('viewConfig'); renderConfig(); pingStatus('config'); }
      // Show the human path in the status line, not the raw routing token (a UUID for a moved target).
      $('statusText').textContent = (r.view === 'stack' || r.view === 'zoom') ? (nameByKey.get(r.name) || r.name) : $('statusText').textContent;
      window.scrollTo(0, 0);
    }
    function nav(hash) { if (location.hash.replace(/^#/, '') !== hash) history.pushState(null, '', '#' + hash); route(); }
    window.addEventListener('popstate', route);

    // ---- events ----
    $('tabOverview').addEventListener('click', () => nav('overview'));
    $('tabGraphs').addEventListener('click', () => nav('graphs'));
    $('tabVantages').addEventListener('click', () => nav('vantages'));
    $('tabConfig').addEventListener('click', () => nav('config'));
    $('backStack').addEventListener('click', () => { if (history.length > 1) history.back(); else nav('graphs'); });
    $('backZoom').addEventListener('click', () => { if (history.length > 1) history.back(); else nav('target=' + enc(curTarget || '')); });
    $('worstSeg').addEventListener('click', (e) => { const b = e.target.closest('button'); if (!b) return; worstBy = b.dataset.by; document.querySelectorAll('#worstSeg button').forEach((x) => x.setAttribute('aria-pressed', String(x === b))); refreshWorst(); });
    document.addEventListener('click', (e) => {
      const sp = e.target.closest('.spanel'); if (sp) { nav('target=' + enc(curTarget) + '&range=' + sp.dataset.range); return; }
      const g = e.target.closest('.gpanel'); if (g) { nav('target=' + enc(g.dataset.target)); return; }
      const who = e.target.closest('.who[data-target]'); if (who) { nav('target=' + enc(who.dataset.target)); }
    });

    // ---- vantage chip legend/selector: click focuses that vantage and re-renders from
    // the already-fetched per-vantage series (byV) — no refetch. ----
    $('stackVantages').addEventListener('click', (e) => {
      const b = e.target.closest('.vchip'); if (!b) return;
      const v = b.dataset.v; if (!v || v === stackFocus) return;
      stackFocus = v;
      for (const c of stackCanvases) renderStackCell(c);
      renderStackChips();
    });
    $('zoomVantages').addEventListener('click', (e) => {
      const b = e.target.closest('.vchip'); if (!b) return;
      const v = b.dataset.v; if (!v || v === zoomFocus || !zoomState || !zoomState.byV) return;
      zoomFocus = v;
      zoomState.focus = v;
      const s = zoomState.byV[v];
      zoomState.series = s;
      // Recompute the stats panel for the newly-focused vantage — mirrors renderStackCell's
      // meta handling, so the band and #zoomMeta never disagree about which vantage is focused
      // (including its NTP stat, M2).
      if (s && !s.unsupported && s.buckets.length >= 2) $('zoomMeta').innerHTML = metaHtml(s, v);
      else $('zoomMeta').innerHTML = '';
      drawZoom();
      renderZoomChips();
    });

    // ---- drag-to-zoom on the detail graph ----
    (function setupZoomDrag() {
      const canvas = $('zoomCanvas'), sel = $('zoomSel');
      let dragging = false, x0 = 0;
      const clampX = (x) => { const { mL, mR } = Smoke.MARGINS; return Math.max(mL, Math.min(canvas.clientWidth - mR, x)); };
      canvas.addEventListener('mousedown', (e) => {
        if (!zoomState) return;
        dragging = true; x0 = clampX(e.offsetX);
        sel.hidden = false; sel.style.left = x0 + 'px'; sel.style.width = '0px';
      });
      canvas.addEventListener('mousemove', (e) => {
        if (!dragging) return; const x = clampX(e.offsetX);
        sel.style.left = Math.min(x0, x) + 'px'; sel.style.width = Math.abs(x - x0) + 'px';
      });
      const finish = (e) => {
        if (!dragging) return; dragging = false; sel.hidden = true;
        if (!zoomState) return;
        const x1 = clampX(e.offsetX);
        if (Math.abs(x1 - x0) < 6) return; // a click, not a drag
        const a = pixelToTime(Math.min(x0, x1), canvas.clientWidth, zoomState.t0, zoomState.t1);
        const b = pixelToTime(Math.max(x0, x1), canvas.clientWidth, zoomState.t0, zoomState.t1);
        if (b - a < 1000) return; // ignore sub-second selections
        zoomTo(a, b);
      };
      canvas.addEventListener('mouseup', finish);
      canvas.addEventListener('mouseleave', finish);
    })();
    $('zoomReset').addEventListener('click', () => { if (curTarget && curRange) renderZoom(curTarget, curRange); });
    $('unisonToggle').addEventListener('change', (e) => { unisonScale = e.target.checked; renderGridPanels(); });
    // Toggle a vantage on the grid: re-render immediately from cached series, then refresh to fetch a
    // just-shown vantage's data and re-aggregate the status dots.
    $('gridVantageBar').addEventListener('click', (e) => {
      const b = e.target.closest('.vseg-chip'); if (!b) return;
      gridVantages = toggleGridVantage(gridVantages, b.dataset.v, availVantages, MAX_GRID_VANTAGES);
      saveGridVantages();
      renderVantageControl();
      renderGridPanels();
      refreshGrid();
    });
    $('colsSeg').addEventListener('click', (e) => { const b = e.target.closest('button'); if (!b) return; gridCols = b.dataset.cols; try { localStorage.setItem('graphCols', gridCols); } catch (err) {} document.querySelectorAll('#colsSeg button').forEach((x) => x.setAttribute('aria-pressed', String(x === b))); applyGridCols(); });
    // reflect the persisted columns choice on load, then apply it to the grid
    document.querySelectorAll('#colsSeg button').forEach((x) => x.setAttribute('aria-pressed', String(x.dataset.cols === gridCols)));
    applyGridCols();
    // Re-evaluate which counts fit (and whether the picker shows) whenever the window resizes.
    window.addEventListener('resize', updateColsPicker);

    // Footer build version (git-describe: latest tag + short SHA for a main/latest build, or the
    // exact tag for a release). Best-effort — a failed fetch leaves the plain "Heliograph".
    (async () => {
      try {
        const boot = await fetchJSON('/api/version');
        // Absolute vs relative time labels is server-configured; apply it uniformly and re-render
        // the current view if it differs from the default we assumed before the fetch landed.
        if (typeof boot.absolute_time === 'boolean' && boot.absolute_time !== timeAbsolute) {
          timeAbsolute = boot.absolute_time;
          rerender();
        }
        const v = boot.version;
        if (!v) return;
        // Link the version to its source: the exact commit for a git-describe build
        // (…-g<sha>), the release page for a clean tag, else the repo root.
        const repo = 'https://github.com/seitzbg/heliograph';
        const sha = (v.match(/-g([0-9a-f]+)$/) || [])[1];
        const href = sha ? repo + '/commit/' + sha : (/^v[0-9]/.test(v) ? repo + '/releases/tag/' + encodeURIComponent(v) : repo);
        $('appFooter').innerHTML = 'Heliograph <a href="' + href + '" target="_blank" rel="noopener noreferrer">' + esc(v) + '</a>';
      } catch (e) { /* leave the default footer + assumed absolute-time default */ }
    })();

    // ---- config-tree menu events ----
    $('navTree').addEventListener('click', (e) => { const row = e.target.closest('.row'); if (row) activateRow(row, !!e.target.closest('[data-twist]')); });
    $('navTree').addEventListener('keydown', (e) => { if (e.key !== 'Enter' && e.key !== ' ') return; const row = e.target.closest('.row'); if (!row) return; e.preventDefault(); activateRow(row, false); });
    $('navFilter').addEventListener('input', (e) => { navQuery = e.target.value; renderTree(); });
    $('graphScope').addEventListener('click', (e) => { const l = e.target.closest('.crumb-link'); if (l) navScope(l.dataset.path || ''); });

    // ---- theme ----
    const btn = $('themeBtn');
    const curTheme = () => document.documentElement.getAttribute('data-theme') || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
    function themeLabel() { const d = curTheme() === 'dark'; $('themeIcon').textContent = d ? '☾' : '☀'; $('themeLabel').textContent = d ? 'Dark' : 'Light'; }
    function rerender() {
      const v = currentView();
      if (v === 'graphs') { renderGridPanels(); }
      // renderStackCell (not renderInto directly) so a theme toggle/resize re-resolves overlay
      // LINE colors via cssVar; renderStackChips likewise re-resolves the chip SWATCH colors
      // (baked as inline style at render time) — both self-guard to a no-op on a single-vantage
      // target (stackVantages.length<=1 => renderStackChips just re-clears the empty container).
      else if (v === 'stack') { for (const c of stackCanvases) renderStackCell(c); renderStackChips(); }
      else if (v === 'zoom') { drawZoom(); renderZoomChips(); } // same: line colors + chip swatches
    }
    btn.addEventListener('click', () => { const next = curTheme() === 'dark' ? 'light' : 'dark'; document.documentElement.setAttribute('data-theme', next); try { localStorage.setItem('theme', next); } catch (e) {} themeLabel(); refreshKey(); rerender(); });
    matchMedia('(prefers-color-scheme: dark)').addEventListener('change', () => { themeLabel(); refreshKey(); rerender(); });

    // ---- legend ----
    (function buildLegend() {
      // Loss key as one compact green→red strip (was 8 labeled chips): each bucket is a segment; its
      // threshold shows on hover so the detail stays available without the horizontal bulk.
      const ll = $('lossLegend');
      ll.innerHTML = '';
      for (const b of Smoke.LOSS_COLORS) { const i = document.createElement('i'); i.style.background = b.color; i.title = b.label + ' loss'; ll.appendChild(i); }
      refreshKey();
    })();
    function refreshKey() { const sk = $('smokeKey'); if (!sk) return; sk.innerHTML = ''; const dark = Smoke.readVars().dark; for (let k = 1; k <= 6; k++) { const i = document.createElement('i'); i.style.background = Smoke.smokeGray(k, 6, dark); sk.appendChild(i); } }

    // ---- refresh cadences (only the visible view does work) ----
    let rt; window.addEventListener('resize', () => { clearTimeout(rt); rt = setTimeout(rerender, 140); });
    setInterval(() => { if (currentView() === 'graphs') refreshGrid(); }, 5000);
    setInterval(() => { if (currentView() === 'overview') refreshOverview(); }, 15000);
    setInterval(() => { const v = currentView(); if (v === 'config' || v === 'vantages') pingStatus(v); }, 15000);
    setInterval(refreshAdminState, 60000); // keep the global indicator honest as a 12h session expires
    setInterval(() => { const v = currentView(); if (v === 'stack') renderStack(curTarget); else if (v === 'zoom' && !(zoomState && zoomState.custom)) { const r = parseRoute(location.hash); renderZoom(r.name, r.range); } }, 30000);

    themeLabel();
    route();
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();

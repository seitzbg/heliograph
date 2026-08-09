// dashboard.js — the smokeping-modern SPA. Reads the Go collector's JSON API and
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
  function gridSince(panels) {
    let min = null;
    for (const p of panels) {
      const bs = p && p.series && p.series.buckets;
      if (!bs || !bs.length) continue;
      const last = bs[bs.length - 1].t;
      if (!Number.isFinite(last)) continue;
      if (min === null || last < min) min = last;
    }
    return min;
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
  // Siblings are sorted by name for a stable menu. Returns the top-level nodes:
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
    (function finish(n) { n.children.sort((a, b) => a.name < b.name ? -1 : a.name > b.name ? 1 : 0); delete n._m; n.children.forEach(finish); })(root);
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
    if (t.error || (t.loss_pct != null && t.loss_pct >= 50)) return 'down';
    if (t.loss_pct != null && t.loss_pct > 0.5) return 'degraded';
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

  // vantageColorVar maps a vantage to a CSS var NAME: the neutral median color for 'local',
  // else a palette slot keyed by the vantage's position among `ordered`'s non-local entries
  // (mod palette length) — stable as long as `ordered` (from orderVantages) is stable.
  function vantageColorVar(vantage, ordered) {
    if (vantage === 'local') return '--median-base';
    const rest = (ordered || []).filter((v) => v !== 'local');
    const i = rest.indexOf(vantage);
    return VPAL[(i < 0 ? 0 : i) % VPAL.length];
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
  function addTarget(doc, name, node) {
    const d = cfgWithChildren(doc);
    if (Object.prototype.hasOwnProperty.call(d.targets.children, name)) throw new Error('a target named "' + name + '" already exists');
    d.targets.children[name] = node;
    return d;
  }
  function editTarget(doc, name, node) {
    const d = cfgWithChildren(doc);
    if (!Object.prototype.hasOwnProperty.call(d.targets.children, name)) throw new Error('no target named "' + name + '"');
    d.targets.children[name] = node;
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

  window.Dash = { RANGES, RANGE_ORDER, parseRoute, mergeSeries, gridSince, fetchJSON, zoomResolution, pixelToTime, sharedYMax, buildTree, underPath, targetStatus, pickSeries, vantageList, orderVantages, defaultFocus, keepFocus, vantageColorVar, adminMode, relTime, listTargets, addTarget, editTarget, removeTarget, buildTargetNode };

  // ---------------------------------------------------------------- init (DOM) --
  function init() {
    const $ = (id) => document.getElementById(id);
    const fmt = (v, d) => (v == null || isNaN(v)) ? '--' : v.toFixed(d);
    const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
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
    function renderInto(canvas, s, R, height, yMax, overlays) {
      if (s && s.unsupported) { drawNote(canvas, 'needs the TimescaleDB store (-dsn -downsample)', height); return; }
      if (!s || s.buckets.length < 2) { drawNote(canvas, 'collecting…', height); return; }
      // Fixed wall-clock domain [now-windowMs, now]. t1 extends to the newest sample if
      // the client clock lags the server, so a fresh sample never clamps to the edge;
      // t0 anchors to the selected range so the axis labels stay literally correct.
      const lastT = s.buckets[s.buckets.length - 1].t;
      const t1 = Math.max(Date.now(), Number.isFinite(lastT) ? lastT : 0);
      const t0 = R.windowMs ? t1 - R.windowMs : undefined;
      Smoke.render(canvas, s, { height, band: R.mode === 'band', xlabels: R.xl, t0, t1: t0 == null ? undefined : t1, yMax, overlays });
    }
    function metaHtml(s) {
      const st = Smoke.seriesStats(s); const lcls = st.lossAvg > 2 ? 'bad' : st.lossAvg > 0.5 ? 'warn' : '';
      return '<span class="stat"><span class="k">median avg</span><span class="v">' + fmt(st.medAvg, 1) + ' ms</span></span>' +
             '<span class="stat"><span class="k">median max</span><span class="v">' + fmt(st.medMax, 1) + ' ms</span></span>' +
             '<span class="stat"><span class="k">loss avg</span><span class="v ' + lcls + '">' + fmt(st.lossAvg, 2) + ' %</span></span>';
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
        return '<li><span class="n">' + (i + 1) + '</span><span class="who" data-target="' + esc(c.name) + '">' + esc(c.name) +
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
            '<span class="who" data-target="' + esc(s.name) + '">' + esc(s.name) + '<span class="pk">' + esc(s.probe) + '</span>' + (thin ? '<span class="chip">thin data</span>' : '') + '</span>' +
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
    // ensureVantages backfills vantagesByTarget for a detail view reached before
    // refreshGrid has populated it (e.g. a deep link to #target=...): a no-op once the
    // grid has run, otherwise one /api/targets fetch to seed the map. `name` is accepted
    // so Task 3's detail views can call it uniformly with the target about to render,
    // even though seeding fetches every target's vantage set at once.
    async function ensureVantages(name) {
      if (vantagesByTarget.size) return;
      try {
        const targets = (await fetchJSON('/api/targets')).targets || [];
        for (const t of targets) vantagesByTarget.set(t.name, vantageList(t));
      } catch (e) { /* transient: vantagesFor falls back to ['local'] */ }
    }
    function ensurePanel(t) {
      let p = panels.get(t.name); if (p) return p;
      const grid = $('graphGrid'); if (panels.size === 0) grid.innerHTML = '';
      const el = document.createElement('div'); el.className = 'panel gpanel'; el.dataset.target = t.name;
      el.innerHTML = '<h2>' + esc(t.name) + ' <span class="probe">' + esc(t.probe) + '</span></h2><div class="meta"></div><canvas></canvas>';
      grid.appendChild(el);
      p = { el, canvas: el.querySelector('canvas'), meta: el.querySelector('.meta'), series: null };
      panels.set(t.name, p); return p;
    }
    // One bulk, incremental read for the whole grid: /api/series/all returns every
    // target's rounds newer than the watermark (or the full 3h window on the first
    // tick, when sinceMs is null), so a refresh is one request + one store query
    // regardless of target count — replacing the old one-fetch-per-target fan-out
    // (CODE_REVIEW #2). Response: { cutoff, targets: { name: { rounds:[...] } } }.
    async function fetchGridSeries(sinceMs) {
      const since = sinceMs != null ? '&since=' + sinceMs : '';
      const r = await fetch('/api/series/all?window=' + RANGES['3h'].window + since, { cache: 'no-store' });
      if (!r.ok) return null;
      return r.json();
    }
    function gridMeta(p, s) {
      const st = Smoke.seriesStats(s); const lcls = st.lossAvg > 2 ? 'bad' : st.lossAvg > 0.5 ? 'warn' : '';
      p.meta.innerHTML = '<span class="stat"><span class="k">median</span><span class="v">' + fmt(st.medAvg, 1) + ' ms</span></span>' +
        '<span class="stat"><span class="k">loss</span><span class="v ' + lcls + '">' + fmt(st.lossAvg, 2) + ' %</span></span>';
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
        // ALL configured targets go in the tree (so a remote-only target is navigable), but
        // only those with local data get a grid thumbnail — the grid reads the local vantage,
        // so a no-data (remote-only) target would otherwise show an empty "collecting…" panel
        // forever. It stays reachable via the tree; its real series shows in the detail view,
        // which focuses the target's own vantage (CODE_REVIEW #3 / P1-3).
        statusByTarget.clear(); vantagesByTarget.clear();
        for (const t of targets) { statusByTarget.set(t.name, targetStatus(t)); vantagesByTarget.set(t.name, vantageList(t)); }
        treeNames = targets.map((t) => t.name);
        const gridTargets = targets.filter((t) => !t.no_data);
        // Reconcile ONLY against an authoritative target list (the fetch above succeeded):
        // drop panels for targets no longer reported (e.g. removed on a SIGHUP reload, or a
        // target that became no-data). A failed /api/targets returned early, so a transient
        // 503 never blanks the grid (#2).
        const live = new Set(gridTargets.map((t) => t.name));
        for (const [name, p] of panels) {
          if (!live.has(name)) { p.el.remove(); panels.delete(name); }
        }
        const cutoffMs = Date.now() - RANGES['3h'].windowMs;
        // Incremental watermark = the oldest frontier among panels holding data, so the
        // shared `since` never advances past a slow target and skips its late rounds (#1).
        // null (until the first fetch lands) means fetch the whole window.
        const since = gridLoaded ? gridSince([...panels.values()]) : null;
        let bulk = null;
        try { bulk = await fetchGridSeries(since); } catch (e) { /* transient: keep panels */ }
        await Promise.all(gridTargets.map(async (t) => {
          const p = ensurePanel(t);
          let incoming = null;
          const raw = bulk && bulk.targets && bulk.targets[t.name];
          if (raw) {
            incoming = Smoke.fromApiSeries(raw);
          } else if (!gridLoaded || !p.series) {
            // First load, or a panel the incremental read didn't cover that has no cached
            // data yet: backfill its full window once.
            try { const s = await fetchRange(t.name, '3h'); if (s && !s.unsupported) incoming = s; } catch (e) { /* transient */ }
          }
          if (incoming) {
            p.series = mergeSeries(p.series, incoming, cutoffMs);
          } else if (p.series) {
            p.series = mergeSeries(p.series, null, cutoffMs); // no new rounds: still age out old ones
          }
          if (p.series && p.series.buckets.length) gridMeta(p, p.series);
        }));
        if (bulk) gridLoaded = true;
        // Don't claim "updated" when the bulk series fetch failed (a non-2xx/network error left
        // bulk null): the panels are showing last-known data, so say so instead of lying (#5).
        if (!bulk) $('statusText').textContent = targets.length + ' targets · graph data degraded (last known) · ' + new Date().toLocaleTimeString();
        renderGridPanels();     // render the visible (scoped) panels, sharing a Y-axis
        renderTreeIfChanged();  // refresh the menu dots when a target's status changed
      } finally { gridBusy = false; }
    }
    // Render every VISIBLE grid panel — those under the current subtree scope (gridScope).
    // In unison mode the visible set shares one Y-axis max (sharedYMax) so the small
    // multiples are comparable; scoping to a subtree rescales to just that subtree.
    let unisonScale = true;
    function renderGridPanels() {
      const vis = [];
      for (const p of panels.values()) {
        const show = underPath(p.el.dataset.target, gridScope);
        p.el.style.display = show ? '' : 'none';
        if (show) vis.push(p);
      }
      const yMax = unisonScale ? sharedYMax(vis.map((p) => p.series)) : undefined;
      for (const p of vis) renderInto(p.canvas, p.series, RANGES['3h'], 170, yMax);
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
      return '<div class="row leaf" data-target="' + esc(n.target) + '" style="--d:' + depth + '" tabindex="0" role="treeitem">' +
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
      if (c.byV) {
        const focused = c.byV[stackFocus];
        const overlays = buildOverlays(c.byV, stackVantages, stackFocus);
        if (focused && !focused.unsupported && focused.buckets.length >= 2) {
          c.meta.innerHTML = metaHtml(focused) + (c.failed ? ' <span class="reslabel">· last known</span>' : '');
        } else { c.meta.innerHTML = ''; }
        renderInto(c.canvas, focused, c.R, 170, undefined, overlays);
      } else {
        const s = c.series;
        if (s && !s.unsupported && s.buckets.length >= 2) c.meta.innerHTML = metaHtml(s) + (c.failed ? ' <span class="reslabel">· last known</span>' : '');
        renderInto(c.canvas, s, c.R, 170);
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
      $('stackTitle').innerHTML = esc(name);
      const grid = $('stackGrid'); grid.innerHTML = '';

      await ensureVantages(name);
      if (gen !== stackGen) return; // a newer renderStack call superseded this one — don't append its panels
      const vs = Dash.orderVantages(vantagesFor(name));
      stackVantages = vs;

      const cells = RANGE_ORDER.map((key) => {
        const R = RANGES[key];
        const el = document.createElement('div'); el.className = 'panel spanel'; el.dataset.range = key;
        el.innerHTML = '<div class="charts-head"><h3>' + R.label + ' <span class="reslabel">' + R.desc + '</span></h3><span class="reslabel">click to zoom ⤢</span></div>' +
          '<div class="meta"></div><canvas></canvas>';
        grid.appendChild(el);
        return { key, R, el, canvas: el.querySelector('canvas'), meta: el.querySelector('.meta') };
      });
      // probe label from the grid cache if we have it
      const gp = panels.get(name); if (gp) { const probe = gp.el.querySelector('.probe'); if (probe) $('stackTitle').innerHTML = esc(name) + ' <span class="probe">' + esc(probe.textContent) + '</span>'; }

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
          const entry = { canvas: c.canvas, meta: c.meta, R: c.R, key: c.key, series: s, failed: pick.failed };
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
      if (!z.series || !z.series.buckets || z.series.buckets.length < 2) { drawNote(z.canvas, 'collecting…', 360); return; }
      const overlays = z.byV ? buildOverlays(z.byV, z.vantages, z.focus) : undefined;
      Smoke.render(z.canvas, z.series, { height: 360, band: z.band, xlabels: z.xlabels, t0: z.t0, t1: z.t1, overlays });
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
    function rangeLabels(t0, t1) {
      const span = t1 - t0;
      const fmt = (t) => { const d = new Date(t); return span < 36 * H ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : d.toLocaleDateString([], { month: 'short', day: 'numeric' }); };
      return [fmt(t0), fmt(t0 + span / 3), fmt(t0 + 2 * span / 3), fmt(t1)];
    }
    async function renderZoom(name, range) {
      const gen = ++zoomGen; // captured before any await — invalidates any earlier in-flight zoom call (renderZoom or zoomTo), same name/range or not
      curTarget = name; curRange = range; const R = RANGES[range];
      $('zoomTitle').innerHTML = esc(name) + ' <span class="reslabel">· ' + R.label + '</span>';
      $('zoomMeta').innerHTML = ''; $('zoomReset').hidden = true;
      $('zoomRes').textContent = 'drag on the graph to zoom into a time range';
      const canvas = $('zoomCanvas');

      await ensureVantages(name);
      if (gen !== zoomGen) return; // a newer zoom call superseded this one
      const vs = Dash.orderVantages(vantagesFor(name));
      zoomVantages = vs;

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
        zoomState = { canvas, series: s, band: R.mode === 'band', t0: t1 - R.windowMs, t1, xlabels: R.xl, custom: false };
        if (s && !s.unsupported && s.buckets.length >= 2) {
          $('zoomMeta').innerHTML = metaHtml(s);
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
      zoomState = { canvas, series: s, band: R.mode === 'band', t0: t1 - R.windowMs, t1, xlabels: R.xl, custom: false, byV, vantages: vs, focus: zoomFocus };
      if (s && !s.unsupported && s.buckets.length >= 2) {
        $('zoomMeta').innerHTML = metaHtml(s);
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
        canvas, series: s, band: zr.mode === 'band', t0: fromMs, t1: toMs, xlabels: rangeLabels(fromMs, toMs), custom: true,
        byV, vantages: vs.length > 1 ? vs : undefined, focus: vs.length > 1 ? zoomFocus : undefined,
      };
      $('zoomReset').hidden = false;
      const desc = zr.mode === 'raw' ? 'per-round' : (zr.res === '1h' ? 'hourly band' : 'daily band');
      if (s && !s.unsupported && s.buckets && s.buckets.length >= 2) {
        $('zoomMeta').innerHTML = metaHtml(s);
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
    async function renderVantages(opts) {
      const afterLogin = !!(opts && opts.afterLogin);
      let r;
      try { r = await fetch('/api/admin/vantages', { cache: 'no-store' }); }
      catch (e) { vShow('vantError'); return; }
      const mode = Dash.adminMode(r.status);
      if (mode === 'disabled') { vShow('vantDisabled'); return; }
      if (mode === 'error') { vShow('vantError'); return; }
      if (mode === 'login') {
        vShow('vantLogin');
        // Secure-cookie probe: a 204 login followed by a 401 here means the session cookie
        // didn't stick (plain-HTTP LAN, not a secure context). Make that legible.
        $('vantLoginErr').textContent = afterLogin
          ? "Login didn't persist — the admin session needs a secure context (HTTPS via the proxy, or localhost). You are on " + location.origin + '.'
          : '';
        if (!afterLogin) $('vantPass').focus();
        return;
      }
      // mode === 'list'
      let data;
      try { data = await r.json(); } catch (e) { vShow('vantError'); return; }
      vadmin.rows = data.vantages || [];
      renderVantageRows();
      vShow('vantList');
    }
    $('vantRetry').addEventListener('click', () => renderVantages());
    $('vantLogin').addEventListener('submit', async (e) => {
      e.preventDefault();
      $('vantLoginErr').textContent = '';
      const pass = $('vantPass').value;
      $('vantPass').value = ''; // clear immediately — don't leave the password in the field
      let lr;
      try {
        lr = await fetch('/api/admin/login', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ password: pass }),
        });
      } catch (err) { $('vantLoginErr').textContent = 'Network error.'; return; }
      if (lr.status === 401) { $('vantLoginErr').textContent = 'Invalid password.'; return; }
      if (lr.status !== 204) { $('vantLoginErr').textContent = 'Login failed (HTTP ' + lr.status + ').'; return; }
      renderVantages({ afterLogin: true });
    });
    function reportMintError(isRegen, msg) {
      if (isRegen) window.alert('Regenerate failed: ' + msg);
      else $('vantAddErr').textContent = msg;
    }
    // mintVantage POSTs a name; the store creates or rotates (regenerate == re-POST the
    // same name). On success it reveals the one-time key/snippet.
    async function mintVantage(name, isRegen) {
      let r;
      try {
        r = await fetch('/api/admin/vantages', {
          method: 'POST', headers: { 'Content-Type': 'application/json' },
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
      let data;
      try { data = await r.json(); } catch (e) { reportMintError(isRegen, 'Malformed server response.'); return; }
      $('vantName').value = '';
      $('vantAddErr').textContent = '';
      showReveal(data.name, data.snippet || data.key || '');
    }
    function showReveal(name, snippet) {
      $('vantRevealName').textContent = name;
      $('vantRevealSnippet').textContent = snippet; // contains the smk_ key
      $('vantReveal').classList.remove('hidden');
      $('vantRevealClose').focus();
    }
    function closeReveal() {
      $('vantRevealSnippet').textContent = ''; // never leave key material in the DOM
      $('vantReveal').classList.add('hidden');
      renderVantages(); // refresh the list (new/rotated row, updated counts)
    }
    $('vantRevealClose').addEventListener('click', closeReveal);
    $('vantReveal').addEventListener('click', (e) => { if (e.target === $('vantReveal')) closeReveal(); }); // backdrop
    $('vantCopy').addEventListener('click', async () => {
      const text = $('vantRevealSnippet').textContent;
      try {
        await navigator.clipboard.writeText(text);
        $('vantCopy').textContent = 'Copied';
        setTimeout(() => { $('vantCopy').textContent = 'Copy'; }, 1500);
      } catch (e) {
        const rng = document.createRange(); rng.selectNodeContents($('vantRevealSnippet'));
        const sel = window.getSelection(); sel.removeAllRanges(); sel.addRange(rng);
      }
    });
    $('vantAdd').addEventListener('submit', (e) => {
      e.preventDefault();
      $('vantAddErr').textContent = '';
      const name = $('vantName').value.trim();
      if (!name) { $('vantAddErr').textContent = 'Name required.'; return; }
      if (!/^[A-Za-z0-9._-]+$/.test(name)) { $('vantAddErr').textContent = 'Use letters, digits, . _ - only.'; return; }
      // "local" is the hub's own vantage — agents don't use it. Hint, but don't hard-block
      // (design §11: allowed-but-hinted): let the user proceed via confirm.
      if (name === 'local' && !window.confirm('"local" is the hub’s own vantage — agents don’t use it. Mint a key anyway?')) return;
      mintVantage(name, false);
    });
    $('vantRows').addEventListener('click', async (e) => {
      const regen = e.target.closest('[data-regen]');
      if (regen) {
        const name = regen.getAttribute('data-regen');
        if (window.confirm('Regenerate the key for "' + name + '"? This invalidates the current key; the agent must be reconfigured with the new one.')) {
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
    function cShow(id) {
      for (const s of ['cfgDisabled', 'cfgLogin', 'cfgList', 'cfgError']) $(s).classList.toggle('hidden', s !== id);
    }
    function renderConfigRows() {
      const rows = Dash.listTargets(cfg.doc);
      $('cfgVersion').textContent = 'v' + cfg.version;
      if (!rows.length) { $('cfgRows').innerHTML = '<tr><td colspan="5" class="vadmin-empty">No DB targets yet — add one.</td></tr>'; return; }
      $('cfgRows').innerHTML = rows.map((r) => {
        const nm = esc(r.name);
        if (r.isFolder) {
          return '<tr><td>' + nm + '</td><td colspan="3" style="color:var(--ink-faint)">folder — managed via files</td><td></td></tr>';
        }
        const n = r.node || {};
        const details = esc([n.params ? Object.entries(n.params).map(([k, v]) => k + '=' + v).join(' ') : '',
          (n.vantages && n.vantages.length) ? '@' + n.vantages.join(',') : ''].filter(Boolean).join('  '));
        return '<tr><td>' + nm + '</td><td>' + esc(n.probe || '') + '</td><td>' + esc(n.host || '') + '</td><td style="color:var(--ink-soft)">' + details +
          '</td><td style="text-align:right; white-space:nowrap">' +
          '<button class="vadmin-btn" data-edit="' + nm + '">Edit</button>' +
          '<button class="vadmin-btn" data-remove="' + nm + '">Remove</button></td></tr>';
      }).join('');
    }
    async function renderConfig(opts) {
      const afterLogin = !!(opts && opts.afterLogin);
      let r;
      try { r = await fetch('/api/admin/config', { cache: 'no-store' }); }
      catch (e) { cShow('cfgError'); return; }
      const mode = Dash.adminMode(r.status);
      if (mode === 'disabled') { cShow('cfgDisabled'); return; }
      if (mode === 'error') { cShow('cfgError'); return; }
      if (mode === 'login') {
        cShow('cfgLogin');
        $('cfgLoginErr').textContent = afterLogin
          ? "Login didn't persist — the admin session needs a secure context (HTTPS via the proxy, or localhost). You are on " + location.origin + '.'
          : '';
        if (!afterLogin) $('cfgPass').focus();
        return;
      }
      let data;
      try { data = await r.json(); } catch (e) { cShow('cfgError'); return; }
      cfg.version = data.version || 0;
      cfg.doc = (data.doc && typeof data.doc === 'object') ? data.doc : { targets: { children: {} } };
      renderConfigRows();
      cShow('cfgList');
    }
    $('cfgRetry').addEventListener('click', () => renderConfig());
    $('cfgLogin').addEventListener('submit', async (e) => {
      e.preventDefault();
      $('cfgLoginErr').textContent = '';
      const pass = $('cfgPass').value;
      $('cfgPass').value = '';
      let lr;
      try {
        lr = await fetch('/api/admin/login', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ password: pass }) });
      } catch (err) { $('cfgLoginErr').textContent = 'Network error.'; return; }
      if (lr.status === 401) { $('cfgLoginErr').textContent = 'Invalid password.'; return; }
      if (lr.status !== 204) { $('cfgLoginErr').textContent = 'Login failed (HTTP ' + lr.status + ').'; return; }
      renderConfig({ afterLogin: true });
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
    function route() {
      // Never leave a one-time key in the DOM across navigations: clear any open reveal.
      { const rev = $('vantReveal'); if (rev && !rev.classList.contains('hidden')) { $('vantRevealSnippet').textContent = ''; rev.classList.add('hidden'); } }
      const r = parseRoute(location.hash);
      if (r.view === 'overview') { setTabs('overview'); show('viewOverview'); refreshOverview(); }
      else if (r.view === 'graphs') { gridScope = r.path || ''; setTabs('graphs'); show('viewGraphs'); renderScope(); renderTree(); renderGridPanels(); refreshGrid(); }
      else if (r.view === 'stack') { setTabs('stack'); show('viewStack'); renderStack(r.name); }
      else if (r.view === 'zoom') { setTabs('zoom'); show('viewZoom'); renderZoom(r.name, r.range); }
      else if (r.view === 'vantages') { setTabs('vantages'); show('viewVantages'); renderVantages(); }
      else if (r.view === 'config') { setTabs('config'); show('viewConfig'); renderConfig(); }
      $('statusText').textContent = (r.view === 'stack' || r.view === 'zoom') ? r.name : $('statusText').textContent;
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
      // meta handling, so the band and #zoomMeta never disagree about which vantage is focused.
      if (s && !s.unsupported && s.buckets.length >= 2) $('zoomMeta').innerHTML = metaHtml(s);
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
      const ll = $('lossLegend');
      for (const b of Smoke.LOSS_COLORS) { const s = document.createElement('span'); s.className = 'swatch'; s.innerHTML = '<i style="background:' + b.color + '"></i>' + b.label; ll.appendChild(s); }
      refreshKey();
    })();
    function refreshKey() { const sk = $('smokeKey'); if (!sk) return; sk.innerHTML = ''; const dark = Smoke.readVars().dark; for (let k = 1; k <= 6; k++) { const i = document.createElement('i'); i.style.background = Smoke.smokeGray(k, 6, dark); sk.appendChild(i); } }

    // ---- refresh cadences (only the visible view does work) ----
    let rt; window.addEventListener('resize', () => { clearTimeout(rt); rt = setTimeout(rerender, 140); });
    setInterval(() => { if (currentView() === 'graphs') refreshGrid(); }, 5000);
    setInterval(() => { if (currentView() === 'overview') refreshOverview(); }, 15000);
    setInterval(() => { const v = currentView(); if (v === 'stack') renderStack(curTarget); else if (v === 'zoom' && !(zoomState && zoomState.custom)) { const r = parseRoute(location.hash); renderZoom(r.name, r.range); } }, 30000);

    themeLabel();
    route();
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();

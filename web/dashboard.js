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
    if (h === 'graphs') return { view: 'graphs' };
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

  window.Dash = { RANGES, RANGE_ORDER, parseRoute, mergeSeries, gridSince, fetchJSON };

  // ---------------------------------------------------------------- init (DOM) --
  function init() {
    const $ = (id) => document.getElementById(id);
    const fmt = (v, d) => (v == null || isNaN(v)) ? '--' : v.toFixed(d);
    const esc = (s) => String(s).replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
    const enc = encodeURIComponent;

    // ---- data fetch for one range (raw series, or a server-windowed rollup band) ----
    async function fetchRange(name, key) {
      const R = RANGES[key];
      if (R.mode === 'raw') {
        const r = await fetch('/api/series?target=' + enc(name) + '&window=' + R.window, { cache: 'no-store' });
        if (!r.ok) return null;
        return Smoke.fromApiSeries(await r.json());
      }
      // Bound the rollup to the range window server-side (Go duration, e.g. 240h for
      // 10 days) so we don't fetch the target's full retained history each refresh.
      const r = await fetch('/api/rollup?target=' + enc(name) + '&res=' + R.res + '&window=' + (R.days * 24) + 'h', { cache: 'no-store' });
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
    function renderInto(canvas, s, R, height) {
      if (s && s.unsupported) { drawNote(canvas, 'needs the TimescaleDB store (-dsn -downsample)', height); return; }
      if (!s || s.buckets.length < 2) { drawNote(canvas, 'collecting…', height); return; }
      // Fixed wall-clock domain [now-windowMs, now]. t1 extends to the newest sample if
      // the client clock lags the server, so a fresh sample never clamps to the edge;
      // t0 anchors to the selected range so the axis labels stay literally correct.
      const lastT = s.buckets[s.buckets.length - 1].t;
      const t1 = Math.max(Date.now(), Number.isFinite(lastT) ? lastT : 0);
      const t0 = R.windowMs ? t1 - R.windowMs : undefined;
      Smoke.render(canvas, s, { height, band: R.mode === 'band', xlabels: R.xl, t0, t1: t0 == null ? undefined : t1 });
    }
    function metaHtml(s) {
      const st = Smoke.seriesStats(s); const lcls = st.lossAvg > 2 ? 'bad' : st.lossAvg > 0.5 ? 'warn' : '';
      return '<span class="stat"><span class="k">median avg</span><span class="v">' + fmt(st.medAvg, 1) + ' ms</span></span>' +
             '<span class="stat"><span class="k">median max</span><span class="v">' + fmt(st.medMax, 1) + ' ms</span></span>' +
             '<span class="stat"><span class="k">loss avg</span><span class="v ' + lcls + '">' + fmt(st.lossAvg, 2) + ' %</span></span>';
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
        // Reconcile ONLY against an authoritative target list (the fetch above succeeded):
        // drop panels for targets no longer reported (e.g. removed on a SIGHUP reload). A
        // failed /api/targets returned early, so a transient 503 never blanks the grid (#2).
        const live = new Set(targets.map((t) => t.name));
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
        await Promise.all(targets.map(async (t) => {
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
          renderInto(p.canvas, p.series, RANGES['3h'], 170);
        }));
        if (bulk) gridLoaded = true;
      } finally { gridBusy = false; }
    }

    // ---- Drill-down: stack (all four) + zoom (one) ----
    let curTarget = null;
    const stackCanvases = []; // {canvas, series, R}
    let zoomState = null;     // {canvas, series, R}
    async function renderStack(name) {
      curTarget = name; stackCanvases.length = 0;
      $('stackTitle').innerHTML = esc(name);
      const grid = $('stackGrid'); grid.innerHTML = '';
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
      await Promise.all(cells.map(async (c) => {
        let s = null; try { s = await fetchRange(name, c.key); } catch (e) { /* transient */ }
        stackCanvases.push({ canvas: c.canvas, series: s, R: c.R });
        if (s && !s.unsupported && s.buckets.length >= 2) c.meta.innerHTML = metaHtml(s);
        renderInto(c.canvas, s, c.R, 170);
      }));
    }
    async function renderZoom(name, range) {
      curTarget = name; const R = RANGES[range];
      $('zoomTitle').innerHTML = esc(name) + ' <span class="reslabel">· ' + R.label + '</span>';
      $('zoomMeta').innerHTML = ''; $('zoomRes').textContent = '';
      const canvas = $('zoomCanvas');
      let s = null; try { s = await fetchRange(name, range); } catch (e) { /* transient */ }
      zoomState = { canvas, series: s, R };
      if (s && !s.unsupported && s.buckets.length >= 2) {
        $('zoomMeta').innerHTML = metaHtml(s);
        $('zoomRes').textContent = 'resolution: ' + R.desc + ' · ' + s.buckets.length + (R.mode === 'raw' ? ' rounds' : ' buckets');
      }
      renderInto(canvas, s, R, 360);
    }

    // ---- routing ----
    function show(id) { for (const v of ['viewOverview', 'viewGraphs', 'viewStack', 'viewZoom']) $(v).classList.toggle('hidden', v !== id); }
    function setTabs(view) {
      const g = (view === 'graphs' || view === 'stack' || view === 'zoom');
      $('tabOverview').setAttribute('aria-selected', String(view === 'overview'));
      $('tabGraphs').setAttribute('aria-selected', String(g));
    }
    function currentView() { return parseRoute(location.hash).view; }
    function route() {
      const r = parseRoute(location.hash);
      if (r.view === 'overview') { setTabs('overview'); show('viewOverview'); refreshOverview(); }
      else if (r.view === 'graphs') { setTabs('graphs'); show('viewGraphs'); refreshGrid(); }
      else if (r.view === 'stack') { setTabs('stack'); show('viewStack'); renderStack(r.name); }
      else if (r.view === 'zoom') { setTabs('zoom'); show('viewZoom'); renderZoom(r.name, r.range); }
      $('statusText').textContent = (r.view === 'stack' || r.view === 'zoom') ? r.name : $('statusText').textContent;
      window.scrollTo(0, 0);
    }
    function nav(hash) { if (location.hash.replace(/^#/, '') !== hash) history.pushState(null, '', '#' + hash); route(); }
    window.addEventListener('popstate', route);

    // ---- events ----
    $('tabOverview').addEventListener('click', () => nav('overview'));
    $('tabGraphs').addEventListener('click', () => nav('graphs'));
    $('backStack').addEventListener('click', () => { if (history.length > 1) history.back(); else nav('graphs'); });
    $('backZoom').addEventListener('click', () => { if (history.length > 1) history.back(); else nav('target=' + enc(curTarget || '')); });
    $('worstSeg').addEventListener('click', (e) => { const b = e.target.closest('button'); if (!b) return; worstBy = b.dataset.by; document.querySelectorAll('#worstSeg button').forEach((x) => x.setAttribute('aria-pressed', String(x === b))); refreshWorst(); });
    document.addEventListener('click', (e) => {
      const sp = e.target.closest('.spanel'); if (sp) { nav('target=' + enc(curTarget) + '&range=' + sp.dataset.range); return; }
      const g = e.target.closest('.gpanel'); if (g) { nav('target=' + enc(g.dataset.target)); return; }
      const who = e.target.closest('.who[data-target]'); if (who) { nav('target=' + enc(who.dataset.target)); }
    });

    // ---- theme ----
    const btn = $('themeBtn');
    const curTheme = () => document.documentElement.getAttribute('data-theme') || (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light');
    function themeLabel() { const d = curTheme() === 'dark'; $('themeIcon').textContent = d ? '☾' : '☀'; $('themeLabel').textContent = d ? 'Dark' : 'Light'; }
    function rerender() {
      const v = currentView();
      if (v === 'graphs') { for (const p of panels.values()) if (p.series) renderInto(p.canvas, p.series, RANGES['3h'], 170); }
      else if (v === 'stack') { for (const c of stackCanvases) renderInto(c.canvas, c.series, c.R, 170); }
      else if (v === 'zoom' && zoomState) { renderInto(zoomState.canvas, zoomState.series, zoomState.R, 360); }
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
    setInterval(() => { const v = currentView(); if (v === 'stack') renderStack(curTarget); else if (v === 'zoom') { const r = parseRoute(location.hash); renderZoom(r.name, r.range); } }, 30000);

    themeLabel();
    route();
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();

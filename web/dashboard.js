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
  const RANGES = {
    '3h':  { mode: 'raw',  window: '3h',  label: 'Last 3 hours',  desc: 'per-round smoke', xl: ['-3h', '-2h', '-1h', 'now'] },
    '30h': { mode: 'raw',  window: '30h', label: 'Last 30 hours', desc: 'per-round smoke', xl: ['-30h', '-20h', '-10h', 'now'] },
    '10d': { mode: 'band', res: '1h', days: 10,  label: 'Last 10 days',  desc: 'hourly band', xl: ['-10d', '', '', 'now'] },
    '400d':{ mode: 'band', res: '1d', days: 400, label: 'Last 400 days', desc: 'daily band',  xl: ['-400d', '', '', 'now'] },
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

  window.Dash = { RANGES, RANGE_ORDER, parseRoute };

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
      Smoke.render(canvas, s, { height, band: R.mode === 'band', xlabels: R.xl });
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
      try { rows = (await (await fetch('/api/charts?by=' + worstBy + '&n=8', { cache: 'no-store' })).json()).charts || []; }
      catch (e) { return; }
      if (!rows.length) { list.innerHTML = '<li class="empty-row">no data yet</li>'; return; }
      list.innerHTML = rows.map((c, i) => {
        let val, cls = '';
        if (worstBy === 'loss') { val = fmt(c.loss_pct, 1) + ' %'; cls = c.loss_pct > 2 ? 'bad' : c.loss_pct > 0.5 ? 'warn' : ''; }
        else if (worstBy === 'median') { val = fmt(c.median_ms, 1) + ' ms'; }
        else { val = '± ' + fmt(c.stddev_ms, 1) + ' ms'; }
        return '<li><span class="n">' + (i + 1) + '</span><span class="who" data-target="' + esc(c.name) + '">' + esc(c.name) +
          '<span class="pk">' + esc(c.probe) + '</span></span><span class="val ' + cls + '">' + val + '</span></li>';
      }).join('');
    }
    const SLA_WINDOW = '24h';
    function humanSpan(sec) { if (sec < 5400) return Math.max(1, Math.round(sec / 60)) + 'm'; if (sec < 172800) return (sec / 3600).toFixed(sec < 36000 ? 1 : 0) + 'h'; return Math.round(sec / 86400) + 'd'; }
    async function refreshSLA() {
      const list = $('slaList');
      let rows;
      try { rows = (await (await fetch('/api/sla?window=' + SLA_WINDOW, { cache: 'no-store' })).json()).targets || []; }
      catch (e) { return; }
      const sub = $('slaWindow'); if (sub) sub.textContent = 'last ' + SLA_WINDOW;
      if (!rows.length) { list.innerHTML = '<li class="empty-row">no data yet</li>'; return; }
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
    }
    let overviewBusy = false;
    async function refreshOverview() {
      if (overviewBusy) return; overviewBusy = true;
      try { await Promise.all([refreshWorst(), refreshSLA()]); $('statusText').textContent = 'live · updated ' + new Date().toLocaleTimeString(); }
      finally { overviewBusy = false; }
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
    let gridBusy = false;
    async function refreshGrid() {
      if (gridBusy) return; gridBusy = true;
      try {
        let targets;
        try { targets = (await (await fetch('/api/targets', { cache: 'no-store' })).json()).targets || []; }
        catch (e) { $('statusText').textContent = 'collector unreachable'; return; }
        $('statusText').textContent = targets.length + ' targets · updated ' + new Date().toLocaleTimeString();
        // Reconcile: drop panels for targets no longer reported (e.g. removed on a
        // SIGHUP reload), so an open Graphs tab doesn't keep showing stale ones
        // (CODE_REVIEW #5).
        const live = new Set(targets.map((t) => t.name));
        for (const [name, p] of panels) {
          if (!live.has(name)) { p.el.remove(); panels.delete(name); }
        }
        await Promise.all(targets.map(async (t) => {
          const p = ensurePanel(t);
          try {
            const s = await fetchRange(t.name, '3h');
            if (!s || s.unsupported) return;
            p.series = s;
            const st = Smoke.seriesStats(s); const lcls = st.lossAvg > 2 ? 'bad' : st.lossAvg > 0.5 ? 'warn' : '';
            p.meta.innerHTML = '<span class="stat"><span class="k">median</span><span class="v">' + fmt(st.medAvg, 1) + ' ms</span></span>' +
              '<span class="stat"><span class="k">loss</span><span class="v ' + lcls + '">' + fmt(st.lossAvg, 2) + ' %</span></span>';
            renderInto(p.canvas, s, RANGES['3h'], 170);
          } catch (e) { /* transient */ }
        }));
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

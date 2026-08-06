// Smoke — the canvas renderer for SmokePing-style latency-distribution graphs.
// Shared by the synthetic POC (smoke-poc.html) and the live dashboard (index.html).
// A "series" is { buckets: [{ centered:[…ms|NaN], samples:[…ms], lost, median, pings }], N }.
// Each bucket carries its own `pings` (ping count) so rounds with different N render correctly.
// Values are in milliseconds. Reproduces codemap §05: nested percentile bands
// darkening toward the median + an 8-bucket loss-colored median line.
window.Smoke = (function () {
  // SmokePing loss colour ramp, keyed by count lost of N (§05).
  const LOSS_COLORS = [
    { maxpct: 0, color: '#26ff00', label: '0' },
    { maxpct: 1, color: '#00b8ff', label: '≤1%' },
    { maxpct: 5, color: '#0059ff', label: '≤5%' },
    { maxpct: 10, color: '#7e00ff', label: '≤10%' },
    { maxpct: 25, color: '#ff00ff', label: '≤25%' },
    { maxpct: 50, color: '#ff5500', label: '≤50%' },
    { maxpct: 99.9, color: '#ff0000', label: '<100%' },
    { maxpct: 100, color: '#a00000', label: '100%' },
  ];
  function lossColorPct(pct) {
    if (pct <= 0) return LOSS_COLORS[0].color;
    if (pct >= 100) return '#a00000';
    for (const b of LOSS_COLORS) if (pct <= b.maxpct) return b.color;
    return '#ff0000';
  }
  function lossColor(lost, N) { return lossColorPct(N > 0 ? (lost / N) * 100 : 0); }

  function readVars() {
    const cs = getComputedStyle(document.documentElement);
    const dark =
      (document.documentElement.getAttribute('data-theme') ||
        (matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light')) === 'dark';
    return {
      plotBg: cs.getPropertyValue('--plot-bg').trim(),
      grid: cs.getPropertyValue('--plot-grid').trim(),
      axis: cs.getPropertyValue('--plot-axis').trim(),
      frame: cs.getPropertyValue('--plot-frame').trim(),
      medianBase: cs.getPropertyValue('--median-base').trim(),
      dark,
    };
  }

  // SmokePing gray ramp: int(190/half*(half-k))+50 -> outer light, centre dark.
  // Inverted for dark ground so the dense core stays the bright, prominent part.
  function smokeGray(k, half, dark) {
    const g = Math.round((190 / half) * (half - k)) + 50;
    if (!dark) return `rgb(${g},${g},${g})`;
    const d = 255 - g;
    return `rgb(${Math.round(d * 0.86)},${Math.round(d * 0.94)},${d})`;
  }

  function robustMax(s) {
    const all = [];
    for (const b of s.buckets) for (const v of b.samples) all.push(v);
    all.sort((a, b) => a - b);
    if (!all.length) return 1;
    const p = all[Math.min(all.length - 1, Math.floor(all.length * 0.965))];
    return Math.max(1, p * 1.18);
  }

  function niceStep(range, target) {
    const raw = range / target,
      mag = Math.pow(10, Math.floor(Math.log10(raw)));
    const n = raw / mag;
    const s = n < 1.5 ? 1 : n < 3 ? 2 : n < 7 ? 5 : 10;
    return s * mag;
  }

  // Per-bucket ping count: rounds can differ in N (a reload retuned pings), so each
  // bucket's loss fraction and band depth use its own denominator, never one
  // series-wide N — which would misreport loss and drop bands for shorter rounds.
  function bucketPings(b) { return b.pings || b.centered.length; }

  function seriesStats(s) {
    // Weight each bucket by the number of rounds it aggregates (1 for a raw round,
    // `rounds` for a rollup bucket), so a sparse bucket doesn't count the same as a
    // full one in the loss/median averages.
    let mwsum = 0, mmax = 0, mw = 0, lsum = 0, wsum = 0;
    for (const b of s.buckets) {
      const w = b.rounds || 1;
      if (!isNaN(b.median)) { mwsum += b.median * w; mmax = Math.max(mmax, b.median); mw += w; }
      const bn = bucketPings(b);
      if (bn > 0) { lsum += (b.lost / bn) * w; wsum += w; }
    }
    return {
      medAvg: mw ? mwsum / mw : NaN,
      medMax: mw ? mmax : NaN, // NaN (not 0) when every bucket was fully lost
      lossAvg: wsum ? (lsum / wsum) * 100 : 0,
    };
  }

  function render(canvas, s, opts) {
    const V = readVars();
    const dpr = Math.max(1, window.devicePixelRatio || 1);
    const cssW = canvas.clientWidth, cssH = opts.height;
    canvas.style.height = cssH + 'px';
    canvas.width = Math.round(cssW * dpr);
    canvas.height = Math.round(cssH * dpr);
    const ctx = canvas.getContext('2d');
    ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
    ctx.clearRect(0, 0, cssW, cssH);

    const mL = 48, mR = 12, mT = 10, mB = 22;
    const pw = cssW - mL - mR, ph = cssH - mT - mB;
    const n = s.buckets.length;
    if (n < 2) return;
    // Band depth is per bucket (each round's own ping count); the shared gray ramp
    // keys off the deepest bucket so a given shade always means the same depth.
    let maxHalf = 0;
    for (let i = 0; i < n; i++) maxHalf = Math.max(maxHalf, Math.floor(bucketPings(s.buckets[i]) / 2));
    const yMax = opts.yMax || robustMax(s);
    // X by wall-clock time when a domain (t0/t1 epoch ms) and per-bucket `.t` are
    // supplied: short data floats at its true position and gaps land at the right
    // place. Without them (smoke-poc.html) fall back to even array-index spacing.
    const bk = s.buckets;
    const useTime = opts.t0 != null && opts.t1 != null && opts.t1 > opts.t0 &&
      bk.every((b) => typeof b.t === 'number' && !isNaN(b.t));
    const clamp01 = (f) => (f < 0 ? 0 : f > 1 ? 1 : f);
    const X = useTime
      ? (i) => mL + pw * clamp01((bk[i].t - opts.t0) / (opts.t1 - opts.t0))
      : (i) => mL + pw * (i / (n - 1));
    const Y = (v) => mT + ph * (1 - Math.min(v, yMax) / yMax);
    const colW = Math.ceil(pw / (n - 1)) + 1;

    // A time gap wider than the inferred cadence breaks lines/bands, so a collector/DB
    // outage renders as a blank span, never a straight line bridging it. Cadence =
    // median of positive consecutive `.t` diffs; gap = diff > gapFactor*cadence
    // (default 1.5 — RRD-style "a round was missed"). Disabled without a time domain.
    let gapMs = Infinity;
    if (useTime && n >= 2) {
      const diffs = [];
      for (let i = 1; i < n; i++) { const d = bk[i].t - bk[i - 1].t; if (d > 0) diffs.push(d); }
      if (diffs.length) {
        diffs.sort((a, b) => a - b);
        gapMs = diffs[Math.floor(diffs.length / 2)] * (opts.gapFactor || 1.5);
      }
    }
    const isGap = (i) => useTime && i >= 1 && (bk[i].t - bk[i - 1].t) > gapMs;

    ctx.fillStyle = V.plotBg;
    ctx.fillRect(mL, mT, pw, ph);

    const step = niceStep(yMax, 4);
    ctx.font = '10px ui-monospace, Menlo, monospace';
    ctx.textBaseline = 'middle';
    for (let g = 0; g <= yMax + 0.001; g += step) {
      const y = Y(g);
      ctx.strokeStyle = V.grid; ctx.lineWidth = 1;
      ctx.beginPath(); ctx.moveTo(mL, Math.round(y) + 0.5); ctx.lineTo(mL + pw, Math.round(y) + 0.5); ctx.stroke();
      ctx.fillStyle = V.axis; ctx.textAlign = 'right';
      ctx.fillText(g >= 100 ? g.toFixed(0) : g.toFixed(g < 10 ? 1 : 0), mL - 6, y);
    }
    ctx.save(); ctx.translate(11, mT + ph / 2); ctx.rotate(-Math.PI / 2);
    ctx.textAlign = 'center'; ctx.fillStyle = V.axis; ctx.fillText('ms', 0, 0); ctx.restore();

    // smooth-fill a run of {x,yLo,yHi} columns as one polygon (a single column
    // falls back to a rectangle). Shared by the smoke stack and the band mode.
    const fillRun = (run) => {
      if (run.length >= 2) {
        ctx.beginPath();
        ctx.moveTo(run[0].x, run[0].yHi);
        for (let p = 1; p < run.length; p++) ctx.lineTo(run[p].x, run[p].yHi);
        for (let p = run.length - 1; p >= 0; p--) ctx.lineTo(run[p].x, run[p].yLo);
        ctx.closePath(); ctx.fill();
      } else if (run.length === 1) {
        ctx.fillRect(run[0].x - colW / 2, run[0].yHi, colW, Math.max(1, run[0].yLo - run[0].yHi));
      }
    };

    if (opts.band) {
      // Aggregated tiers (hourly/daily) have no per-round distribution — only a
      // min/avg/max per bucket. Draw one soft translucent min->max range-area
      // instead of the nested smoke stack, whose single min->max pair renders as
      // a muddy near-black blob. The avg still gets the loss-tinted median line.
      ctx.fillStyle = V.axis;
      ctx.globalAlpha = V.dark ? 0.34 : 0.22;
      let run = [];
      for (let i = 0; i < n; i++) {
        if (isGap(i)) { fillRun(run); run = []; } // time gap: don't span the range-area across it
        const smp = s.buckets[i].samples;
        if (!smp || smp.length === 0) { fillRun(run); run = []; continue; } // fully-lost bucket = gap
        let lo = smp[0], hi = smp[0];
        for (const v of smp) { if (v < lo) lo = v; if (v > hi) hi = v; }
        run.push({ x: X(i), yLo: Y(lo), yHi: Y(hi) });
      }
      fillRun(run);
      ctx.globalAlpha = 1;
    } else {
      // smoke bands, outer(light)->inner(dark), smooth over contiguous runs
      for (let k = 1; k <= maxHalf; k++) {
        ctx.fillStyle = smokeGray(k, maxHalf, V.dark);
        let run = [];
        for (let i = 0; i < n; i++) {
          if (isGap(i)) { fillRun(run); run = []; } // time gap: break the smoke band here
          const c = s.buckets[i].centered;
          const bn = bucketPings(s.buckets[i]);
          if (k > Math.floor(bn / 2)) { fillRun(run); run = []; continue; } // this bucket has no k-th band
          const lo = c[k - 1], hi = c[bn - k];
          if (isNaN(lo) || isNaN(hi)) { fillRun(run); run = []; continue; }
          run.push({ x: X(i), yLo: Y(lo), yHi: Y(hi) });
        }
        fillRun(run);
      }
    }

    // median base line
    ctx.lineWidth = 1.4; ctx.strokeStyle = V.medianBase; ctx.beginPath();
    let started = false;
    for (let i = 0; i < n; i++) {
      const m = s.buckets[i].median;
      if (isNaN(m)) { started = false; continue; }
      const x = X(i), y = Y(m);
      if (!started || isGap(i)) { ctx.moveTo(x, y); started = true; } else ctx.lineTo(x, y); // pen-up across a gap
    }
    ctx.stroke();

    // median coloured by loss (incl. green for zero-loss), 2px segments
    ctx.lineWidth = 2.2;
    for (let i = 1; i < n; i++) {
      const a = s.buckets[i - 1], b = s.buckets[i];
      if (isNaN(a.median) || isNaN(b.median) || isGap(i)) continue; // no coloured segment across a gap
      const pct = Math.max((a.lost / bucketPings(a)) * 100, (b.lost / bucketPings(b)) * 100);
      ctx.strokeStyle = lossColorPct(pct);
      ctx.beginPath(); ctx.moveTo(X(i - 1), Y(a.median)); ctx.lineTo(X(i), Y(b.median)); ctx.stroke();
    }

    // 100%-loss ticks at baseline
    ctx.fillStyle = '#a00000';
    for (let i = 0; i < n; i++) if (s.buckets[i].lost >= bucketPings(s.buckets[i])) ctx.fillRect(Math.round(X(i)) - 1, mT + ph - 3, 2, 3);

    ctx.strokeStyle = V.frame; ctx.lineWidth = 1; ctx.strokeRect(mL + 0.5, mT + 0.5, pw - 1, ph - 1);

    ctx.fillStyle = V.axis; ctx.textAlign = 'center'; ctx.textBaseline = 'top';
    const labels = opts.xlabels || ['', '', '', 'now'];
    for (let j = 0; j < labels.length; j++) {
      const x = mL + pw * (j / (labels.length - 1));
      ctx.fillText(labels[j], Math.min(Math.max(x, mL + 12), mL + pw - 12), mT + ph + 5);
    }
  }

  // Adapt an /api/series response ({rounds:[{median_ms,loss,pings,rtts_ms}]}) into a series.
  function fromApiSeries(resp) {
    const rounds = resp.rounds || [];
    let N = 0;
    const buckets = rounds.map((r) => {
      const centered = (r.rtts_ms || []).map((v) => (v == null ? NaN : v));
      const samples = centered.filter((v) => !isNaN(v));
      const pings = r.pings || centered.length; // retain each round's own N
      N = Math.max(N, pings);
      const t = r.t == null ? NaN : Date.parse(r.t); // wall-clock ms for time-scaled X + gap breaks
      return { centered, samples, lost: r.loss || 0, median: r.median_ms == null ? NaN : r.median_ms, pings, t };
    });
    return { buckets, N: N || (buckets[0] ? buckets[0].centered.length : 0) };
  }

  // Adapt an /api/rollup response into a series. The hourly tier has no per-round
  // distribution — only the median's avg/min/max and a loss fraction per bucket —
  // so each bucket becomes a single min→max band (N=2) with the avg as the median
  // line. loss_pct is mapped onto "lost of N=2" so lossColor tints it identically
  // to the raw view. This mirrors SmokePing's long-range RRD min/max consolidation.
  function fromApiRollup(resp) {
    const raw = resp.buckets || [];
    const buckets = raw.map((x) => {
      const lo = x.median_min_ms == null ? NaN : x.median_min_ms;
      const hi = x.median_max_ms == null ? NaN : x.median_max_ms;
      const centered = [lo, hi];
      return {
        centered,
        samples: centered.filter((v) => !isNaN(v)),
        lost: (x.loss_pct || 0) / 50, // 0..100% -> 0..2 lost of N=2
        median: x.median_avg_ms == null ? NaN : x.median_avg_ms,
        pings: 2, // min→max band; loss expressed as "lost of 2"
        rounds: x.rounds || 1, // raw rounds this bucket aggregates — weights seriesStats
        t: x.bucket == null ? NaN : Date.parse(x.bucket), // wall-clock ms for time-scaled X + gap breaks
      };
    });
    return { buckets, N: 2, resolution: resp.resolution || '1h' };
  }

  return { LOSS_COLORS, lossColor, smokeGray, robustMax, render, seriesStats, readVars, fromApiSeries, fromApiRollup };
})();

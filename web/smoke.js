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

  // robustMax is the y-axis top for the unison (shared-scale) grid — the same range robustRange
  // computes for a single latency panel, so a median peak is never clipped there either.
  function robustMax(s) { return robustRange(s, false)[1]; }

  // robustRange returns the [yMin, yMax] the y-axis should span. A latency series (signed=false) is
  // 0-based and floored at 1ms. A SIGNED series (NTP clock offset; explicitly flagged by the caller,
  // since it can be legitimately all-positive) always gets a tight range that includes zero, padded,
  // with NO 1ms floor — so a µs-scale offset fills the plot instead of collapsing to a sliver.
  //
  // The SAMPLE band is scaled by the 3.5th/96.5th percentiles so a single outlier ping doesn't set
  // the scale. But the MEDIAN line is the primary signal and must never be clipped: a brief latency
  // spike's few samples fall in that trimmed top-tail, yet its per-round median is real and robust
  // (one wild ping can't move a median). So fold the median extremes into the range — the spike
  // shows, while the sample percentiles still keep a lone outlier ping from blowing up the band
  // scale. (Reported symptom: graph peaks getting cut off at the top frame.)
  function robustRange(s, signed) {
    const all = [];
    let medLo = Infinity, medHi = -Infinity;
    for (const b of s.buckets) {
      for (const v of b.samples) all.push(v);
      if (b.median != null && !isNaN(b.median)) {
        if (b.median < medLo) medLo = b.median;
        if (b.median > medHi) medHi = b.median;
      }
    }
    if (!all.length) return signed ? [-1e-3, 1e-3] : [0, 1];
    all.sort((a, b) => a - b);
    const q = (f) => all[Math.min(all.length - 1, Math.max(0, Math.floor(all.length * f)))];
    let hi = q(0.965), lo = q(0.035);
    if (medHi > -Infinity) { hi = Math.max(hi, medHi); lo = Math.min(lo, medLo); }
    if (!signed) return [0, Math.max(1, hi * 1.18)]; // latency: 0-based, 1ms floor
    const pad = Math.max((hi - lo) * 0.15, 1e-4);    // offset: include zero, pad both ends, no floor
    return [Math.min(0, lo - pad), Math.max(0, hi + pad)];
  }

  function niceStep(range, target) {
    const raw = range / target,
      mag = Math.pow(10, Math.floor(Math.log10(raw)));
    const n = raw / mag;
    const s = n < 1.5 ? 1 : n < 3 ? 2 : n < 7 ? 5 : 10;
    return s * mag;
  }

  // gapThreshold returns the largest consecutive-timestamp gap (ms) that is NOT a
  // break; a diff beyond it is an outage the renderer pens up across. Cadence is a low
  // quantile (~25th percentile) of the positive consecutive `.t` diffs, times gapFactor
  // (default 1.5, RRD-style "a round was missed"). A low quantile — not the median —
  // keeps a few large gaps (or a sparse two-point series whose single hole would BE the
  // median) from inflating the cadence and hiding the outage. Infinity when there aren't
  // two timestamps to diff (nothing can be a gap).
  function gapThreshold(buckets, gapFactor) {
    const diffs = [];
    for (let i = 1; i < buckets.length; i++) {
      const d = buckets[i].t - buckets[i - 1].t;
      if (d > 0) diffs.push(d);
    }
    if (!diffs.length) return Infinity;
    diffs.sort((a, b) => a - b);
    const cadence = diffs[Math.floor(diffs.length * 0.25)]; // low-quantile (nearest-rank p25)
    return cadence * (gapFactor || 1.5);
  }

  // Per-bucket ping count: rounds can differ in N (a reload retuned pings), so each
  // bucket's loss fraction and band depth use its own denominator, never one
  // series-wide N — which would misreport loss and drop bands for shorter rounds.
  function bucketPings(b) { return b.pings || b.centered.length; }

  function seriesStats(s) {
    // Weight each bucket by the rounds it aggregates (1 for a raw round). Loss is weighted
    // by TOTAL rounds; the median by the rounds that actually produced a median (not fully
    // lost). During an outage the two differ, and weighting the median by total rounds would
    // let one surviving round in a mostly-lost bucket count as a full bucket's median (P2-6).
    // mmax starts at -Infinity, not 0: an all-negative (clock-offset) series has a real negative
    // maximum, and a 0 seed would fabricate medMax=0 via Math.max(0, negative). mmax is only read
    // when mw>0, i.e. after at least one real median updated it, so -Infinity never escapes (L6).
    let mwsum = 0, mmax = -Infinity, mw = 0, lsum = 0, wsum = 0;
    for (const b of s.buckets) {
      const lossW = b.rounds || 1;
      const medW = b.medianRounds != null ? b.medianRounds : lossW;
      if (!isNaN(b.median) && medW > 0) { mwsum += b.median * medW; mmax = Math.max(mmax, b.median); mw += medW; }
      const bn = bucketPings(b);
      if (bn > 0) { lsum += (b.lost / bn) * lossW; wsum += lossW; }
    }
    return {
      medAvg: mw ? mwsum / mw : NaN,
      medMax: mw ? mmax : NaN, // NaN (not 0) when every bucket was fully lost
      lossAvg: wsum ? (lsum / wsum) * 100 : 0,
    };
  }

  // Plot margins (CSS px): left gutter for the y-axis, right/top/bottom padding. Shared
  // with the dashboard's drag-to-zoom so pixel<->time mapping matches what render draws.
  const MARGINS = { mL: 48, mR: 12, mT: 10, mB: 22 };

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

    const { mL, mR, mT, mB } = MARGINS;
    const pw = cssW - mL - mR, ph = cssH - mT - mB;
    const n = s.buckets.length;
    if (n < 2) return;
    // Band depth is per bucket (each round's own ping count); the shared gray ramp
    // keys off the deepest bucket so a given shade always means the same depth.
    let maxHalf = 0;
    for (let i = 0; i < n; i++) maxHalf = Math.max(maxHalf, Math.floor(bucketPings(s.buckets[i]) / 2));
    // Y range. Latency series stay 0-based (yMin 0); a SIGNED series (NTP offset) gets a
    // zero-centered range with a baseline. A pinned yMax (unison grid) is honored only for
    // non-negative data — a signed series always uses its own range so negatives aren't clipped.
    const signed = !!opts.signed;
    let [yMin, yMax] = robustRange(s, signed);
    if (opts.yMax != null && !signed) { yMin = 0; yMax = opts.yMax; }
    // widen for overlaid vantages (unless a pinned latency max already fixed the scale)
    if (!(opts.yMax != null && !signed) && opts.overlays && opts.overlays.length) {
      for (const o of opts.overlays) if (o && o.series) {
        const [oLo, oHi] = robustRange(o.series, signed);
        yMin = Math.min(yMin, oLo); yMax = Math.max(yMax, oHi);
      }
    }
    if (yMax <= yMin) yMax = yMin + 1; // guard a degenerate (flat) range
    const signedAxis = yMin < 0;
    const ySpan = yMax - yMin;
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
    // wall-clock time->x for overlay series, which carry their own buckets/timestamps
    // (not indexed alongside the focused series' bk/n). Valid only in useTime mode.
    const Xt = (t) => mL + pw * clamp01((t - opts.t0) / (opts.t1 - opts.t0));
    // Clamp to [yMin, yMax] so an out-of-scale sample sits on the frame instead of drawing past it
    // (matches the previous top-clamp for latency; adds the bottom for signed offset axes).
    const Y = (v) => mT + ph * (1 - (Math.max(yMin, Math.min(v, yMax)) - yMin) / ySpan);
    const colW = Math.ceil(pw / (n - 1)) + 1;

    // A time gap wider than the inferred cadence breaks lines/bands, so a collector/DB
    // outage renders as a blank span, never a straight line bridging it (gapThreshold).
    const gapMs = useTime ? gapThreshold(bk, opts.gapFactor) : Infinity;
    const isGap = (i) => useTime && i >= 1 && (bk[i].t - bk[i - 1].t) > gapMs;

    ctx.fillStyle = V.plotBg;
    ctx.fillRect(mL, mT, pw, ph);

    const step = niceStep(ySpan, 4);
    // Signed axes (offset) label with an explicit sign and decimals matched to the (often µs-scale)
    // step; a non-negative axis keeps the exact historical latency formatting so those graphs are
    // unchanged. The gridline start snaps to a nice multiple at or below yMin.
    // Decimals track the nice step so a µs-scale offset step (e.g. 0.002 ms) still gets distinct
    // labels instead of every tick rounding to 0.00; capped at 3 so latency-scale steps stay tidy
    // (L5). Only the signed (offset) axis uses this — the non-negative latency axis keeps its exact
    // historical formatting, so those graphs are unchanged.
    const decs = step >= 1 ? 0 : Math.min(3, Math.ceil(-Math.log10(step)));
    const fmtTick = signed
      ? (g) => (signedAxis && g > 1e-9 ? '+' : g < -1e-9 ? '−' : '') + Math.abs(g).toFixed(decs)
      : (g) => (g >= 100 ? g.toFixed(0) : g.toFixed(g < 10 ? 1 : 0));
    ctx.font = '10px ui-monospace, Menlo, monospace';
    ctx.textBaseline = 'middle';
    for (let g = Math.ceil(yMin / step) * step; g <= yMax + step * 0.001; g += step) {
      const y = Y(g);
      ctx.strokeStyle = V.grid; ctx.lineWidth = 1;
      ctx.beginPath(); ctx.moveTo(mL, Math.round(y) + 0.5); ctx.lineTo(mL + pw, Math.round(y) + 0.5); ctx.stroke();
      ctx.fillStyle = V.axis; ctx.textAlign = 'right';
      ctx.fillText(fmtTick(g), mL - 6, y);
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

    // Clip the data (bands + median lines + ticks) to the plot rect. A value above yMax
    // is clamped to the top by Y(), but the WIDTH of the line/tick drawn there would still
    // spill ~1px past the top frame; clipping keeps everything inside the box. The grid,
    // axis labels and frame are drawn outside this save/restore so they aren't clipped.
    ctx.save();
    ctx.beginPath();
    ctx.rect(mL, mT, pw, ph);
    ctx.clip();

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

    // overlay lines: extra per-vantage median-only context, drawn after the band/smoke
    // stack but under the focused median so the primary series stays visually on top.
    // Each overlay carries its own buckets/timestamps and is penned up across its own
    // cadence gaps, independent of the focused series' gaps. Time-scaled X only.
    if (opts.overlays && useTime) {
      ctx.lineWidth = 1.4;
      for (const o of opts.overlays) {
        if (!o || !o.series || !o.series.buckets) continue;
        const ob = o.series.buckets;
        const oGap = gapThreshold(ob, opts.gapFactor);
        ctx.strokeStyle = o.color; ctx.beginPath();
        let ostarted = false;
        for (let i = 0; i < ob.length; i++) {
          const m = ob[i].median;
          if (isNaN(m) || typeof ob[i].t !== 'number' || isNaN(ob[i].t)) { ostarted = false; continue; }
          const gap = i >= 1 && (ob[i].t - ob[i - 1].t) > oGap;
          const x = Xt(ob[i].t), y = Y(m);
          if (!ostarted || gap) { ctx.moveTo(x, y); ostarted = true; } else ctx.lineTo(x, y);
        }
        ctx.stroke();
      }
    }

    // zero baseline — only on a signed (offset) axis, where 0 = perfectly in sync. Drawn over the
    // band but under the median so the offset line reads against it. Dashed + faint to stay subordinate.
    if (signedAxis && yMax > 0) {
      const yz = Math.round(Y(0)) + 0.5;
      ctx.save();
      ctx.setLineDash([4, 3]); ctx.strokeStyle = V.axis; ctx.globalAlpha = 0.65; ctx.lineWidth = 1;
      ctx.beginPath(); ctx.moveTo(mL, yz); ctx.lineTo(mL + pw, yz); ctx.stroke();
      ctx.restore();
    }

    // median base line
    ctx.lineWidth = 1; ctx.strokeStyle = V.medianBase; ctx.beginPath();
    let started = false;
    for (let i = 0; i < n; i++) {
      const m = s.buckets[i].median;
      if (isNaN(m)) { started = false; continue; }
      const x = X(i), y = Y(m);
      if (!started || isGap(i)) { ctx.moveTo(x, y); started = true; } else ctx.lineTo(x, y); // pen-up across a gap
    }
    ctx.stroke();

    // median coloured by loss (incl. green for zero-loss)
    ctx.lineWidth = 1.5;
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

    ctx.restore(); // end plot-rect clip

    ctx.strokeStyle = V.frame; ctx.lineWidth = 1; ctx.strokeRect(mL + 0.5, mT + 0.5, pw - 1, ph - 1);

    ctx.fillStyle = V.axis; ctx.textAlign = 'center'; ctx.textBaseline = 'top';
    const labels = opts.xlabels || ['', '', '', 'now'];
    for (let j = 0; j < labels.length; j++) {
      const x = mL + pw * (j / (labels.length - 1));
      ctx.fillText(labels[j], Math.min(Math.max(x, mL + 12), mL + pw - 12), mT + ph + 5);
    }
    return yMax;
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
        rounds: x.rounds || 1, // total rounds this bucket aggregates — weights loss in seriesStats
        // rounds that produced a median (not fully lost) — weights the median in seriesStats,
        // so a bucket of mostly-lost rounds doesn't count its one surviving median as a full
        // bucket (P2-6). Falls back to total rounds if the field is absent (older hub).
        medianRounds: x.median_rounds == null ? (x.rounds || 1) : x.median_rounds,
        t: x.bucket == null ? NaN : Date.parse(x.bucket), // wall-clock ms for time-scaled X + gap breaks
      };
    });
    return { buckets, N: 2, resolution: resp.resolution || '1h' };
  }

  return { LOSS_COLORS, MARGINS, lossColor, smokeGray, robustMax, robustRange, gapThreshold, render, seriesStats, readVars, fromApiSeries, fromApiRollup };
})();

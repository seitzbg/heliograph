// Smoke — the canvas renderer for SmokePing-style latency-distribution graphs.
// Shared by the synthetic POC (smoke-poc.html) and the live dashboard (index.html).
// A "series" is { buckets: [{ centered:[…ms|NaN], samples:[…ms], lost, median }], N }.
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
  function lossColor(lost, N) {
    if (lost <= 0) return LOSS_COLORS[0].color;
    const pct = (lost / N) * 100;
    if (pct >= 100) return '#a00000';
    for (const b of LOSS_COLORS) if (pct <= b.maxpct) return b.color;
    return '#ff0000';
  }

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

  function seriesStats(s) {
    let msum = 0, mmax = 0, mn = 0, lsum = 0;
    for (const b of s.buckets) {
      if (!isNaN(b.median)) { msum += b.median; mmax = Math.max(mmax, b.median); mn++; }
      lsum += b.lost;
    }
    return { medAvg: mn ? msum / mn : NaN, medMax: mmax, lossAvg: (lsum / (s.buckets.length * s.N)) * 100 };
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
    const n = s.buckets.length, N = s.N, half = Math.floor(N / 2);
    if (n < 2) return;
    const yMax = opts.yMax || robustMax(s);
    const X = (i) => mL + pw * (i / (n - 1));
    const Y = (v) => mT + ph * (1 - Math.min(v, yMax) / yMax);
    const colW = Math.ceil(pw / (n - 1)) + 1;

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

    // smoke bands, outer(light)->inner(dark), smooth over contiguous runs
    for (let k = 1; k <= half; k++) {
      ctx.fillStyle = smokeGray(k, half, V.dark);
      let run = [];
      const flush = () => {
        if (run.length >= 2) {
          ctx.beginPath();
          ctx.moveTo(run[0].x, run[0].yHi);
          for (let p = 1; p < run.length; p++) ctx.lineTo(run[p].x, run[p].yHi);
          for (let p = run.length - 1; p >= 0; p--) ctx.lineTo(run[p].x, run[p].yLo);
          ctx.closePath(); ctx.fill();
        } else if (run.length === 1) {
          ctx.fillRect(run[0].x - colW / 2, run[0].yHi, colW, Math.max(1, run[0].yLo - run[0].yHi));
        }
        run = [];
      };
      for (let i = 0; i < n; i++) {
        const c = s.buckets[i].centered;
        const lo = c[k - 1], hi = c[N - k];
        if (isNaN(lo) || isNaN(hi)) { flush(); continue; }
        run.push({ x: X(i), yLo: Y(lo), yHi: Y(hi) });
      }
      flush();
    }

    // median base line
    ctx.lineWidth = 1.4; ctx.strokeStyle = V.medianBase; ctx.beginPath();
    let started = false;
    for (let i = 0; i < n; i++) {
      const m = s.buckets[i].median;
      if (isNaN(m)) { started = false; continue; }
      const x = X(i), y = Y(m);
      if (!started) { ctx.moveTo(x, y); started = true; } else ctx.lineTo(x, y);
    }
    ctx.stroke();

    // median coloured by loss (incl. green for zero-loss), 2px segments
    ctx.lineWidth = 2.2;
    for (let i = 1; i < n; i++) {
      const a = s.buckets[i - 1], b = s.buckets[i];
      if (isNaN(a.median) || isNaN(b.median)) continue;
      ctx.strokeStyle = lossColor(Math.max(a.lost, b.lost), N);
      ctx.beginPath(); ctx.moveTo(X(i - 1), Y(a.median)); ctx.lineTo(X(i), Y(b.median)); ctx.stroke();
    }

    // 100%-loss ticks at baseline
    ctx.fillStyle = '#a00000';
    for (let i = 0; i < n; i++) if (s.buckets[i].lost >= N) ctx.fillRect(Math.round(X(i)) - 1, mT + ph - 3, 2, 3);

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
      N = Math.max(N, r.pings || 0);
      const centered = (r.rtts_ms || []).map((v) => (v == null ? NaN : v));
      const samples = centered.filter((v) => !isNaN(v));
      return { centered, samples, lost: r.loss || 0, median: r.median_ms == null ? NaN : r.median_ms };
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
      };
    });
    return { buckets, N: 2, resolution: resp.resolution || '1h' };
  }

  return { LOSS_COLORS, lossColor, smokeGray, robustMax, render, seriesStats, readVars, fromApiSeries, fromApiRollup };
})();

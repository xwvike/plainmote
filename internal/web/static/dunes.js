// Ordered Bayer 8x8 dithered desert dunes & sky backdrop.
// Renders a deterministic multi-layer wind-sculpted landscape onto a low-res
// pixel grid (2px CSS cells, pixelated scaling) so every halftone dot stays
// crisp on Retina displays with zero animation loop overhead. As the page
// scrolls upward, the dithered backdrop fades out smoothly while the header
// transitions from transparent sky into frosted glass.

const BAYER8 = [
   0, 32,  8, 40,  2, 34, 10, 42,
  48, 16, 56, 24, 50, 18, 58, 26,
  12, 44,  4, 36, 14, 46,  6, 38,
  60, 28, 52, 20, 62, 30, 54, 22,
   3, 35, 11, 43,  1, 33,  9, 41,
  51, 19, 59, 27, 49, 17, 57, 25,
  15, 47,  7, 39, 13, 45,  5, 37,
  63, 31, 55, 23, 61, 29, 53, 21,
];

const CELL = 2;

function isDarkTheme() {
  const explicit = document.documentElement.dataset.theme;
  if (explicit === "dark") return true;
  if (explicit === "light") return false;
  return window.matchMedia && window.matchMedia("(prefers-color-scheme: dark)").matches;
}

function clamp01(v) {
  return v < 0 ? 0 : v > 1 ? 1 : v;
}

function smoothstep(edge0, edge1, x) {
  const t = clamp01((x - edge0) / (edge1 - edge0));
  return t * t * (3 - 2 * t);
}

// Fast deterministic 2D value noise for clouds, wind ripples, and sand scatter.
function hash2(ix, iy) {
  let n = (ix * 374761393 + iy * 668265263) ^ 0x5bf03635;
  n = Math.imul(n ^ (n >>> 13), 1274126177);
  return ((n ^ (n >>> 16)) >>> 0) / 4294967295;
}

function noise2(x, y) {
  const ix = Math.floor(x);
  const iy = Math.floor(y);
  const fx = x - ix;
  const fy = y - iy;
  const ux = fx * fx * (3 - 2 * fx);
  const uy = fy * fy * (3 - 2 * fy);
  const a = hash2(ix, iy);
  const b = hash2(ix + 1, iy);
  const c = hash2(ix, iy + 1);
  const d = hash2(ix + 1, iy + 1);
  return a + (b - a) * ux + (c - a) * uy + (a - b - c + d) * ux * uy;
}

function fbm(x, y) {
  return (
    0.55 * noise2(x, y) +
    0.28 * noise2(x * 2.03 + 11.7, y * 2.01 + 5.3) +
    0.17 * noise2(x * 4.07 + 23.1, y * 4.03 + 17.9)
  );
}

// Five overlapping wind-sculpted dune ridges in normalized [0..1] canvas coords.
function ridge1(u) {
  // Distant horizon range.
  return 0.23
    - 0.065 * Math.sin(u * Math.PI * 1.15 + 0.4)
    - 0.028 * Math.sin(u * Math.PI * 2.8 - 0.6)
    + 0.012 * Math.cos(u * Math.PI * 5.2 + 0.9);
}

function ridge2(u) {
  // Far-mid sweeping dune rising gracefully toward the left.
  return 0.34
    - 0.085 * Math.sin(u * Math.PI * 1.25 + 1.05)
    + 0.036 * Math.cos(u * Math.PI * 2.55 - 0.2)
    - 0.014 * Math.sin(u * Math.PI * 4.7 + 0.3);
}

function ridge3(u) {
  // Hero desert dune: bold crest sweeping down from upper-right across center.
  return 0.45
    + 0.115 * Math.sin(u * Math.PI * 1.35 - 0.55)
    - 0.052 * Math.cos(u * Math.PI * 2.4 - 0.35)
    + 0.020 * Math.sin(u * Math.PI * 4.1 + 1.2);
}

function ridge4(u) {
  // Mid-foreground counter-sweeping ridge intersecting the valley.
  return 0.58
    - 0.095 * Math.sin(u * Math.PI * 1.15 + 0.55)
    - 0.040 * Math.cos(u * Math.PI * 2.7 + 0.6)
    + 0.014 * Math.sin(u * Math.PI * 4.9 - 0.4);
}

function ridge5(u) {
  // Foreground broad sand bank fading smoothly into mid-page paper.
  return 0.72
    + 0.065 * Math.sin(u * Math.PI * 1.25 - 1.1)
    - 0.028 * Math.cos(u * Math.PI * 2.6 + 0.3);
}

function slopeOf(fn, u) {
  const eps = 0.004;
  return (fn(u + eps) - fn(u - eps)) / (2 * eps);
}

function drawDunes(canvas) {
  const rect = canvas.getBoundingClientRect();
  if (rect.width < 1 || rect.height < 1) return;

  const w = Math.max(1, Math.ceil(rect.width / CELL));
  const h = Math.max(1, Math.ceil(rect.height / CELL));
  if (canvas.width !== w || canvas.height !== h) {
    canvas.width = w;
    canvas.height = h;
  }

  const ctx = canvas.getContext("2d", { alpha: true });
  if (!ctx) return;

  const img = ctx.createImageData(w, h);
  const data = img.data;
  const dark = isDarkTheme();

  // Brand-rooted desert palette with clear, sculptural Bayer dot visibility.
  const skyColor   = dark ? [94, 180, 182]  : [24, 106, 110];
  const r1Color    = dark ? [94, 180, 182]  : [24, 106, 110];
  const r2Color    = dark ? [136, 174, 152] : [68, 112, 104];
  const r3Color    = dark ? [226, 165, 66]  : [168, 112, 36];
  const r4Color    = dark ? [204, 148, 72]  : [146, 102, 46];
  const r5Color    = dark ? [178, 138, 84]  : [128, 96, 56];
  const starColor  = dark ? [242, 224, 186] : [24, 106, 110];

  const skyAlpha = dark ? 42 : 34;
  const r1Alpha  = dark ? 48 : 40;
  const r2Alpha  = dark ? 56 : 46;
  const r3Alpha  = dark ? 66 : 56;
  const r4Alpha  = dark ? 56 : 48;
  const r5Alpha  = dark ? 44 : 38;

  // Precompute column curves and directional slopes.
  const y1 = new Float32Array(w);
  const y2 = new Float32Array(w);
  const y3 = new Float32Array(w);
  const y4 = new Float32Array(w);
  const y5 = new Float32Array(w);
  const s1 = new Float32Array(w);
  const s2 = new Float32Array(w);
  const s3 = new Float32Array(w);
  const s4 = new Float32Array(w);
  const s5 = new Float32Array(w);

  for (let x = 0; x < w; x++) {
    const u = x / w;
    y1[x] = ridge1(u);
    y2[x] = ridge2(u);
    y3[x] = ridge3(u);
    y4[x] = ridge4(u);
    y5[x] = ridge5(u);

    // Light from upper-left: positive slope faces away from the sun into
    // crisp slip-face shadow; negative slope catches sunlit highlights.
    s1[x] = clamp01(0.45 + slopeOf(ridge1, u) * 1.15);
    s2[x] = clamp01(0.46 + slopeOf(ridge2, u) * 1.25);
    s3[x] = clamp01(0.50 + slopeOf(ridge3, u) * 1.45);
    s4[x] = clamp01(0.48 + slopeOf(ridge4, u) * 1.30);
    s5[x] = clamp01(0.42 + slopeOf(ridge5, u) * 1.05);
  }

  // Aspect-normalized noise coordinates so cloud & ripple shapes stay natural
  // on both ultrawide monitors and narrower windows.
  const aspect = rect.width / rect.height;

  for (let y = 0; y < h; y++) {
    const v = y / h;
    const bottomFade = 1 - smoothstep(0.64, 0.99, v);
    if (bottomFade <= 0.001) continue;

    const bayerRow = (y & 7) << 3;
    const rowOff = y * w * 4;

    for (let x = 0; x < w; x++) {
      const u = x / w;
      const threshold = (BAYER8[bayerRow + (x & 7)] + 0.5) / 64;
      let density = 0;
      let color = skyColor;
      let alpha = skyAlpha;

      const c1 = y1[x];
      const c2 = y2[x];
      const c3 = y3[x];
      const c4 = y4[x];
      const c5 = y5[x];

      if (v >= c5) {
        // Layer 5: Soft foreground sand bank with fine wind ripples & grain scatter.
        const dy = v - c5;
        const crest = Math.exp(-dy * 4.8);
        const ripple = 0.08 * Math.sin(u * aspect * 22 + dy * 64 + noise2(u * 8, v * 8) * 2.2);
        const scatter = (noise2(u * aspect * 18, v * 18) - 0.5) * 0.10;
        density = (0.16 + 0.62 * s5[x] * crest + ripple * crest + scatter) * bottomFade;
        color = r5Color;
        alpha = r5Alpha;
      } else if (v >= c4) {
        // Layer 4: Counter-sweeping warm dune with sharp crest drop-off.
        const dy = v - c4;
        const crest = Math.exp(-dy * 5.2);
        const ripple = 0.075 * Math.sin(u * aspect * 28 - dy * 72 + noise2(u * 10, v * 10) * 1.8);
        // Sinuous diagonal slip-face shadow crease descending from peak near u=0.22.
        const spine = 0.24 + dy * 0.65 + 0.03 * Math.sin(dy * 18);
        const slipShadow = smoothstep(spine - 0.06, spine + 0.04, u) * 0.26 * Math.exp(-dy * 3.0);
        density = (0.18 + 0.66 * s4[x] * crest + slipShadow + ripple * 0.85) * bottomFade;
        color = r4Color;
        alpha = r4Alpha;
      } else if (v >= c3) {
        // Layer 3: Hero sculpted star dune with crisp crest line, sunlit/shadow
        // bifurcation, and wind-carved ripples.
        const dy = v - c3;
        const crestEdge = Math.exp(-dy * 18) * 0.28;
        const crestBody = Math.exp(-dy * 4.2);
        // Dramatic curving ridge spine descending from the main right peak.
        const spine = 0.72 - dy * 0.85 + 0.035 * Math.sin(dy * 20);
        const slipFace = smoothstep(spine - 0.05, spine + 0.05, u);
        const litSlope = 0.14 + 0.34 * s3[x] * crestBody;
        const darkSlope = 0.28 + 0.62 * crestBody;
        const base = litSlope + (darkSlope - litSlope) * slipFace;
        const ripple = 0.08 * Math.sin(u * aspect * 34 + dy * 84 + noise2(u * 12, v * 12) * 2.0) * (1 - slipFace * 0.5);
        density = (base + crestEdge + ripple * Math.exp(-dy * 2.2)) * bottomFade;
        color = r3Color;
        alpha = r3Alpha;
      } else if (v >= c2) {
        // Layer 2: Mid-distant teal-sand transition ridge.
        const dy = v - c2;
        const crest = Math.exp(-dy * 5.6);
        const crestEdge = Math.exp(-dy * 22) * 0.20;
        const ripple = 0.05 * Math.sin(u * aspect * 30 - dy * 60);
        density = (0.15 + 0.58 * s2[x] * crest + crestEdge + ripple * crest) * bottomFade;
        color = r2Color;
        alpha = r2Alpha;
      } else if (v >= c1) {
        // Layer 1: Distant atmospheric horizon ridge.
        const dy = v - c1;
        const crest = Math.exp(-dy * 6.4);
        density = (0.14 + 0.52 * s1[x] * crest) * bottomFade;
        color = r1Color;
        alpha = r1Alpha;
      } else {
        // Sky band extending all the way under the transparent header bar:
        // upper atmospheric gradient sculpted by wispy Bayer-dithered clouds.
        const skyH = Math.max(0.10, c1);
        const alt = 1 - clamp01(v / skyH); // 1 at top edge, 0 at distant horizon
        const cx = u * aspect * 2.1;
        const cy = v * 4.8;
        const cloud = fbm(cx + 1.4, cy + 0.6);
        // Carve crisp bright cloud puffs against a richer stippled upper sky,
        // mirroring classic 1-bit Bayer cloud halftone engravings.
        const skyGrad = 0.54 * Math.pow(alt, 1.25);
        const cloudCarve = smoothstep(0.46, 0.72, cloud) * (0.48 + 0.25 * (1 - alt));
        const cloudBelly = smoothstep(0.36, 0.50, cloud) * (1 - smoothstep(0.50, 0.64, cloud)) * 0.16;
        density = clamp01(skyGrad - cloudCarve + cloudBelly);
        color = skyColor;
        alpha = skyAlpha;
      }

      if (density > threshold) {
        const i = rowOff + (x << 2);
        data[i]     = color[0];
        data[i + 1] = color[1];
        data[i + 2] = color[2];
        data[i + 3] = alpha;
      }
    }
  }

  // Subtle 4-point desert sparkle mote in the upper-right sky (nod to the logo).
  const sx = Math.round(w * 0.84);
  const sy = Math.round(h * 0.10);
  if (sy < Math.floor(y1[sx] * h) - 6) {
    const pts = [
      [0, 0, 1.0],
      [-1, 0, 0.75], [1, 0, 0.75], [0, -1, 0.75], [0, 1, 0.75],
      [-2, 0, 0.45], [2, 0, 0.45], [0, -2, 0.45], [0, 2, 0.45],
    ];
    for (const [dx, dy, k] of pts) {
      const px = sx + dx;
      const py = sy + dy;
      if (px < 0 || px >= w || py < 0 || py >= h) continue;
      const i = (py * w + px) * 4;
      data[i]     = starColor[0];
      data[i + 1] = starColor[1];
      data[i + 2] = starColor[2];
      data[i + 3] = Math.round((dark ? 105 : 78) * k);
    }
  }

  ctx.putImageData(img, 0, 0);
}

const canvas = document.querySelector("canvas[data-dunes]");
if (canvas) {
  const root = document.documentElement;

  let resizeTimer = 0;
  const render = () => drawDunes(canvas);
  const scheduleResize = () => {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(render, 120);
  };

  // Fade the dithered backdrop smoothly as the page scrolls upward.
  let scrollRaf = 0;
  const syncScroll = () => {
    scrollRaf = 0;
    const y = window.scrollY || 0;
    const dunesFade = smoothstep(0, 300, y);
    root.style.setProperty("--dunes-fade", dunesFade.toFixed(3));
  };
  const onScroll = () => {
    if (!scrollRaf) scrollRaf = window.requestAnimationFrame(syncScroll);
  };

  render();
  syncScroll();
  window.addEventListener("scroll", onScroll, { passive: true });
  window.addEventListener("resize", scheduleResize, { passive: true });
  if (window.matchMedia) {
    window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", render);
  }
}

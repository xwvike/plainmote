// Procedural ordered Bayer 8x8 dithered desert dunes & living sky backdrop.
//
// - Each page load generates a unique wind-sculpted desert landscape from a
//   fresh seed (kept stable across resizes of the same load).
// - The heavy Bayer-dithered landscape is rendered once into an offscreen
//   pixel buffer (2px CSS cells, pixelated scaling).
// - A lightweight overlay (~30fps, automatically paused when hidden, scrolled
//   out of view, or under prefers-reduced-motion) adds subtle drifting sand
//   motes across the ridges, and in dark mode adds gently twinkling pixel
//   stars plus random dithered shooting stars (meteors) across the night sky.

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

function prefersReducedMotion() {
  return Boolean(window.matchMedia && window.matchMedia("(prefers-reduced-motion: reduce)").matches);
}

function clamp01(v) {
  return v < 0 ? 0 : v > 1 ? 1 : v;
}

function smoothstep(edge0, edge1, x) {
  const t = clamp01((x - edge0) / (edge1 - edge0));
  return t * t * (3 - 2 * t);
}

// Fast deterministic PRNG so each refresh is unique while resizes stay stable.
function mulberry32(seed) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = Math.imul(a ^ (a >>> 15), 1 | a);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

function freshSeed() {
  if (window.crypto && window.crypto.getRandomValues) {
    const buf = new Uint32Array(1);
    window.crypto.getRandomValues(buf);
    return buf[0];
  }
  return Math.floor(Math.random() * 0xffffffff) >>> 0;
}

function hash2(ix, iy, salt) {
  let n = (ix * 374761393 + iy * 668265263 + salt * 1442695041) ^ 0x5bf03635;
  n = Math.imul(n ^ (n >>> 13), 1274126177);
  return ((n ^ (n >>> 16)) >>> 0) / 4294967295;
}

function noise2(x, y, salt) {
  const ix = Math.floor(x);
  const iy = Math.floor(y);
  const fx = x - ix;
  const fy = y - iy;
  const ux = fx * fx * (3 - 2 * fx);
  const uy = fy * fy * (3 - 2 * fy);
  const a = hash2(ix, iy, salt);
  const b = hash2(ix + 1, iy, salt);
  const c = hash2(ix, iy + 1, salt);
  const d = hash2(ix + 1, iy + 1, salt);
  return a + (b - a) * ux + (c - a) * uy + (a - b - c + d) * ux * uy;
}

function fbm(x, y, salt) {
  return (
    0.55 * noise2(x, y, salt) +
    0.28 * noise2(x * 2.03 + 11.7, y * 2.01 + 5.3, salt) +
    0.17 * noise2(x * 4.07 + 23.1, y * 4.03 + 17.9, salt)
  );
}

// Build a balanced random desert scene specification for this page visit.
function createScene(seed) {
  const rng = mulberry32(seed);
  const range = (min, max) => min + rng() * (max - min);
  const tau = Math.PI * 2;

  const salt = Math.floor(rng() * 10000);
  const sunDir = rng() < 0.5 ? 1 : -1; // upper-left or upper-right light

  const r1 = {
    base: range(0.20, 0.26),
    a1: range(0.050, 0.075), f1: range(1.0, 1.35), p1: range(0, tau),
    a2: range(0.020, 0.034), f2: range(2.3, 3.1),  p2: range(0, tau),
    a3: range(0.008, 0.015), f3: range(4.5, 5.8),  p3: range(0, tau),
  };
  const r2 = {
    base: r1.base + range(0.09, 0.13),
    a1: range(0.065, 0.095), f1: range(1.05, 1.4), p1: range(0, tau),
    a2: range(0.026, 0.042), f2: range(2.2, 2.9),  p2: range(0, tau),
    a3: range(0.010, 0.017), f3: range(4.2, 5.4),  p3: range(0, tau),
  };
  const r3 = {
    base: r2.base + range(0.09, 0.13),
    a1: range(0.085, 0.125), f1: range(1.15, 1.5), p1: range(0, tau),
    a2: range(0.036, 0.056), f2: range(2.1, 2.8),  p2: range(0, tau),
    a3: range(0.014, 0.022), f3: range(3.8, 4.9),  p3: range(0, tau),
    spineX: range(0.28, 0.76),
    spineTilt: (rng() < 0.5 ? -1 : 1) * range(0.55, 0.95),
  };
  const r4 = {
    base: r3.base + range(0.11, 0.15),
    a1: range(0.070, 0.105), f1: range(1.0, 1.35), p1: range(0, tau),
    a2: range(0.028, 0.045), f2: range(2.2, 3.0),  p2: range(0, tau),
    a3: range(0.010, 0.016), f3: range(4.2, 5.3),  p3: range(0, tau),
    spineX: range(0.20, 0.78),
    spineTilt: (rng() < 0.5 ? -1 : 1) * range(0.45, 0.80),
  };
  const r5 = {
    base: Math.min(0.76, r4.base + range(0.11, 0.15)),
    a1: range(0.050, 0.075), f1: range(1.05, 1.4), p1: range(0, tau),
    a2: range(0.020, 0.032), f2: range(2.2, 2.9),  p2: range(0, tau),
    a3: 0, f3: 1, p3: 0,
  };

  const evalRidge = (r, u) =>
    r.base
    - r.a1 * Math.sin(u * Math.PI * r.f1 + r.p1)
    - r.a2 * Math.cos(u * Math.PI * r.f2 + r.p2)
    + r.a3 * Math.sin(u * Math.PI * r.f3 + r.p3);

  const cloudOffsetX = range(0, 12);
  const cloudOffsetY = range(0, 6);
  const rippleShift = range(0, tau);
  const sparkleU = range(0.72, 0.91);
  const sparkleV = range(0.05, 0.12);

  // Night sky twinkling stars (normalized coords, filtered above horizon).
  const stars = [];
  const starCount = 18 + Math.floor(rng() * 8);
  for (let i = 0; i < starCount; i++) {
    stars.push({
      u: range(0.03, 0.97),
      v: range(0.03, 0.26),
      phase: range(0, tau),
      speed: range(0.7, 1.9),
      cross: i < 3,
      bright: range(0.55, 1.0),
    });
  }

  // Gentle wind-blown sand motes skimming across the ridges.
  const motes = [];
  const moteCount = 24;
  for (let i = 0; i < moteCount; i++) {
    motes.push({
      u: rng(),
      layer: i % 3, // 0: ridge2, 1: ridge3, 2: ridge4
      lift: range(-0.025, 0.055),
      speed: range(0.008, 0.022) * (sunDir > 0 ? 1 : -1),
      bobPhase: range(0, tau),
      bobSpeed: range(0.9, 2.1),
      lifePhase: range(0, tau),
      lifeSpeed: range(0.45, 1.1),
    });
  }

  return {
    salt, sunDir, r1, r2, r3, r4, r5, evalRidge,
    cloudOffsetX, cloudOffsetY, rippleShift,
    sparkleU, sparkleV, stars, motes,
  };
}

function slopeOf(scene, r, u) {
  const eps = 0.004;
  return (scene.evalRidge(r, u + eps) - scene.evalRidge(r, u - eps)) / (2 * eps);
}

// Bake the static Bayer-dithered landscape into an offscreen pixel buffer.
function bakeBase(scene, w, h, aspect, dark) {
  const off = document.createElement("canvas");
  off.width = w;
  off.height = h;
  const ctx = off.getContext("2d", { alpha: true });
  if (!ctx) return { canvas: off, y1: new Float32Array(w), y2: new Float32Array(w), y3: new Float32Array(w), y4: new Float32Array(w) };

  const img = ctx.createImageData(w, h);
  const data = img.data;

  // Brand-rooted desert palette kept calm behind foreground text & cards.
  const skyColor = dark ? [94, 180, 182]  : [24, 106, 110];
  const r1Color  = dark ? [94, 180, 182]  : [24, 106, 110];
  const r2Color  = dark ? [136, 174, 152] : [68, 112, 104];
  const r3Color  = dark ? [226, 165, 66]  : [168, 112, 36];
  const r4Color  = dark ? [204, 148, 72]  : [146, 102, 46];
  const r5Color  = dark ? [178, 138, 84]  : [128, 96, 56];

  const skyAlpha = dark ? 42 : 34;
  const r1Alpha  = dark ? 48 : 40;
  const r2Alpha  = dark ? 56 : 46;
  const r3Alpha  = dark ? 66 : 56;
  const r4Alpha  = dark ? 56 : 48;
  const r5Alpha  = dark ? 44 : 38;

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

  const dir = scene.sunDir;
  for (let x = 0; x < w; x++) {
    const u = x / w;
    y1[x] = scene.evalRidge(scene.r1, u);
    y2[x] = scene.evalRidge(scene.r2, u);
    y3[x] = scene.evalRidge(scene.r3, u);
    y4[x] = scene.evalRidge(scene.r4, u);
    y5[x] = scene.evalRidge(scene.r5, u);

    s1[x] = clamp01(0.45 + dir * slopeOf(scene, scene.r1, u) * 1.15);
    s2[x] = clamp01(0.46 + dir * slopeOf(scene, scene.r2, u) * 1.25);
    s3[x] = clamp01(0.50 + dir * slopeOf(scene, scene.r3, u) * 1.45);
    s4[x] = clamp01(0.48 + dir * slopeOf(scene, scene.r4, u) * 1.30);
    s5[x] = clamp01(0.42 + dir * slopeOf(scene, scene.r5, u) * 1.05);
  }

  const salt = scene.salt;
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
        const dy = v - c5;
        const crest = Math.exp(-dy * 4.8);
        const ripple = 0.08 * Math.sin(u * aspect * 22 + dy * 64 + scene.rippleShift + noise2(u * 8, v * 8, salt) * 2.2);
        const scatter = (noise2(u * aspect * 18, v * 18, salt) - 0.5) * 0.10;
        density = (0.16 + 0.62 * s5[x] * crest + ripple * crest + scatter) * bottomFade;
        color = r5Color;
        alpha = r5Alpha;
      } else if (v >= c4) {
        const dy = v - c4;
        const crest = Math.exp(-dy * 5.2);
        const ripple = 0.075 * Math.sin(u * aspect * 28 - dy * 72 + scene.rippleShift + noise2(u * 10, v * 10, salt) * 1.8);
        const spine = scene.r4.spineX + dy * scene.r4.spineTilt + 0.03 * Math.sin(dy * 18);
        const slipShadow = smoothstep(spine - 0.06, spine + 0.04, u) * 0.26 * Math.exp(-dy * 3.0);
        density = (0.18 + 0.66 * s4[x] * crest + slipShadow + ripple * 0.85) * bottomFade;
        color = r4Color;
        alpha = r4Alpha;
      } else if (v >= c3) {
        const dy = v - c3;
        const crestEdge = Math.exp(-dy * 18) * 0.28;
        const crestBody = Math.exp(-dy * 4.2);
        const spine = scene.r3.spineX + dy * scene.r3.spineTilt + 0.035 * Math.sin(dy * 20);
        const slipFace = smoothstep(spine - 0.05, spine + 0.05, u);
        const litSlope = 0.14 + 0.34 * s3[x] * crestBody;
        const darkSlope = 0.28 + 0.62 * crestBody;
        const base = litSlope + (darkSlope - litSlope) * slipFace;
        const ripple = 0.08 * Math.sin(u * aspect * 34 + dy * 84 + scene.rippleShift + noise2(u * 12, v * 12, salt) * 2.0) * (1 - slipFace * 0.5);
        density = (base + crestEdge + ripple * Math.exp(-dy * 2.2)) * bottomFade;
        color = r3Color;
        alpha = r3Alpha;
      } else if (v >= c2) {
        const dy = v - c2;
        const crest = Math.exp(-dy * 5.6);
        const crestEdge = Math.exp(-dy * 22) * 0.20;
        const ripple = 0.05 * Math.sin(u * aspect * 30 - dy * 60 + scene.rippleShift);
        density = (0.15 + 0.58 * s2[x] * crest + crestEdge + ripple * crest) * bottomFade;
        color = r2Color;
        alpha = r2Alpha;
      } else if (v >= c1) {
        const dy = v - c1;
        const crest = Math.exp(-dy * 6.4);
        density = (0.14 + 0.52 * s1[x] * crest) * bottomFade;
        color = r1Color;
        alpha = r1Alpha;
      } else {
        const skyH = Math.max(0.10, c1);
        const alt = 1 - clamp01(v / skyH);
        const cx = u * aspect * 2.1 + scene.cloudOffsetX;
        const cy = v * 4.8 + scene.cloudOffsetY;
        const cloud = fbm(cx, cy, salt);
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

  // Quiet 4-point sparkle mote in light mode (dark mode uses live twinkling stars).
  if (!dark) {
    const sx = Math.round(w * scene.sparkleU);
    const sy = Math.round(h * scene.sparkleV);
    if (sx >= 2 && sx < w - 2 && sy >= 2 && sy < Math.floor(y1[sx] * h) - 6) {
      const pts = [
        [0, 0, 1.0],
        [-1, 0, 0.7], [1, 0, 0.7], [0, -1, 0.7], [0, 1, 0.7],
        [-2, 0, 0.4], [2, 0, 0.4], [0, -2, 0.4], [0, 2, 0.4],
      ];
      for (const [dx, dy, k] of pts) {
        const i = ((sy + dy) * w + (sx + dx)) * 4;
        data[i]     = 24;
        data[i + 1] = 106;
        data[i + 2] = 110;
        data[i + 3] = Math.round(74 * k);
      }
    }
  }

  ctx.putImageData(img, 0, 0);
  return { canvas: off, y1, y2, y3, y4 };
}

const canvas = document.querySelector("canvas[data-dunes]");
if (canvas) {
  const root = document.documentElement;
  const ctx = canvas.getContext("2d", { alpha: true });
  const scene = createScene(freshSeed());

  let width = 0;
  let height = 0;
  let dark = isDarkTheme();
  let baked = null;
  let dunesFade = 0;
  let meteor = null;
  let nextMeteorAt = 0;

  function spawnMeteor(now) {
    const goRight = Math.random() < 0.62;
    const startU = goRight ? (0.08 + Math.random() * 0.56) : (0.36 + Math.random() * 0.56);
    const startV = 0.02 + Math.random() * 0.09;
    const angle = (goRight ? 1 : -1) * (0.28 + Math.random() * 0.24);
    const span = (0.14 + Math.random() * 0.16) * (goRight ? 1 : -1);
    const vx = span;
    const vy = Math.abs(span) * Math.abs(Math.tan(angle)) * 0.85;
    meteor = {
      startAt: now,
      duration: 620 + Math.random() * 420,
      x0: startU * width,
      y0: startV * height,
      dx: vx * width,
      dy: vy * height,
      tail: 18 + Math.floor(Math.random() * 14),
    };
  }

  function scheduleNextMeteor(now, initial = false) {
    // Give dark-mode visitors an early welcome meteor shortly after load,
    // then space subsequent ones organically every 5–13 seconds.
    const delay = initial ? (1400 + Math.random() * 1600) : (5200 + Math.random() * 8000);
    nextMeteorAt = now + delay;
  }

  function rebuild() {
    const rect = canvas.getBoundingClientRect();
    if (rect.width < 1 || rect.height < 1) return;
    width = Math.max(1, Math.ceil(rect.width / CELL));
    height = Math.max(1, Math.ceil(rect.height / CELL));
    if (canvas.width !== width || canvas.height !== height) {
      canvas.width = width;
      canvas.height = height;
    }
    dark = isDarkTheme();
    baked = bakeBase(scene, width, height, rect.width / rect.height, dark);
    meteor = null;
    if (dark) scheduleNextMeteor(performance.now(), true);
    paintFrame(performance.now(), true);
  }

  function paintFrame(now, staticOnly = false) {
    if (!ctx || !baked) return;
    ctx.clearRect(0, 0, width, height);
    ctx.drawImage(baked.canvas, 0, 0);

    const t = now * 0.001;

    // 1. Dark mode: twinkling pixel stars in the clear sky above the far ridge.
    if (dark) {
      for (const s of scene.stars) {
        const sx = Math.round(s.u * width);
        const sy = Math.round(s.v * height);
        if (sx < 2 || sx >= width - 2 || sy < 2) continue;
        const horizon = Math.floor(baked.y1[sx] * height) - 4;
        if (sy >= horizon) continue;
        const pulse = staticOnly ? 0.8 : (0.35 + 0.65 * (0.5 + 0.5 * Math.sin(t * s.speed + s.phase)));
        const a = clamp01(s.bright * pulse * 0.62);
        ctx.fillStyle = `rgba(242, 224, 186, ${a.toFixed(3)})`;
        ctx.fillRect(sx, sy, 1, 1);
        if (s.cross && pulse > 0.55) {
          const arm = (a * 0.52).toFixed(3);
          ctx.fillStyle = `rgba(176, 214, 212, ${arm})`;
          ctx.fillRect(sx - 1, sy, 1, 1);
          ctx.fillRect(sx + 1, sy, 1, 1);
          ctx.fillRect(sx, sy - 1, 1, 1);
          ctx.fillRect(sx, sy + 1, 1, 1);
        }
      }
    }

    if (staticOnly) return;

    // 2. Subtle wind-drifting sand motes along the dune crests (light & dark).
    for (const m of scene.motes) {
      const u = ((m.u + t * m.speed) % 1 + 1) % 1;
      const mx = Math.floor(u * width);
      if (mx < 0 || mx >= width) continue;
      const ridgeArr = m.layer === 0 ? baked.y2 : m.layer === 1 ? baked.y3 : baked.y4;
      const bob = 0.008 * Math.sin(t * m.bobSpeed + m.bobPhase);
      const mv = ridgeArr[mx] + m.lift + bob;
      const my = Math.floor(mv * height);
      if (my < 0 || my >= height) continue;
      const fadeY = 1 - smoothstep(0.62, 0.96, mv);
      const life = Math.pow(0.5 + 0.5 * Math.sin(t * m.lifeSpeed + m.lifePhase), 1.6);
      const a = life * fadeY * (dark ? 0.34 : 0.24);
      if (a < 0.03) continue;
      ctx.fillStyle = dark
        ? `rgba(232, 186, 108, ${a.toFixed(3)})`
        : `rgba(156, 106, 40, ${a.toFixed(3)})`;
      ctx.fillRect(mx, my, 1, 1);
    }

    // 3. Dark mode: random dithered shooting stars (meteors) across the sky.
    if (dark) {
      if (!meteor && now >= nextMeteorAt) {
        spawnMeteor(now);
      }
      if (meteor) {
        const raw = (now - meteor.startAt) / meteor.duration;
        if (raw >= 1) {
          meteor = null;
          scheduleNextMeteor(now, false);
        } else {
          // Ease-out glide + clean fade-in / fade-out envelope.
          const progress = 1 - Math.pow(1 - raw, 2.1);
          const env = smoothstep(0, 0.14, raw) * (1 - smoothstep(0.58, 1.0, raw));
          const hx = meteor.x0 + meteor.dx * progress;
          const hy = meteor.y0 + meteor.dy * progress;
          const len = Math.hypot(meteor.dx, meteor.dy) || 1;
          const ux = meteor.dx / len;
          const uy = meteor.dy / len;

          for (let step = 0; step < meteor.tail; step++) {
            const px = Math.round(hx - ux * step);
            const py = Math.round(hy - uy * step);
            if (px < 0 || px >= width || py < 0 || py >= height) continue;
            // Disappear naturally behind the distant horizon ridge.
            if (py >= Math.floor(baked.y1[px] * height) - 1) continue;

            const k = 1 - step / meteor.tail;
            // Ordered Bayer stippling along the fading meteor tail so it blends
            // seamlessly with the bitmap dithered aesthetic.
            const bayer = (BAYER8[((py & 7) << 3) + (px & 7)] + 0.5) / 64;
            if (step > 2 && k * 1.15 < bayer) continue;

            const a = clamp01(env * Math.pow(k, 1.25) * (step === 0 ? 0.95 : 0.75));
            if (a < 0.04) continue;
            if (step <= 1) {
              ctx.fillStyle = `rgba(250, 240, 214, ${a.toFixed(3)})`;
            } else if (step < 7) {
              ctx.fillStyle = `rgba(232, 186, 108, ${a.toFixed(3)})`;
            } else {
              ctx.fillStyle = `rgba(116, 196, 198, ${(a * 0.85).toFixed(3)})`;
            }
            ctx.fillRect(px, py, 1, 1);
          }
        }
      }
    }
  }

  let animRaf = 0;
  let lastPaint = 0;
  function tick(now) {
    animRaf = 0;
    if (document.hidden || dunesFade >= 0.98 || prefersReducedMotion()) return;
    if (now - lastPaint >= 32) {
      lastPaint = now;
      paintFrame(now, false);
    }
    animRaf = window.requestAnimationFrame(tick);
  }

  function ensureLoop() {
    if (animRaf || document.hidden || dunesFade >= 0.98 || prefersReducedMotion()) return;
    animRaf = window.requestAnimationFrame(tick);
  }

  let scrollRaf = 0;
  function syncScroll() {
    scrollRaf = 0;
    const y = window.scrollY || 0;
    dunesFade = smoothstep(0, 300, y);
    root.style.setProperty("--dunes-fade", dunesFade.toFixed(3));
    if (dunesFade < 0.98) ensureLoop();
  }

  let resizeTimer = 0;
  const scheduleResize = () => {
    clearTimeout(resizeTimer);
    resizeTimer = setTimeout(() => {
      rebuild();
      ensureLoop();
    }, 120);
  };

  rebuild();
  syncScroll();
  ensureLoop();

  window.addEventListener("scroll", () => {
    if (!scrollRaf) scrollRaf = window.requestAnimationFrame(syncScroll);
  }, { passive: true });
  window.addEventListener("resize", scheduleResize, { passive: true });
  document.addEventListener("visibilitychange", () => {
    if (!document.hidden) ensureLoop();
  });
  if (window.matchMedia) {
    window.matchMedia("(prefers-color-scheme: dark)").addEventListener("change", () => {
      rebuild();
      ensureLoop();
    });
    window.matchMedia("(prefers-reduced-motion: reduce)").addEventListener("change", () => {
      if (prefersReducedMotion()) paintFrame(performance.now(), true);
      else ensureLoop();
    });
  }
}

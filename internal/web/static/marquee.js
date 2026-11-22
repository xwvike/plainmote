// Generic hover-scroll marquee for single-line truncated elements.
// Add `data-marquee` to any single-line container that clips with ellipsis:
//   - Idle: DOM stays untouched so native `text-overflow: ellipsis` and
//     dynamic text updaters (time.js, sealed-names.js) work uninterrupted.
//   - Mouse hover: after a brief dwell, if content overflows, glides slowly
//     to reveal the hidden tail with soft clipped-edge fades.
//   - Mouse leave: swiftly eases back to the start and restores ellipsis.
//   - Respects prefers-reduced-motion.

const DWELL_MS = 160;
const SCROLL_SPEED = 42; // px per second while hovered
const MIN_FORWARD_S = 0.65;
const RETURN_SPEED = 540; // px per second when returning to start
const MIN_RETURN_S = 0.16;
const MAX_RETURN_S = 0.34;
const END_PAD = 10; // px breathing room after the last character
const MIN_OVERFLOW = 2;

const states = new WeakMap();

function reducedMotion() {
  return Boolean(globalThis.matchMedia?.("(prefers-reduced-motion: reduce)")?.matches);
}

function getState(el) {
  let state = states.get(el);
  if (!state) {
    state = { dwell: 0, fallback: 0, track: null, onEnd: null, phase: "idle" };
    states.set(el, state);
  }
  return state;
}

function clearTimers(state) {
  if (state.dwell) {
    clearTimeout(state.dwell);
    state.dwell = 0;
  }
  if (state.fallback) {
    clearTimeout(state.fallback);
    state.fallback = 0;
  }
  if (state.track && state.onEnd) {
    state.track.removeEventListener("transitionend", state.onEnd);
    state.onEnd = null;
  }
}

function currentOffset(track) {
  const transform = getComputedStyle(track).transform;
  if (!transform || transform === "none") return 0;
  try {
    return new DOMMatrix(transform).m41;
  } catch {
    return 0;
  }
}

function wrap(el) {
  const track = document.createElement("span");
  track.className = "mq-track";
  if (getComputedStyle(el).display.includes("flex")) {
    track.classList.add("mq-track-flex");
  }
  while (el.firstChild) track.appendChild(el.firstChild);
  el.appendChild(track);
  return track;
}

function unwrap(el, state) {
  clearTimers(state);
  const track = state.track;
  state.track = null;
  state.phase = "idle";
  if (track && track.parentNode === el) {
    while (track.firstChild) el.insertBefore(track.firstChild, track);
    track.remove();
  }
  el.classList.remove("mq-active", "mq-fade-l", "mq-fade-r");
}

function scrollForward(el, state, fromOffset) {
  const track = state.track;
  if (!track || track.parentNode !== el) return;
  const overflow = Math.ceil(track.scrollWidth - el.clientWidth);
  if (overflow <= MIN_OVERFLOW) {
    unwrap(el, state);
    return;
  }
  const distance = overflow + END_PAD;
  const remaining = Math.max(0, distance + fromOffset);
  const duration = Math.max(MIN_FORWARD_S, remaining / SCROLL_SPEED);

  el.classList.add("mq-active", "mq-fade-l", "mq-fade-r");
  track.style.transition = "none";
  track.style.transform = `translateX(${fromOffset}px)`;
  void track.offsetWidth;

  state.phase = "forward";
  const finish = () => {
    clearTimers(state);
    if (state.phase === "forward") {
      state.phase = "held";
      el.classList.remove("mq-fade-r");
    }
  };
  state.onEnd = (event) => {
    if (event.target === track && event.propertyName === "transform") finish();
  };
  track.addEventListener("transitionend", state.onEnd);
  state.fallback = setTimeout(finish, duration * 1000 + 80);

  track.style.transition = `transform ${duration.toFixed(3)}s linear`;
  track.style.transform = `translateX(${-distance}px)`;
}

function enter(el) {
  if (reducedMotion()) return;
  const state = getState(el);
  clearTimers(state);

  // Re-entering mid-return: smoothly resume forward from current pixel.
  if (state.track && state.track.parentNode === el) {
    const offset = Math.min(0, currentOffset(state.track));
    scrollForward(el, state, offset);
    return;
  }

  // Fast exit when content already fits without truncation.
  if (el.scrollWidth - el.clientWidth <= MIN_OVERFLOW) return;

  state.phase = "dwell";
  state.dwell = setTimeout(() => {
    state.dwell = 0;
    if (!el.isConnected || el.scrollWidth - el.clientWidth <= MIN_OVERFLOW) {
      state.phase = "idle";
      return;
    }
    state.track = wrap(el);
    scrollForward(el, state, 0);
  }, DWELL_MS);
}

function leave(el) {
  const state = states.get(el);
  if (!state) return;
  clearTimers(state);

  const track = state.track;
  if (!track || track.parentNode !== el) {
    state.track = null;
    state.phase = "idle";
    el.classList.remove("mq-active", "mq-fade-l", "mq-fade-r");
    return;
  }

  const offset = Math.min(0, currentOffset(track));
  if (Math.abs(offset) < 1) {
    unwrap(el, state);
    return;
  }

  const duration = Math.min(MAX_RETURN_S, Math.max(MIN_RETURN_S, Math.abs(offset) / RETURN_SPEED));
  track.style.transition = "none";
  track.style.transform = `translateX(${offset}px)`;
  void track.offsetWidth;

  el.classList.add("mq-active", "mq-fade-r");
  el.classList.remove("mq-fade-l");
  state.phase = "returning";

  const done = () => unwrap(el, state);
  state.onEnd = (event) => {
    if (event.target === track && event.propertyName === "transform") done();
  };
  track.addEventListener("transitionend", state.onEnd);
  state.fallback = setTimeout(done, duration * 1000 + 80);

  track.style.transition = `transform ${duration.toFixed(3)}s cubic-bezier(0.22, 1, 0.36, 1)`;
  track.style.transform = "translateX(0)";
}

function targetMarquee(node) {
  return node instanceof Element ? node.closest("[data-marquee]") : null;
}

document.addEventListener("pointerover", (event) => {
  if (event.pointerType !== "mouse") return;
  const el = targetMarquee(event.target);
  if (!el) return;
  if (event.relatedTarget instanceof Node && el.contains(event.relatedTarget)) return;
  enter(el);
});

document.addEventListener("pointerout", (event) => {
  if (event.pointerType !== "mouse") return;
  const el = targetMarquee(event.target);
  if (!el) return;
  if (event.relatedTarget instanceof Node && el.contains(event.relatedTarget)) return;
  leave(el);
});

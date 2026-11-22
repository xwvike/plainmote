// Progressive enhancement for modern, zero-click-friction list controls.
// Without this module every form keeps its submit button and works identically.
import "./marquee.js";

document.documentElement.classList.add("js-ready");

document.addEventListener("change", (event) => {
  const target = event.target;
  if (!(target instanceof HTMLElement) || !target.matches("[data-auto-submit]")) return;
  const form = target.form || target.closest("form");
  if (!form || event.defaultPrevented) return;
  if (typeof form.requestSubmit === "function") form.requestSubmit();
  else form.submit();
});

// Clearing a search input via its native clear button submits immediately.
document.addEventListener("search", (event) => {
  const target = event.target;
  if (!(target instanceof HTMLInputElement) || !target.matches("[data-auto-search]")) return;
  const form = target.form || target.closest("form");
  if (!form) return;
  if (typeof form.requestSubmit === "function") form.requestSubmit();
  else form.submit();
});

// Mobile app-style navigation drawer: open/close, Esc, desktop-resize cleanup,
// and horizontal swipe-left dismissal on touch devices.
(() => {
  const root = document.documentElement;
  const drawer = document.getElementById("app-drawer");
  const opener = document.querySelector("[data-drawer-open]");
  if (!drawer || !opener) return;

  const setOpen = (open, restoreFocus = false) => {
    const isOpen = root.classList.contains("drawer-open");
    if (open === isOpen) return;
    root.classList.toggle("drawer-open", open);
    opener.setAttribute("aria-expanded", open ? "true" : "false");
    drawer.setAttribute("aria-hidden", open ? "false" : "true");
    if (open) {
      const closeBtn = drawer.querySelector("[data-drawer-close]");
      if (closeBtn instanceof HTMLElement) closeBtn.focus({ preventScroll: true });
    } else if (restoreFocus && opener instanceof HTMLElement) {
      opener.focus({ preventScroll: true });
    }
  };

  document.addEventListener("click", (event) => {
    const target = event.target;
    if (!(target instanceof Element)) return;
    if (target.closest("[data-drawer-open]")) {
      setOpen(!root.classList.contains("drawer-open"));
      return;
    }
    if (target.closest("[data-drawer-close]")) {
      setOpen(false, true);
    }
  });

  document.addEventListener("keydown", (event) => {
    if (event.key === "Escape" && root.classList.contains("drawer-open")) {
      setOpen(false, true);
    }
  });

  const desktop = window.matchMedia("(min-width: 761px)");
  const onMedia = (e) => { if (e.matches) setOpen(false); };
  if (typeof desktop.addEventListener === "function") desktop.addEventListener("change", onMedia);

  // Swipe left on the drawer surface to dismiss naturally.
  let startX = 0;
  let startY = 0;
  let dragging = false;
  let locked = false;
  let lastDx = 0;

  drawer.addEventListener("touchstart", (event) => {
    if (!root.classList.contains("drawer-open") || event.touches.length !== 1) return;
    startX = event.touches[0].clientX;
    startY = event.touches[0].clientY;
    dragging = false;
    locked = false;
    lastDx = 0;
  }, { passive: true });

  drawer.addEventListener("touchmove", (event) => {
    if (!root.classList.contains("drawer-open") || locked || event.touches.length !== 1) return;
    const dx = event.touches[0].clientX - startX;
    const dy = event.touches[0].clientY - startY;
    if (!dragging) {
      if (Math.abs(dy) > Math.abs(dx) && Math.abs(dy) > 8) { locked = true; return; }
      if (dx < -8) {
        dragging = true;
        drawer.classList.add("dragging");
      } else {
        return;
      }
    }
    lastDx = Math.min(0, dx);
    drawer.style.transform = `translateX(${lastDx}px)`;
  }, { passive: true });

  const endSwipe = () => {
    if (!dragging) return;
    dragging = false;
    drawer.classList.remove("dragging");
    drawer.style.transform = "";
    if (lastDx < -64) setOpen(false);
    lastDx = 0;
  };
  drawer.addEventListener("touchend", endSwipe, { passive: true });
  drawer.addEventListener("touchcancel", endSwipe, { passive: true });
})();

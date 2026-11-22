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

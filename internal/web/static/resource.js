// The custom lifetime box is live only while the custom stop is chosen, and
// choosing that stop puts the cursor in it.
function syncLifetime(box, focus) {
  const custom = box.form ? box.form.querySelector('input[name="ttl"][value="custom"]') : null;
  box.disabled = !custom || !custom.checked;
  if (focus && !box.disabled) box.focus();
}

function followLifetime(event) {
  const radio = event.target;
  if (!(radio instanceof HTMLInputElement) || radio.name !== "ttl") return;
  const box = radio.form ? radio.form.querySelector("[data-ttl-custom]") : null;
  if (box) syncLifetime(box, true);
}

function syncWithin(root) {
  for (const box of root.querySelectorAll("[data-ttl-custom]")) syncLifetime(box, false);
}

// Command-S or Ctrl-S saves, from the editor or any field of the form, the
// way pressing the save button does. The browser's own "save page as" is no
// use here.
function saveShortcut(event) {
  if (event.key !== "s" || event.shiftKey || event.altKey || !(event.metaKey || event.ctrlKey)) return;
  const form = document.querySelector("form.rform");
  const button = form ? form.querySelector("button[type='submit'][data-part='submit']") : null;
  if (!button || document.querySelector("dialog[open]")) return;
  event.preventDefault();
  if (!button.disabled) form.requestSubmit(button);
}

if (typeof document !== "undefined" && document.addEventListener) {
  document.addEventListener("change", followLifetime);
  document.addEventListener("keydown", saveShortcut);
  // The share dialog arrives after the page has loaded, through parts.js.
  document.addEventListener("plainmote:parts", (event) => syncWithin(event.detail.root));
  syncWithin(document);
}

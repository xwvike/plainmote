// Creating a share changes only the share list. Keep the editor, selection and
// scroll position intact instead of rebuilding the page.
export function formActionURL(form, baseURL) {
  // The form also has an input named "action". Named form controls can
  // shadow form.action in browsers, so read the URL from the attribute.
  return new URL(form.getAttribute("action"), baseURL);
}

async function createShare(event) {
  const form = event.target.closest("form[data-share-create]");
  if (!form) return;
  event.preventDefault();

  const button = form.querySelector("button[type='submit']");
  const panel = form.closest("[data-share-panel]");
  if (button) button.disabled = true;
  if (panel) panel.setAttribute("aria-busy", "true");

  try {
    const target = formActionURL(form, window.location.href);
    const response = await fetch(target, {
      method: "POST",
      body: new FormData(form),
      credentials: "same-origin",
      headers: { "X-PlainMote-Fragment": "shares" },
    });
    if (response.redirected) {
      window.location.assign(response.url);
      return;
    }
    if (!response.ok) throw new Error(`share request failed: ${response.status}`);

    const parsed = new DOMParser().parseFromString(await response.text(), "text/html");
    const nextPanel = parsed.querySelector("[data-share-panel]");
    const currentPanel = document.querySelector("[data-share-panel]");
    if (!nextPanel || !currentPanel) throw new Error("share fragment is incomplete");
    currentPanel.replaceWith(nextPanel);
  } catch (error) {
    // The POST may already have succeeded, so never repeat it automatically.
    // A GET safely reconciles the page with whichever result reached the server.
    console.error("resource: cannot refresh share panel", error);
    window.location.reload();
  }
}

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

if (typeof document !== "undefined" && document.addEventListener) {
  document.addEventListener("submit", createShare);
  document.addEventListener("change", followLifetime);
  for (const box of document.querySelectorAll("[data-ttl-custom]")) syncLifetime(box, false);
}

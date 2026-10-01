// Dialogs open over the page instead of reloading it, and forms marked
// data-parts are sent without leaving it, so an edit in progress survives
// both. The server answers either with the parts of the page that changed -
// elements carrying data-part, rendered from the same templates as the page -
// and every link and form still works as a plain request without this script.
//
//   a[data-dialog]     fetches its href and opens the dialog in the answer
//   form[data-parts]   posts, then puts each part of the answer in place
//   [data-close]       closes the dialog it is in
//
// A part named "dialog" is the dialog to show; an answer without one closes
// the dialog that is open. Any other part replaces the element of the same
// name on the page, and a <title> in the answer becomes the page's title.
//
// A posted form that the server redirected - an action that went through -
// moves the address to where the redirect pointed, as the plain request
// would have, and the form hears "plainmote:submitted" with the outcome.

const partsHeader = "X-PlainMote-Fragment";

export function formActionURL(form, baseURL) {
  // The form also has an input named "action". Named form controls can
  // shadow form.action in browsers, so read the URL from the attribute.
  return new URL(form.getAttribute("action"), baseURL);
}

// sameOrigin keeps the script to this site's own addresses: anything else is
// left to the browser, which follows it as an ordinary link.
export function sameOrigin(url, baseURL) {
  return new URL(url, baseURL).origin === new URL(baseURL).origin;
}

let busy = false;

async function fetchParts(url, init) {
  const response = await fetch(url, {
    ...init,
    credentials: "same-origin",
    headers: { [partsHeader]: "1" },
  });
  // A redirect to another page - the sign-in page once a session has ended,
  // or wherever a finished action leads - is followed for real.
  if (response.redirected && new URL(response.url).pathname !== window.location.pathname) {
    window.location.assign(response.url);
    return null;
  }
  const type = response.headers.get("Content-Type") || "";
  if (!type.startsWith("text/html")) throw new Error(`unexpected answer: ${response.status}`);
  const parsed = new DOMParser().parseFromString(await response.text(), "text/html");
  const parts = [...parsed.body.querySelectorAll("[data-part]")];
  if (parts.length === 0) throw new Error(`answer has no parts: ${response.status}`);
  const title = parsed.querySelector("title");
  return {
    parts,
    title: title ? title.textContent : null,
    redirected: response.redirected,
    url: response.url,
  };
}

// A part inside a form belongs to that form and changes only with an answer
// to it. The version an edit starts from is one: refreshed by a share action,
// it would let the next save go over a change made elsewhere unnoticed.
function apply({ parts, title }, opener, closeTo, submitted = null) {
  let dialog = null;
  for (const part of parts) {
    const node = document.adoptNode(part);
    if (node.dataset.part === "dialog") {
      dialog = node;
      continue;
    }
    const current = document.querySelector(`[data-part="${node.dataset.part}"]`);
    const owner = current ? current.closest("form") : null;
    if (current && (!owner || owner === submitted)) {
      current.replaceWith(node);
      announce(node);
    }
  }
  if (title !== null) document.title = title;
  const open = document.querySelector("dialog[data-part='dialog'][open]");
  if (dialog) {
    show(dialog, open, opener, closeTo);
  } else if (open) {
    open.close();
  }
}

// Scripts that set up elements when the page loads (resource.js, for one)
// hear about elements that arrive later.
function announce(root) {
  document.dispatchEvent(new CustomEvent("plainmote:parts", { detail: { root } }));
}

function show(dialog, current, opener, closeTo) {
  dialog.removeAttribute("open");
  if (current) {
    // A refused submission comes back as the same dialog with a message in
    // it. Replace it in place and keep where focus is to return to.
    dialog.opener = current.opener;
    current.removeEventListener("close", current.cleanup);
    current.replaceWith(dialog);
  } else {
    dialog.opener = opener;
    document.body.append(dialog);
  }
  wire(dialog, closeTo);
  dialog.showModal();
  announce(dialog);
}

function wire(dialog, closeTo) {
  dialog.cleanup = () => {
    dialog.remove();
    if (closeTo) history.replaceState(history.state, "", closeTo);
    if (dialog.opener && dialog.opener.isConnected) dialog.opener.focus();
  };
  dialog.addEventListener("close", dialog.cleanup, { once: true });
  // A click outside the box closes it, unless something in it was changed:
  // a stray click should not throw away what was typed.
  dialog.addEventListener("input", () => { dialog.dataset.dirty = "1"; });
  dialog.addEventListener("click", (event) => {
    if (event.target === dialog && !dialog.dataset.dirty) dialog.close();
  });
}

async function openDialog(event) {
  const link = event.target.closest("a[data-dialog]");
  if (!link || event.defaultPrevented || event.button !== 0) return;
  if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
  if (!sameOrigin(link.href, window.location.href)) return;
  event.preventDefault();
  if (busy) return;
  busy = true;
  link.setAttribute("aria-busy", "true");
  try {
    const answer = await fetchParts(link.href, { method: "GET" });
    if (answer) apply(answer, link);
  } catch (error) {
    // Nothing was changed by asking, so the plain link is a safe fallback.
    console.error("parts: cannot open dialog", error);
    window.location.assign(link.href);
  } finally {
    busy = false;
    link.removeAttribute("aria-busy");
  }
}

// A chosen file can change what kind of page comes back - a picture where
// there was text - so a form carrying one is sent the plain way.
function carriesFile(form) {
  return [...form.querySelectorAll("input[type='file']")].some((input) => input.files && input.files.length > 0);
}

// Said where the page puts its messages, when the answer to a posted form
// never arrives. Reloading instead would be safe for the server but would
// throw away an edit the save was meant to keep.
function unconfirmed() {
  const message = document.body.dataset.msgRequestFailed;
  const slot = document.querySelector("[data-part='notices'], [data-part='flash']");
  if (!message || !slot) return false;
  const line = document.createElement("p");
  line.className = "err";
  line.setAttribute("role", "alert");
  line.textContent = message;
  slot.replaceChildren(line);
  return true;
}

async function submit(event) {
  const form = event.target.closest("form[data-parts]");
  if (!form || event.defaultPrevented || carriesFile(form)) return;
  const target = formActionURL(form, window.location.href);
  if (!sameOrigin(target, window.location.href)) return;
  event.preventDefault();
  if (busy) return;
  busy = true;
  // Read before the buttons are disabled: a disabled submitter is left out.
  const body = new FormData(form, event.submitter || undefined);
  const buttons = [...form.querySelectorAll("button[type='submit'], button:not([type])")];
  for (const button of buttons) button.disabled = true;
  form.setAttribute("aria-busy", "true");
  try {
    const answer = await fetchParts(target, { method: "POST", body });
    if (answer) {
      let closeTo = null;
      if (answer.redirected) {
        history.replaceState(history.state, "", answer.url);
        // A dialog the redirect reopened - a refusal - was opened by the
        // address, and closing it takes the address back to the page.
        closeTo = window.location.pathname;
      }
      apply(answer, null, closeTo, form);
      form.dispatchEvent(new CustomEvent("plainmote:submitted", { detail: { saved: answer.redirected } }));
    }
  } catch (error) {
    // The POST may already have taken effect, so it is never repeated.
    console.error("parts: cannot apply the answer", error);
    if (!unconfirmed()) window.location.reload();
  } finally {
    busy = false;
    for (const button of buttons) button.disabled = false;
    form.removeAttribute("aria-busy");
  }
}

function close(event) {
  const control = event.target.closest("[data-close]");
  if (!control || event.button !== 0) return;
  const dialog = control.closest("dialog[open]");
  if (!dialog || !dialog.cleanup) return;
  event.preventDefault();
  dialog.close();
}

// A dialog the server rendered open - an address with ?delete=1 typed in, or
// a refusal on a page loaded the plain way - becomes the same modal dialog.
// Closing it takes the address back to the page without the dialog.
function adoptRendered() {
  const dialog = document.querySelector("dialog[data-part='dialog'][open]");
  if (!dialog) return;
  for (const scrim of document.querySelectorAll(".scrim")) scrim.remove();
  const closer = dialog.querySelector("[data-close]");
  dialog.removeAttribute("open");
  wire(dialog, closer ? closer.getAttribute("href") : null);
  dialog.showModal();
}

if (typeof document !== "undefined" && document.addEventListener) {
  document.addEventListener("click", close);
  document.addEventListener("click", openDialog);
  document.addEventListener("submit", submit);
  adoptRendered();
}

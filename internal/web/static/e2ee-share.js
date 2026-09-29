// The home page for an account with end-to-end encryption on. The box's text,
// or the file chosen in its place, is encrypted here and only the ciphertext is posted; the key goes into the
// address of the result page after #, which is never sent to the server, and
// the result page puts it on the end of the share link.
import { seal } from "./e2ee.js";

function sizeText(bytes) {
  if (bytes >= 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(2)} MiB`;
  if (bytes >= 1024) return `${(bytes / 1024).toFixed(2)} KiB`;
  return `${bytes} B`;
}

// The code a recipient types: four characters from letters and digits that
// cannot be mistaken for one another - no 0 or O, no 1, I or L - read in
// either case. Drawn without bias from the browser's own randomness.
const PIN_ALPHABET = "ABCDEFGHJKMNPQRSTUVWXYZ23456789";

function newPin() {
  let pin = "";
  while (pin.length < 4) {
    const [value] = crypto.getRandomValues(new Uint8Array(1));
    if (value < 248) pin += PIN_ALPHABET[value % PIN_ALPHABET.length];
  }
  return pin;
}

// Adding a code makes one; the code can be swapped for another or dropped.
function setupPin(form) {
  const pick = form.querySelector("[data-pin-pick]");
  if (!pick) return () => "";
  const add = pick.querySelector("[data-pin-add]");
  const on = pick.querySelector("[data-pin-on]");
  const shown = pick.querySelector("[data-pin-code]");
  let pin = "";
  const set = (next) => {
    pin = next;
    shown.textContent = pin;
    on.hidden = !pin;
    add.hidden = Boolean(pin);
  };
  add.addEventListener("click", () => set(newPin()));
  pick.querySelector("[data-pin-renew]").addEventListener("click", () => set(newPin()));
  pick.querySelector("[data-pin-drop]").addEventListener("click", () => {
    set("");
    add.focus();
  });
  return () => pin;
}

function setupForm(form) {
  const button = form.querySelector("button[type='submit']");
  const textarea = form.querySelector("textarea[name='content']");
  const filename = form.querySelector("input[name='filename']");
  const currentPin = setupPin(form);
  const upload = form.querySelector("input[data-upload]");
  const error = form.querySelector("[data-e2ee-error]");
  if (!button || !textarea || !error) return;
  // The button is disabled in the markup, so without this script nothing can
  // be sent in the clear by pressing it or Enter.
  button.disabled = false;
  const label = button.textContent;
  const maxBytes = Number(form.dataset.maxBytes);

  const fail = (message) => {
    error.textContent = message;
    error.hidden = !message;
  };

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    fail("");
    // A chosen file is sealed as it is, under its name; the box's text as the
    // plain form would have it on the server, where a textarea submits CRLF.
    const file = upload && upload.files.length > 0 ? upload.files[0] : null;
    const content = file
      ? new Uint8Array(await file.arrayBuffer())
      : new TextEncoder().encode(textarea.value.replace(/\r\n?/g, "\n"));
    if (content.length === 0) return fail(form.dataset.msgEmpty);
    if (content.length > maxBytes) return fail(form.dataset.msgTooLarge.replace("%s", sizeText(maxBytes)));
    const checked = form.querySelector("input[name='ttl']:checked");

    button.disabled = true;
    button.textContent = form.dataset.msgEncrypting;
    try {
      const name = (filename ? filename.value.trim() : "") || (file ? file.name : "");
      const pin = currentPin();
      const { envelope, key } = await seal(content, name, pin);
      const body = new FormData();
      body.set("csrf", form.dataset.csrf);
      body.set("ttl", checked ? checked.value : "");
      body.set("envelope", new Blob([envelope], { type: "application/octet-stream" }), "envelope");
      const response = await fetch(form.dataset.endpoint, {
        method: "POST", body, credentials: "same-origin", headers: { Accept: "application/json" },
      });
      if (!response.ok) throw new Error((await response.text()).trim() || form.dataset.msgFailed);
      const { result } = await response.json();
      // #k= carries the key; #p= carries the code, for this page to show its
      // creator - it goes into no link. Either way the fragment stays in this
      // browser.
      window.location.assign(result + (key ? `#k=${key}` : `#p=${pin}`));
    } catch (failure) {
      fail(failure instanceof Error && failure.message ? failure.message : form.dataset.msgFailed);
      button.disabled = false;
      button.textContent = label;
    }
  });
}

function setupResult(result) {
  const hash = window.location.hash;
  const key = hash.startsWith("#k=") ? hash.slice(3) : "";
  const address = result.querySelector(".ticket .addr");
  const copy = result.querySelector(".ticket [data-copy]");
  if (key && address && copy) {
    const full = copy.getAttribute("data-copy") + `#k=${key}`;
    copy.setAttribute("data-copy", full);
    const shown = document.createElement("span");
    shown.className = "tk";
    shown.textContent = `#k=${key}`;
    address.append(shown);
  }
  const pin = hash.startsWith("#p=") ? hash.slice(3).toUpperCase().replace(/[^A-Z0-9]/g, "").slice(0, 4) : "";
  const note = result.querySelector(key ? "[data-e2ee-key]" : pin ? "[data-e2ee-pin]" : "[data-e2ee-lost]");
  if (note && pin) {
    const [before, after = ""] = note.dataset.message.split("{pin}");
    const code = document.createElement("code");
    code.className = "pin-code";
    code.textContent = pin;
    note.replaceChildren(before, code, after);
  }
  if (note) note.hidden = false;
  // The theme and language switches go through a redirect back here. A
  // fragment on the link they follow survives that redirect, so the key does
  // too, without being stored anywhere.
  if (hash) {
    for (const link of document.querySelectorAll("a[href^='/theme?'], a[href^='/language?']")) {
      link.setAttribute("href", link.getAttribute("href").split("#")[0] + hash);
    }
  }
}

for (const form of document.querySelectorAll("form[data-e2ee]")) setupForm(form);
for (const result of document.querySelectorAll("[data-e2ee-result]")) setupResult(result);

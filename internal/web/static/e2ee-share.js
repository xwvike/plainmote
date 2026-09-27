// The home page for an account with end-to-end encryption on. The box's text
// is encrypted here and only the ciphertext is posted; the key goes into the
// address of the result page after #, which is never sent to the server, and
// the result page puts it on the end of the share link.
import { seal } from "./e2ee.js";

const MAX_BYTES = 128 * 1024;

function sizeText(bytes) {
  return bytes >= 1024 ? `${Math.round(bytes / 1024)} KiB` : `${bytes} B`;
}

function setupForm(form) {
  const button = form.querySelector("button[type='submit']");
  const textarea = form.querySelector("textarea[name='content']");
  const filename = form.querySelector("input[name='filename']");
  const passphrase = form.querySelector("[data-passphrase]");
  const error = form.querySelector("[data-e2ee-error]");
  if (!button || !textarea || !error) return;
  // The button is disabled in the markup, so without this script nothing can
  // be sent in the clear by pressing it or Enter.
  button.disabled = false;
  const label = button.textContent;

  const fail = (message) => {
    error.textContent = message;
    error.hidden = !message;
  };

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    fail("");
    // What the plain form does on the server: a textarea submits CRLF.
    const text = textarea.value.replace(/\r\n?/g, "\n");
    const content = new TextEncoder().encode(text);
    if (content.length === 0) return fail(form.dataset.msgEmpty);
    if (content.length > MAX_BYTES) return fail(form.dataset.msgTooLarge.replace("%s", sizeText(MAX_BYTES)));
    const checked = form.querySelector("input[name='ttl']:checked");

    button.disabled = true;
    button.textContent = form.dataset.msgEncrypting;
    try {
      const { envelope, key } = await seal(content, filename ? filename.value.trim() : "", passphrase ? passphrase.value : "");
      const body = new FormData();
      body.set("csrf", form.dataset.csrf);
      body.set("ttl", checked ? checked.value : "");
      body.set("envelope", new Blob([envelope], { type: "application/octet-stream" }), "envelope");
      const response = await fetch(form.dataset.endpoint, {
        method: "POST", body, credentials: "same-origin", headers: { Accept: "application/json" },
      });
      if (!response.ok) throw new Error((await response.text()).trim() || form.dataset.msgFailed);
      const { result } = await response.json();
      // #k= carries the key; #p says the key is the passphrase, told to
      // nobody. Either way the fragment stays in this browser.
      window.location.assign(result + (key ? `#k=${key}` : "#p"));
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
  const note = result.querySelector(key ? "[data-e2ee-key]" : hash === "#p" ? "[data-e2ee-passphrase]" : "[data-e2ee-lost]");
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

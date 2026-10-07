// The home page for an account with a master password, its encryption
// switch on. The box's text, or the file chosen in its place, is encrypted
// here as an encrypted resource is (docs/encryption.md) and only the
// ciphertext is posted, with its keys wrapped: the content key by the
// account key and by the link's key, the link's key - or its code - by the
// account key, so its owner can show the link again. The link's key goes into the address of the result
// page after #, which is never sent to the server, and the result page puts
// it on the end of the share link.
import { newContentKey, wrapContentKey, encryptContent, encryptMeta, newLinkKeys, newCodeLinkKeys, newCode } from "./seal.js";
import { toBase64URL } from "./e2ee.js";
import { accountKey, unlock, WrongPassword, NoStorage } from "./unlock.js";
import { sizeText, textType, fileType } from "./upload.js";

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
  const renew = pick.querySelector("[data-pin-renew]");
  // The key pressed goes away with the press; focus goes on to the code's
  // own controls rather than being dropped.
  add.addEventListener("click", () => {
    set(newCode());
    renew.focus();
  });
  renew.addEventListener("click", () => set(newCode()));
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
  const unlockRow = form.querySelector("[data-e2ee-unlock]");
  const toggle = form.querySelector("[data-e2ee-switch] input");
  if (!button || !textarea || !error || !toggle) return;
  const label = button.textContent;
  // Off, the box is the plain form it is without this script; on, the code
  // can be added and the share is encrypted here.
  const pick = form.querySelector("[data-pin-pick]");
  toggle.addEventListener("change", () => {
    if (pick) pick.hidden = !toggle.checked;
    if (unlockRow && !toggle.checked) unlockRow.hidden = true;
    error.textContent = "";
    error.hidden = true;
  });
  document.addEventListener("plainmote:unlocked", () => {
    if (unlockRow) unlockRow.hidden = true;
  });
  const maxBytes = Number(form.dataset.maxBytes);

  const fail = (message) => {
    error.textContent = message;
    error.hidden = !message;
  };

  // The account key, unlocked in this browser before or with the password
  // asked for here; null while it is still to be typed.
  const key = async () => {
    const kept = await accountKey();
    if (kept) return kept;
    const password = unlockRow?.querySelector("input");
    if (!password || unlockRow.hidden || !password.value) {
      if (unlockRow) unlockRow.hidden = false;
      password?.focus();
      fail(form.dataset.msgUnlock);
      return null;
    }
    try {
      const unlocked = await unlock(password.value);
      unlockRow.hidden = true;
      password.value = "";
      return unlocked;
    } catch (failure) {
      password.select();
      fail(failure instanceof WrongPassword ? form.dataset.msgWrong : failure instanceof NoStorage ? form.dataset.msgNoStorage : form.dataset.msgFailed);
      return null;
    }
  };

  form.addEventListener("submit", async (event) => {
    if (!toggle.checked) return;
    event.preventDefault();
    fail("");
    // A chosen file is encrypted as it is, under its name; the box's text as
    // the plain form would have it on the server, where a textarea submits
    // CRLF.
    const file = upload && upload.files.length > 0 ? upload.files[0] : null;
    const content = file
      ? new Uint8Array(await file.arrayBuffer())
      : new TextEncoder().encode(textarea.value.replace(/\r\n?/g, "\n"));
    if (content.length === 0) return fail(form.dataset.msgEmpty);
    if (content.length > maxBytes) return fail(form.dataset.msgTooLarge.replace("%s", sizeText(maxBytes)));
    const checked = form.querySelector("input[name='ttl']:checked");

    button.disabled = true;
    try {
      const accountKey = await key();
      if (!accountKey) {
        button.disabled = false;
        return;
      }
      button.textContent = form.dataset.msgEncrypting;
      const name = (filename ? filename.value.trim() : "") || (file ? file.name : "");
      const type = file ? await fileType(file) : textType(name);
      const pin = currentPin();
      const id = crypto.randomUUID();
      const linkID = crypto.randomUUID();
      const contentKey = await newContentKey();
      const link = pin ? await newCodeLinkKeys(contentKey, accountKey, linkID, pin) : await newLinkKeys(contentKey, accountKey, linkID);
      const body = new FormData();
      body.set("csrf", form.dataset.csrf);
      body.set("ttl", checked ? checked.value : "");
      body.set("id", id);
      body.set("link_id", linkID);
      body.set("sealed_key", toBase64URL(await wrapContentKey(contentKey, accountKey, id)));
      body.set("link_key", toBase64URL(link.sealedKey));
      body.set("owner_key", toBase64URL(link.ownerKey));
      body.set("meta", toBase64URL(await encryptMeta(contentKey, id, { name: "", filename: name, type })));
      body.set("content", new Blob([await encryptContent(contentKey, id, content)], { type: "application/octet-stream" }), "blob");
      const response = await fetch(form.dataset.endpoint, {
        method: "POST", body, credentials: "same-origin", headers: { Accept: "application/json" },
      });
      if (!response.ok) throw new Error((await response.text()).trim() || form.dataset.msgFailed);
      const { result } = await response.json();
      // #k= carries the key; #p= carries the code, for this page to show its
      // creator - it goes into no link. Either way the fragment stays in this
      // browser.
      window.location.assign(result + (pin ? `#p=${pin}` : `#k=${toBase64URL(link.bytes)}`));
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
  // With the key the link says everything; otherwise the page says either the
  // code or that the key is gone.
  const note = key ? null : result.querySelector(pin ? "[data-e2ee-pin]" : "[data-e2ee-lost]");
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

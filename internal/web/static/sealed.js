// End-to-end encrypted resources on the resource page: opening one once
// unlocked, saving it encrypted, its share links and their full addresses,
// turning encryption on or off - every earlier version carried across - and
// creating a new resource encrypted. The service only ever receives what
// seal.js produced; docs/encryption.md is the design.
import {
  newContentKey, wrapContentKey, openContentKey, encryptContent, decryptContent,
  encryptMeta, decryptMeta, newLinkKeys, openLinkKey, newCodeLinkKeys, openLinkCode, newCode,
} from "./seal.js";
import { toBase64URL, fromBase64URL } from "./e2ee.js";
import { accountKey, unlock, WrongPassword, NoStorage } from "./unlock.js";
import { textLike, textType, readsAsText, fileType } from "./upload.js";

const csrf = document.querySelector('input[name="csrf"]')?.value || "";
const encoder = new TextEncoder();
const sealedRoot = document.querySelector("[data-sealed]");
const toggle = document.querySelector("[data-seal-switch]");

function say(element, text) {
  if (element) element.textContent = text || "";
}

// Plaintext in another encoding is carried across as UTF-8.
function utf8Bytes(bytes, type, encoding) {
  if (!textLike(type) || !encoding || /^utf-?8$/i.test(encoding)) return { bytes, type };
  const text = new TextDecoder(encoding).decode(bytes);
  return { bytes: encoder.encode(text), type: type.replace(/;\s*charset=[^;]+/i, "") + "; charset=utf-8" };
}

async function bytesOf(url) {
  const response = await fetch(url, { credentials: "same-origin" });
  if (!response.ok) throw new Error(`read ${url}: ${response.status}`);
  return new Uint8Array(await response.arrayBuffer());
}

function blobPart(form, name, bytes) {
  form.append(name, new Blob([bytes], { type: "application/octet-stream" }), "blob");
}

async function post(url, fields, files = {}) {
  const form = new FormData();
  form.append("csrf", csrf);
  for (const [key, value] of Object.entries(fields)) form.append(key, value);
  for (const [key, value] of Object.entries(files)) blobPart(form, key, value);
  const response = await fetch(url, { method: "POST", body: form, credentials: "same-origin", headers: { Accept: "application/json" } });
  const answer = await response.json().catch(() => ({}));
  return { status: response.status, ok: response.ok, answer };
}

// --- An encrypted resource -------------------------------------------------

const opened = { key: null, contentKey: null, meta: null, text: null };

// The name goes wherever the page shows it - again after parts.js swaps
// the title or the crumb back in with the placeholder.
function nameTitles(meta) {
  const label = meta.name || meta.filename;
  if (!label) return;
  for (const title of document.querySelectorAll("[data-sealed-title]")) title.textContent = label;
  const crumb = document.querySelector('[data-part="crumb"] > span:last-child');
  if (crumb) crumb.textContent = label;
  document.title = `${label} · PlainMote`;
}

// A quick share not yet kept is shown, not edited: no name or filename to
// change, and its text in a preview rather than the editor.
const readOnly = Boolean(sealedRoot?.hasAttribute("data-read-only"));

function showOpened(meta, content) {
  const id = sealedRoot.dataset.resource;
  const label = meta.name || meta.filename;
  nameTitles(meta);
  const form = sealedRoot.querySelector("[data-sealed-form]");
  for (const [selector, value] of [["[data-sealed-name]", meta.name], ["[data-sealed-filename]", meta.filename]]) {
    const field = form.querySelector(selector);
    if (field) field.value = value;
  }
  say(form.querySelector("[data-sealed-meta-line]"), `${meta.type.split(";")[0]} · ${content.length.toLocaleString()} B`);
  const textarea = form.querySelector("[data-sealed-text]");
  const media = form.querySelector("[data-sealed-media]");
  if (textLike(meta.type)) {
    opened.text = new TextDecoder().decode(content);
    textarea.value = opened.text;
    textarea.dataset.filename = meta.filename;
    textarea.dataset.contentType = meta.type;
    textarea.dataset.encoding = "utf-8";
    textarea.dataset.eol = content.includes(13) ? "crlf" : "lf";
    textarea.removeAttribute("data-lazy-editor");
    if (readOnly) {
      textarea.disabled = true;
      textarea.setAttribute("data-preview", "");
    } else {
      textarea.setAttribute("data-editor", "");
      form.querySelector("[data-sealed-text-tags]").hidden = false;
    }
    textarea.hidden = false;
    import("./editor.js");
  } else {
    const url = URL.createObjectURL(new Blob([content], { type: meta.type }));
    media.replaceChildren();
    let element;
    if (meta.type.startsWith("image/")) {
      element = document.createElement("img");
      element.className = "resource-media resource-image";
      element.alt = label || "";
    } else if (meta.type.startsWith("audio/") || meta.type.startsWith("video/")) {
      element = document.createElement(meta.type.startsWith("audio/") ? "audio" : "video");
      element.className = `resource-media resource-${meta.type.startsWith("audio/") ? "audio" : "video"}`;
      element.controls = true;
    }
    // Marked as the current content, so upload.js hides it while a chosen
    // file is previewed in its place.
    media.dataset.currentContent = "";
    if (element) {
      element.src = url;
      media.append(element);
    } else {
      const card = document.createElement("div");
      card.className = "file-card";
      const name = document.createElement("strong");
      name.textContent = meta.filename || label || id;
      const download = document.createElement("a");
      download.className = "btn sm";
      download.href = url;
      download.download = meta.filename || "file";
      download.textContent = sealedRoot.dataset.msgDownload || "↓";
      card.append(name, download);
      media.append(card);
    }
    media.hidden = false;
  }
  sealedRoot.querySelector("[data-sealed-placeholder]").hidden = true;
  form.hidden = false;
}

async function openSealed(key) {
  const id = sealedRoot.dataset.resource;
  opened.key = key;
  opened.contentKey = await openContentKey(fromBase64URL(sealedRoot.dataset.sealedKey), key, id);
  opened.meta = await decryptMeta(opened.contentKey, id, fromBase64URL(sealedRoot.dataset.sealedMeta));
  const content = await decryptContent(opened.contentKey, id, await bytesOf(`/resources/${id}/raw`));
  showOpened(opened.meta, content);
  await fillLinks(key);
}

async function saveSealed(event) {
  event.preventDefault();
  const form = event.currentTarget;
  const id = sealedRoot.dataset.resource;
  const problem = form.querySelector("[data-sealed-save-error]");
  say(problem, "");
  for (const button of form.querySelectorAll("button")) button.disabled = true;
  try {
    const filename = form.querySelector("[data-sealed-filename]").value.trim();
    const meta = { name: form.querySelector("[data-sealed-name]").value.trim(), filename, type: opened.meta.type };
    let content = null;
    // A chosen text file is read into the editor, which is then what is
    // saved; any other chosen file is saved as it is.
    const chosen = form.querySelector("[data-sealed-file]").files[0] || null;
    const intoEditor = Boolean(chosen) && opened.text !== null && (await readsAsText(chosen));
    if (chosen && !intoEditor) {
      content = new Uint8Array(await chosen.arrayBuffer());
      meta.type = await fileType(chosen);
    } else if (opened.text !== null) {
      const text = form.querySelector("[data-sealed-text]").value;
      if (text !== opened.text) content = encoder.encode(text);
      if (filename !== opened.meta.filename || !textLike(meta.type)) meta.type = textType(filename);
    }
    const fields = { action: "save", base_version: sealedRoot.dataset.version, meta: toBase64URL(await encryptMeta(opened.contentKey, id, meta)) };
    const files = content ? { content: await encryptContent(opened.contentKey, id, content) } : {};
    const result = await post(`/resources/${id}/sealed`, fields, files);
    if (!result.ok) throw new Error(result.answer.message || sealedRoot.dataset.msgFailed);
    location.assign(result.answer.location);
  } catch (error) {
    say(problem, error.message || sealedRoot.dataset.msgFailed);
    for (const button of form.querySelectorAll("button")) button.disabled = false;
  }
}

// The share list's addresses are completed with their keys - or shown with
// the code that opens them - in the list, in the terms dialog and on a quick
// share's page, again whenever parts.js swaps them in.
async function fillLinks(key) {
  for (const code of document.querySelectorAll("[data-sealed-link]")) {
    if (code.dataset.filled === "true") continue;
    const linkID = code.dataset.sealedLink;
    try {
      const ownerKey = fromBase64URL(code.dataset.ownerKey);
      let full = code.dataset.url;
      if (code.hasAttribute("data-code-link")) {
        const pin = await openLinkCode(ownerKey, key, linkID);
        for (const chip of document.querySelectorAll(`[data-code-chip="${linkID}"]`)) {
          chip.querySelector("[data-code-value]").textContent = pin;
          chip.hidden = false;
        }
      } else {
        full = `${full}#k=${toBase64URL(await openLinkKey(ownerKey, key, linkID))}`;
      }
      code.textContent = full;
      code.dataset.filled = "true";
      for (const button of document.querySelectorAll(`[data-sealed-copy="${linkID}"]`)) {
        button.dataset.copy = full;
        button.disabled = false;
      }
      for (const toggle of document.querySelectorAll(`[data-sealed-code-toggle="${linkID}"]`)) toggle.disabled = false;
    } catch {
      // A link made before a key could not be read stays as it is.
    }
  }
  for (const button of document.querySelectorAll("[data-sealed-share]")) button.disabled = false;
}

// A link action posted the way the page's own forms are: through parts.js
// where the page has a share list to swap in, so an edit in progress stays
// as it is; as a plain form where it has not.
function postShare(fields) {
  const form = document.createElement("form");
  form.method = "post";
  form.action = `/resources/${sealedRoot.dataset.resource}/share`;
  form.hidden = true;
  const viaParts = Boolean(document.querySelector("[data-share-panel]"));
  if (viaParts) form.setAttribute("data-parts", "");
  for (const [name, value] of Object.entries({ csrf, ...fields })) {
    const input = document.createElement("input");
    input.type = "hidden";
    input.name = name;
    input.value = value;
    form.append(input);
  }
  document.body.append(form);
  form.requestSubmit();
  // parts.js read the form as it was submitted.
  if (viaParts) form.remove();
}

// A link's keys: a random key for the address after #, or one derived from
// a new code. The content key is opened able to be wrapped for it.
async function linkKeys(linkID, withCode) {
  const id = sealedRoot.dataset.resource;
  const contentKey = await openContentKey(fromBase64URL(sealedRoot.dataset.sealedKey), opened.key, id, true);
  const keys = withCode
    ? await newCodeLinkKeys(contentKey, opened.key, linkID, newCode())
    : await newLinkKeys(contentKey, opened.key, linkID);
  return { sealed_key: toBase64URL(keys.sealedKey), owner_key: toBase64URL(keys.ownerKey) };
}

async function newSealedShare(button) {
  button.disabled = true;
  try {
    const linkID = crypto.randomUUID();
    postShare({ action: "create", link_id: linkID, ...(await linkKeys(linkID, false)) });
  } finally {
    button.disabled = false;
  }
}

// A link opened by a code, or by its key again, or with a new code: new
// keys, the same address.
async function rekey(linkID, withCode, control) {
  control.disabled = true;
  try {
    postShare({ action: "rekey", share_id: linkID, ...(await linkKeys(linkID, withCode)) });
  } catch {
    control.disabled = false;
    if (control.type === "checkbox") control.checked = !control.checked;
  }
}

// The list is swapped in whole by parts.js, so its controls are heard here.
document.addEventListener("change", (event) => {
  const toggle = event.target.closest?.("[data-sealed-code-toggle]");
  if (toggle && opened.key) rekey(toggle.dataset.sealedCodeToggle, toggle.checked, toggle);
});
document.addEventListener("click", (event) => {
  const renew = event.target.closest?.("[data-sealed-recode]");
  if (renew && opened.key) rekey(renew.dataset.sealedRecode, true, renew);
  const share = event.target.closest?.("[data-sealed-share]");
  if (share && opened.key) newSealedShare(share);
});

async function startSealed() {
  const unlockForm = sealedRoot.querySelector("[data-sealed-unlock]");
  const status = sealedRoot.querySelector("[data-sealed-status]");
  sealedRoot.querySelector("[data-sealed-form]").addEventListener("submit", saveSealed);
  // A non-text resource given a text file: upload.js has made the editor,
  // which now holds what will be saved.
  sealedRoot.querySelector("[data-sealed-form]").addEventListener("plainmote:text-editor", () => {
    opened.text = "";
  });
  document.addEventListener("plainmote:parts", () => {
    if (!opened.key) return;
    nameTitles(opened.meta);
    fillLinks(opened.key);
  });
  document.addEventListener("plainmote:locked", () => location.reload());
  // Unlocked from the top bar while this waited for it.
  document.addEventListener("plainmote:unlocked", (event) => {
    if (opened.key) return;
    unlockForm.hidden = true;
    tryOpen(event.detail.key);
  });
  const tryOpen = async (key) => {
    say(status, sealedRoot.dataset.msgDecrypting || "");
    try {
      await openSealed(key);
    } catch {
      say(status, sealedRoot.dataset.msgFailed);
    }
  };
  const key = await accountKey();
  if (key) return tryOpen(key);
  // The page says the content needs scripts; they are here, and it needs
  // unlocking.
  say(status, sealedRoot.dataset.msgLocked);
  unlockForm.hidden = false;
  unlockForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    const problem = unlockForm.querySelector("[data-sealed-error]");
    say(problem, "");
    try {
      // What opens it is the listener above, as from the top bar.
      await unlock(unlockForm.elements.password.value);
    } catch (error) {
      say(problem, error instanceof WrongPassword ? sealedRoot.dataset.msgWrong
        : error instanceof NoStorage ? sealedRoot.dataset.msgNoStorage : sealedRoot.dataset.msgFailed);
    }
  });
}

// --- Turning encryption on and off ------------------------------------------

async function keyFor(dialog) {
  const key = await accountKey();
  if (key) return key;
  const field = dialog.querySelector("[data-seal-password]");
  const password = field.querySelector("input").value;
  if (field.hidden || !password) {
    field.hidden = false;
    field.querySelector("input").required = true;
    throw new Error(toggle.dataset.msgUnlock);
  }
  try {
    return await unlock(password);
  } catch (error) {
    throw new Error(error instanceof WrongPassword ? toggle.dataset.msgWrong : toggle.dataset.msgFailed);
  }
}

function progress(dialog, done, total) {
  say(dialog.querySelector("[data-seal-progress]"), toggle.dataset.msgProgress.replace("%d", done).replace("%d", total));
}

async function sealResource(dialog) {
  const key = await keyFor(dialog);
  const id = toggle.dataset.resource;
  const history = JSON.parse(toggle.dataset.history || "[]");
  const total = history.length + 1;
  const contentKey = await newContentKey();
  const fields = { action: "seal", base_version: toggle.dataset.version, sealed_key: toBase64URL(await wrapContentKey(contentKey, key, id)) };
  const files = {};
  progress(dialog, 1, total);
  const current = utf8Bytes(await bytesOf(`/resources/${id}/raw`), toggle.dataset.type, toggle.dataset.encoding);
  files.content = await encryptContent(contentKey, id, current.bytes);
  fields.meta = toBase64URL(await encryptMeta(contentKey, id, { name: toggle.dataset.name, filename: toggle.dataset.filename, type: current.type }));
  for (const [index, version] of history.entries()) {
    progress(dialog, index + 2, total);
    const plain = utf8Bytes(await bytesOf(`/resources/${id}/versions/${version.n}/raw`), version.t || "", version.e || "");
    files[`version_${version.n}_content`] = await encryptContent(contentKey, id, plain.bytes);
    fields[`version_${version.n}_meta`] = toBase64URL(await encryptMeta(contentKey, id, { name: "", filename: version.f || "", type: plain.type }));
  }
  return post(`/resources/${id}/sealed`, fields, files);
}

async function unsealResource(dialog) {
  const key = await keyFor(dialog);
  const id = toggle.dataset.resource;
  const history = JSON.parse(toggle.dataset.history || "[]");
  const total = history.length + 1;
  const contentKey = await openContentKey(fromBase64URL(sealedRoot.dataset.sealedKey), key, id);
  progress(dialog, 1, total);
  const meta = await decryptMeta(contentKey, id, fromBase64URL(sealedRoot.dataset.sealedMeta));
  const fields = { action: "unseal", base_version: toggle.dataset.version, name: meta.name, filename: meta.filename };
  const files = { content: await decryptContent(contentKey, id, await bytesOf(`/resources/${id}/raw`)) };
  for (const [index, version] of history.entries()) {
    progress(dialog, index + 2, total);
    const versionMeta = await decryptMeta(contentKey, id, fromBase64URL(version.m));
    files[`version_${version.n}_content`] = await decryptContent(contentKey, id, await bytesOf(`/resources/${id}/versions/${version.n}/raw`));
    fields[`version_${version.n}_filename`] = versionMeta.filename;
  }
  return post(`/resources/${id}/sealed`, fields, files);
}

function startToggle() {
  const input = toggle.querySelector("input");
  input.disabled = false;
  if (!toggle.dataset.resource) return startNew(input);
  const sealed = input.checked;
  const dialog = document.querySelector(`[data-seal-dialog="${sealed ? "off" : "on"}"]`);
  if (!dialog) return;
  input.addEventListener("change", () => {
    // The switch shows what the resource is; it moves once that changes.
    input.checked = sealed;
    const form = dialog.querySelector("form");
    form.reset();
    say(dialog.querySelector("[data-seal-error]"), "");
    say(dialog.querySelector("[data-seal-progress]"), "");
    dialog.showModal();
  });
  for (const close of dialog.querySelectorAll("[data-seal-close]")) close.addEventListener("click", () => dialog.close());
  dialog.querySelector("form").addEventListener("submit", async (event) => {
    event.preventDefault();
    const form = event.currentTarget;
    for (const button of form.querySelectorAll("button")) button.disabled = true;
    say(dialog.querySelector("[data-seal-error]"), "");
    try {
      const result = await (sealed ? unsealResource(dialog) : sealResource(dialog));
      if (!result.ok) throw new Error(result.answer.message || toggle.dataset.msgFailed);
      location.assign(result.answer.location);
    } catch (error) {
      say(dialog.querySelector("[data-seal-error]"), error.message || toggle.dataset.msgFailed);
      say(dialog.querySelector("[data-seal-progress]"), "");
      for (const button of form.querySelectorAll("button")) button.disabled = false;
    }
  });
}

// --- A new resource, encrypted from the start --------------------------------

function startNew(input) {
  const form = document.querySelector("form.rform");
  const unlockForm = document.querySelector("[data-new-unlock]");
  if (!form) return;
  let key = null;
  const needKey = async () => {
    key = key || (await accountKey());
    if (unlockForm) unlockForm.hidden = Boolean(key) || !input.checked;
  };
  input.addEventListener("change", needKey);
  document.addEventListener("plainmote:unlocked", (event) => {
    key = event.detail.key;
    if (unlockForm) unlockForm.hidden = true;
  });
  unlockForm?.addEventListener("submit", async (event) => {
    event.preventDefault();
    const problem = unlockForm.querySelector("[data-new-unlock-error]");
    say(problem, "");
    try {
      key = await unlock(unlockForm.elements.password.value);
      unlockForm.hidden = true;
    } catch (error) {
      say(problem, error instanceof WrongPassword ? toggle.dataset.msgWrong : toggle.dataset.msgFailed);
    }
  });
  // After the editor has put its text in the textarea, in the capture phase.
  form.addEventListener("submit", async (event) => {
    if (!input.checked) return;
    event.preventDefault();
    event.stopImmediatePropagation();
    await needKey();
    if (!key) return;
    const buttons = form.querySelectorAll("button");
    for (const button of buttons) button.disabled = true;
    try {
      const id = crypto.randomUUID();
      const file = form.querySelector("input[data-upload]")?.files[0];
      const filename = (form.elements.filename?.value || "").trim() || file?.name || "";
      const content = file ? new Uint8Array(await file.arrayBuffer()) : encoder.encode(form.elements.content.value);
      const type = file ? await fileType(file) : textType(filename);
      const contentKey = await newContentKey();
      const result = await post("/resources/sealed", {
        id,
        sealed_key: toBase64URL(await wrapContentKey(contentKey, key, id)),
        meta: toBase64URL(await encryptMeta(contentKey, id, { name: (form.elements.name?.value || "").trim(), filename, type })),
      }, { content: await encryptContent(contentKey, id, content) });
      if (!result.ok) throw new Error(result.answer.message || toggle.dataset.msgFailed);
      location.assign(result.answer.location);
    } catch (error) {
      alertIn(form, error.message || toggle.dataset.msgFailed);
      for (const button of buttons) button.disabled = false;
    }
  });
}

function alertIn(form, text) {
  let slot = form.querySelector("[data-new-sealed-error]");
  if (!slot) {
    slot = document.createElement("p");
    slot.className = "err";
    slot.setAttribute("role", "alert");
    slot.dataset.newSealedError = "";
    form.querySelector(".savebar")?.append(slot);
  }
  slot.textContent = text;
}

if (globalThis.crypto?.subtle) {
  if (sealedRoot) startSealed();
  if (toggle) startToggle();
}

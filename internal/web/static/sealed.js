// End-to-end encrypted resources on the resource page: opening one once
// unlocked, saving it encrypted, its share links and their full addresses,
// turning encryption on or off - every earlier version carried across - and
// creating a new resource encrypted. The service only ever receives what
// seal.js produced; docs/encryption.md is the design.
import {
  newContentKey, wrapContentKey, openContentKey, encryptContent, decryptContent,
  encryptMeta, decryptMeta, newLinkKeys, openLinkKey,
} from "./seal.js";
import { toBase64URL, fromBase64URL } from "./e2ee.js";
import { accountKey, unlock, WrongPassword, NoStorage } from "./unlock.js";
import { classifyUpload, uploadController } from "./upload.js";

const csrf = document.querySelector('input[name="csrf"]')?.value || "";
const encoder = new TextEncoder();
const sealedRoot = document.querySelector("[data-sealed]");
const toggle = document.querySelector("[data-seal-switch]");

function say(element, text) {
  if (element) element.textContent = text || "";
}

// What a resource's bytes are, from its type: text goes in the editor.
function textLike(type) {
  return /^text\//.test(type) || /(json|xml|yaml|toml|javascript|x-sh|sql|x-ndjson)/.test(type);
}

// A type for content made here, from its filename - the service cannot look
// at encrypted bytes, so what a resource is is decided before encrypting.
const extensionTypes = {
  json: "application/json", yaml: "text/yaml", yml: "text/yaml", toml: "application/toml", xml: "application/xml",
  sh: "text/x-sh", sql: "text/x-sql", md: "text/markdown", csv: "text/csv", html: "text/plain", htm: "text/plain",
};
function textType(filename) {
  const extension = (filename.split(".").pop() || "").toLowerCase();
  return `${extensionTypes[extension] || "text/plain"}; charset=utf-8`;
}

function isUTF8(bytes) {
  try {
    new TextDecoder("utf-8", { fatal: true }).decode(bytes);
    return true;
  } catch {
    return false;
  }
}

// What a chosen file is, by the same reading upload.js gives it: text by
// its name or type, or - for a file of no telling type - by whether it reads
// as UTF-8; media as the browser types it; anything else a download.
async function readsAsText(file) {
  const kind = classifyUpload(file);
  return kind === "text" || (kind === "unknown" && isUTF8(new Uint8Array(await file.arrayBuffer())));
}

async function fileType(file) {
  if (await readsAsText(file)) return textType(file.name);
  const kind = classifyUpload(file);
  if ((kind === "image" || kind === "audio" || kind === "video") && file.type) return file.type;
  return "application/octet-stream";
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

function showOpened(meta, content) {
  const id = sealedRoot.dataset.resource;
  const label = meta.name || meta.filename;
  nameTitles(meta);
  const form = sealedRoot.querySelector("[data-sealed-form]");
  form.querySelector("[data-sealed-name]").value = meta.name;
  form.querySelector("[data-sealed-filename]").value = meta.filename;
  say(form.querySelector("[data-sealed-meta-line]"), `${meta.type.split(";")[0]} · ${content.length.toLocaleString()} B`);
  const textarea = form.querySelector("[data-sealed-text]");
  const media = form.querySelector("[data-sealed-media]");
  if (textLike(meta.type)) {
    opened.text = new TextDecoder().decode(content);
    textarea.value = opened.text;
    textarea.dataset.filename = meta.filename;
    textarea.dataset.contentType = meta.type;
    textarea.setAttribute("data-editor", "");
    textarea.hidden = false;
    form.querySelector("[data-sealed-text-tags]").hidden = false;
    say(form.querySelector("[data-sealed-eol]"), content.includes(13) ? "CRLF" : "LF");
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

// A non-text resource given a text file becomes text on the spot: the file
// goes into the editor, as it would on a text resource, and saving it saves
// a text resource.
function watchChosenText(form) {
  const input = form.querySelector("[data-sealed-file]");
  input.addEventListener("change", async () => {
    const file = input.files[0];
    if (!file || opened.text !== null || !(await readsAsText(file))) return;
    const media = form.querySelector("[data-sealed-media]");
    media.hidden = true;
    delete media.dataset.currentContent;
    const textarea = form.querySelector("[data-sealed-text]");
    textarea.value = new TextDecoder().decode(new Uint8Array(await file.arrayBuffer()));
    // The whole content is replaced, so is the name it goes by.
    form.querySelector("[data-sealed-filename]").value = file.name;
    textarea.dataset.filename = file.name;
    textarea.dataset.contentType = textType(file.name);
    textarea.setAttribute("data-editor", "");
    textarea.hidden = false;
    form.querySelector("[data-sealed-text-tags]").hidden = false;
    // The text is the editor's now: the file and its card are let go.
    uploadController(input)?.useEditor();
    opened.text = "";
    import("./editor.js");
  });
}

async function openSealed(key) {
  const id = sealedRoot.dataset.resource;
  opened.key = key;
  opened.contentKey = await openContentKey(fromBase64URL(sealedRoot.dataset.sealedKey), key, id);
  opened.meta = await decryptMeta(opened.contentKey, id, fromBase64URL(sealedRoot.dataset.sealedMeta));
  const content = await decryptContent(opened.contentKey, id, await bytesOf(`/resources/${id}/raw`));
  showOpened(opened.meta, content);
  watchChosenText(sealedRoot.querySelector("[data-sealed-form]"));
  await fillLinks(key);
  for (const button of document.querySelectorAll("[data-sealed-share]")) button.disabled = false;
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

// The share list's addresses are completed with their keys, here and in the
// terms dialog - again whenever parts.js swaps them in.
async function fillLinks(key) {
  for (const code of document.querySelectorAll("[data-sealed-link]")) {
    if (code.dataset.filled === "true") continue;
    try {
      const linkKey = await openLinkKey(fromBase64URL(code.dataset.ownerKey), key, code.dataset.sealedLink);
      const full = `${code.dataset.url}#k=${toBase64URL(linkKey)}`;
      code.textContent = full;
      code.dataset.filled = "true";
      for (const button of document.querySelectorAll(`[data-sealed-copy="${code.dataset.sealedLink}"]`)) {
        button.dataset.copy = full;
        button.disabled = false;
      }
    } catch {
      // A link made before a key could not be read stays as it is.
    }
  }
}

async function newSealedShare(button) {
  button.disabled = true;
  try {
    const id = sealedRoot.dataset.resource;
    const linkID = crypto.randomUUID();
    const contentKey = await openContentKey(fromBase64URL(sealedRoot.dataset.sealedKey), opened.key, id, true);
    const keys = await newLinkKeys(contentKey, opened.key, linkID);
    const response = await fetch(`/resources/${id}/share`, {
      method: "POST",
      credentials: "same-origin",
      body: new URLSearchParams({ csrf, action: "create", link_id: linkID, sealed_key: toBase64URL(keys.sealedKey), owner_key: toBase64URL(keys.ownerKey) }),
    });
    location.assign(response.url || location.href);
  } catch {
    button.disabled = false;
  }
}

async function startSealed() {
  const unlockForm = sealedRoot.querySelector("[data-sealed-unlock]");
  const status = sealedRoot.querySelector("[data-sealed-status]");
  sealedRoot.querySelector("[data-sealed-form]").addEventListener("submit", saveSealed);
  for (const button of document.querySelectorAll("[data-sealed-share]")) button.addEventListener("click", () => newSealedShare(button));
  document.addEventListener("plainmote:parts", () => {
    if (!opened.key) return;
    nameTitles(opened.meta);
    fillLinks(opened.key);
  });
  document.addEventListener("plainmote:locked", () => location.reload());
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
  unlockForm.hidden = false;
  unlockForm.addEventListener("submit", async (event) => {
    event.preventDefault();
    const problem = unlockForm.querySelector("[data-sealed-error]");
    say(problem, "");
    try {
      const unlocked = await unlock(unlockForm.elements.password.value);
      unlockForm.hidden = true;
      await tryOpen(unlocked);
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
  if (!toggle.hasAttribute("data-ready")) return;
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

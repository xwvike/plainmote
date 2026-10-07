// An encrypted resource's history pages: its name in the titles, an earlier
// version opened, two versions compared, and a version copied into a new
// resource - encrypted again under that resource's own key. All of it in
// the browser, once unlocked; the pages themselves hold only ciphertext.
import { newContentKey, wrapContentKey, openContentKey, encryptContent, decryptContent, encryptMeta, decryptMeta } from "./seal.js";
import { toBase64URL, fromBase64URL } from "./e2ee.js";
import { compare } from "./diff.js";
import { accountKey, unlock, WrongPassword, NoStorage } from "./unlock.js";

const page = document.querySelector("[data-sealed-page]");
const msg = (name) => page?.dataset[name] || "";
const csrf = document.querySelector('input[name="csrf"]')?.value || "";
const id = page?.dataset.resource || "";

function textLike(type) {
  return /^text\//.test(type) || /(json|xml|yaml|toml|javascript|x-sh|sql|x-ndjson)/.test(type);
}

async function bytesOf(url) {
  const response = await fetch(url, { credentials: "same-origin" });
  if (!response.ok) throw new Error(String(response.status));
  return new Uint8Array(await response.arrayBuffer());
}

// Each side's own metadata, falling back to the resource's when a version
// carries none.
async function openSide(contentKey, raw, metaB64) {
  const meta = await decryptMeta(contentKey, id, fromBase64URL(metaB64 || page.dataset.sealedMeta));
  return { meta, content: await decryptContent(contentKey, id, await bytesOf(raw)) };
}

function nameTitles(meta) {
  const label = meta.name || meta.filename;
  if (!label) return;
  for (const title of document.querySelectorAll("[data-sealed-title]")) title.textContent = label;
  document.title = document.title.replace(page.dataset.placeholder || "", label);
}

function showVersion(holder, side) {
  const { meta, content } = side;
  holder.querySelector("[data-sealed-status]").hidden = true;
  if (textLike(meta.type)) {
    const textarea = holder.querySelector("[data-sealed-text]");
    textarea.value = new TextDecoder().decode(content);
    textarea.dataset.filename = meta.filename;
    textarea.dataset.contentType = meta.type;
    textarea.setAttribute("data-preview", "");
    textarea.hidden = false;
    import("./editor.js");
    return;
  }
  const media = holder.querySelector("[data-sealed-media]");
  const url = URL.createObjectURL(new Blob([content], { type: meta.type }));
  let element;
  if (meta.type.startsWith("image/")) {
    element = document.createElement("img");
    element.className = "resource-media resource-image";
  } else if (meta.type.startsWith("audio/") || meta.type.startsWith("video/")) {
    element = document.createElement(meta.type.startsWith("audio/") ? "audio" : "video");
    element.className = "resource-media";
    element.controls = true;
  }
  if (element) {
    element.src = url;
    media.append(element);
  } else {
    const card = document.createElement("div");
    card.className = "file-card";
    const name = document.createElement("strong");
    name.textContent = meta.filename || `v${holder.dataset.number}`;
    const link = document.createElement("a");
    link.className = "btn sm";
    link.href = url;
    link.download = meta.filename || "file";
    link.textContent = msg("msgDownload");
    card.append(name, link);
    media.append(card);
  }
  media.hidden = false;
}

// The same rows the server draws for a plain comparison.
// One side of a comparison that is not text, as the server draws it for a
// plain resource: looked at, or played, next to the other.
async function sideOf(holder, which, side) {
  const { meta, content } = side;
  const number = holder.dataset[`${which}Number`];
  const section = document.createElement("section");
  section.className = "cmp-side";
  const header = document.createElement("header");
  const link = document.createElement("a");
  link.className = "v";
  link.href = holder.dataset[`${which}Page`];
  link.textContent = `v${number}`;
  header.append(link);
  if (holder.hasAttribute(`data-${which}-current`)) {
    const tag = document.createElement("span");
    tag.className = "cur-tag";
    tag.textContent = holder.dataset.currentLabel;
    header.append(tag);
  }
  const about = document.createElement("em");
  about.textContent = `${meta.type.split(";")[0]} · ${content.length.toLocaleString()} B`;
  header.append(about);
  const stage = document.createElement("div");
  stage.className = "media-stage cmp-stage";
  const url = URL.createObjectURL(new Blob([content], { type: meta.type }));
  let element;
  if (meta.type.startsWith("image/")) {
    element = document.createElement("img");
    element.className = "resource-media resource-image";
    element.alt = `v${number}`;
  } else if (meta.type.startsWith("audio/") || meta.type.startsWith("video/")) {
    element = document.createElement(meta.type.startsWith("audio/") ? "audio" : "video");
    element.className = "resource-media";
    element.controls = true;
  }
  if (element) {
    element.src = url;
    stage.append(element);
  } else {
    const card = document.createElement("div");
    card.className = "file-card";
    const name = document.createElement("strong");
    name.textContent = meta.filename || `v${number}`;
    const open = document.createElement("a");
    open.className = "btn sm";
    open.href = url;
    open.download = meta.filename || `v${number}`;
    open.textContent = msg("msgDownload");
    card.append(name, open);
    stage.append(card);
  }
  const footer = document.createElement("footer");
  const digest = new Uint8Array(await crypto.subtle.digest("SHA-256", content));
  const hex = [...digest].map((byte) => byte.toString(16).padStart(2, "0")).join("");
  const code = document.createElement("code");
  code.title = hex;
  code.textContent = hex.slice(0, 12);
  footer.append("sha256 ", code);
  section.append(header, stage, footer);
  return section;
}

async function showDiff(holder, before, after) {
  const status = holder.querySelector("[data-sealed-status]");
  const bothText = textLike(before.meta.type) && textLike(after.meta.type);
  const same = before.content.length === after.content.length && before.content.every((byte, i) => byte === after.content[i]);
  if (!bothText) {
    status.textContent = same ? msg("msgSame") : msg("msgDiffer");
    const sides = holder.querySelector("[data-sealed-sides]");
    sides.replaceChildren(await sideOf(holder, "from", before), await sideOf(holder, "to", after));
    sides.hidden = false;
    return;
  }
  const decoder = new TextDecoder();
  const diff = compare(decoder.decode(before.content), decoder.decode(after.content), holder.dataset.full === "1");
  if (diff.tooLarge) {
    status.textContent = msg("msgTooLarge");
    return;
  }
  if (diff.added + diff.removed === 0) {
    status.textContent = msg("msgSame");
    return;
  }
  status.hidden = true;
  const sheet = holder.querySelector("[data-sealed-diff]");
  for (const line of diff.lines) {
    if (line.kind === "fold") {
      const fold = document.createElement("span");
      fold.className = "fold";
      fold.textContent = line.skipped === 1 ? msg("msgFoldOne") : msg("msgFold").replace("%d", line.skipped);
      sheet.append(fold);
      continue;
    }
    const row = document.createElement("div");
    row.className = `row${line.kind === "+" ? " add" : line.kind === "-" ? " del" : ""}`;
    for (const text of [line.old || "", line.new || "", line.kind === "+" ? "+" : line.kind === "-" ? "−" : "", line.text]) {
      const cell = document.createElement("span");
      cell.textContent = String(text);
      row.append(cell);
    }
    sheet.append(row);
  }
  sheet.hidden = false;
  const stat = document.querySelector("[data-sealed-stat]");
  if (stat) {
    stat.querySelector(".a").textContent = `+${diff.added}`;
    stat.querySelector(".d").textContent = `−${diff.removed}`;
    stat.hidden = false;
  }
  const full = document.querySelector("[data-sealed-full]");
  if (full) full.hidden = false;
}

// A version copied into a new resource: decrypted with this resource's key,
// encrypted with a new one under the new resource's id.
async function copyVersion(key, holder, side, button) {
  button.disabled = true;
  try {
    const newID = crypto.randomUUID();
    const contentKey = await newContentKey();
    const label = (await decryptMeta(await openContentKey(fromBase64URL(page.dataset.sealedKey), key, id), id, fromBase64URL(page.dataset.sealedMeta))).name || side.meta.filename;
    const form = new FormData();
    form.append("csrf", csrf);
    form.append("id", newID);
    form.append("sealed_key", toBase64URL(await wrapContentKey(contentKey, key, newID)));
    form.append("meta", toBase64URL(await encryptMeta(contentKey, newID, {
      name: `${label} (v${holder.dataset.number})`, filename: side.meta.filename, type: side.meta.type,
    })));
    form.append("content", new Blob([await encryptContent(contentKey, newID, side.content)]), "blob");
    const response = await fetch("/resources/sealed", { method: "POST", body: form, credentials: "same-origin", headers: { Accept: "application/json" } });
    const answer = await response.json().catch(() => ({}));
    if (!response.ok) throw new Error(answer.message || msg("msgFailed"));
    location.assign(answer.location.replace("?created=1", ""));
  } catch {
    button.disabled = false;
  }
}

async function run(key) {
  const contentKey = await openContentKey(fromBase64URL(page.dataset.sealedKey), key, id);
  nameTitles(await decryptMeta(contentKey, id, fromBase64URL(page.dataset.sealedMeta)));
  const version = document.querySelector("[data-sealed-version]");
  if (version) {
    const side = await openSide(contentKey, version.dataset.raw, version.dataset.meta);
    showVersion(version, side);
    const copy = document.querySelector("[data-sealed-copy-version]");
    if (copy) {
      copy.disabled = false;
      copy.addEventListener("click", () => copyVersion(key, version, side, copy));
    }
  }
  const comparison = document.querySelector("[data-sealed-compare]");
  if (comparison) {
    const [before, after] = await Promise.all([
      openSide(contentKey, comparison.dataset.fromRaw, comparison.dataset.fromMeta),
      openSide(contentKey, comparison.dataset.toRaw, comparison.dataset.toMeta),
    ]);
    await showDiff(comparison, before, after);
  }
}

async function start() {
  page.dataset.placeholder = document.querySelector("[data-sealed-title]")?.textContent || "";
  const holder = document.querySelector("[data-sealed-version], [data-sealed-compare]");
  const failed = () => {
    const status = holder?.querySelector("[data-sealed-status]");
    if (status) status.textContent = msg("msgFailed");
  };
  let ran = false;
  const open = (key) => {
    if (ran) return;
    ran = true;
    run(key).catch(failed);
  };
  document.addEventListener("plainmote:locked", () => {
    if (ran) location.reload();
  });
  const key = await accountKey();
  if (key) return open(key);
  const form = holder?.querySelector("[data-sealed-unlock]");
  // Unlocked here or from the top bar.
  document.addEventListener("plainmote:unlocked", (event) => {
    if (form) form.hidden = true;
    open(event.detail.key);
  });
  if (!form) return;
  form.hidden = false;
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const problem = form.querySelector("[data-sealed-error]");
    problem.textContent = "";
    try {
      await unlock(form.elements.password.value);
    } catch (error) {
      problem.textContent = error instanceof WrongPassword ? msg("msgWrong") : error instanceof NoStorage ? msg("msgNoStorage") : msg("msgFailed");
    }
  });
}

if (page && globalThis.crypto?.subtle) start();

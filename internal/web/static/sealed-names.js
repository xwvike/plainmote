// Encrypted resources by name, once unlocked: in the resource list and the
// access history, and in searches - the service cannot match what it cannot
// read, so the list's own search leaves them out and they are matched here,
// in the browser, and shown in a block of their own.
import { openContentKey, decryptMeta } from "./seal.js";
import { fromBase64URL } from "./e2ee.js";
import { accountKey, unlock, WrongPassword, NoStorage } from "./unlock.js";

const search = document.querySelector("[data-sealed-search]");
const query = (search?.dataset.query || "").trim().toLowerCase();
const msg = (name) => search?.dataset[name] || "";
const named = document.querySelectorAll("[data-sealed-name], [data-sealed-file]");

async function readIndex() {
  const response = await fetch("/resources/sealed", { headers: { Accept: "application/json" }, credentials: "same-origin" });
  if (!response.ok) return [];
  return (await response.json()).resources || [];
}

async function openNames(key, entries) {
  const names = new Map();
  await Promise.all(entries.map(async (entry) => {
    try {
      const contentKey = await openContentKey(fromBase64URL(entry.sealed_key), key, entry.id);
      names.set(entry.id, await decryptMeta(contentKey, entry.id, fromBase64URL(entry.sealed_meta)));
    } catch {
      // One that will not open stays as it was shown.
    }
  }));
  return names;
}

function fill(names) {
  for (const element of document.querySelectorAll("[data-sealed-name]")) {
    const meta = names.get(element.dataset.sealedName);
    if (meta && (meta.name || meta.filename)) {
      element.textContent = meta.name || meta.filename;
      element.closest("[title]")?.setAttribute("title", element.textContent);
    }
  }
  for (const element of document.querySelectorAll("[data-sealed-file]")) {
    const meta = names.get(element.dataset.sealedFile);
    if (meta?.filename) {
      element.textContent = meta.filename;
      element.title = meta.filename;
    }
  }
}

function showMatches(names) {
  const matches = [...names.entries()].filter(([, meta]) =>
    meta.name.toLowerCase().includes(query) || meta.filename.toLowerCase().includes(query));
  if (matches.length === 0) return;
  const body = search.querySelector("[data-sealed-matches-body]");
  for (const [id, meta] of matches) {
    const row = document.createElement("tr");
    const nameCell = document.createElement("td");
    nameCell.className = "trunc";
    const link = document.createElement("a");
    link.href = `/resources/${id}`;
    link.textContent = meta.name || meta.filename || id;
    nameCell.append(link);
    const fileCell = document.createElement("td");
    const line = document.createElement("span");
    line.className = "fl";
    const file = document.createElement("span");
    file.className = "mono trunc";
    file.textContent = meta.filename || "—";
    const tag = document.createElement("span");
    tag.className = "tag enc";
    tag.textContent = msg("msgTag");
    line.append(file, tag);
    fileCell.append(line);
    row.append(nameCell, fileCell);
    body.append(row);
  }
  search.querySelector("[data-sealed-matches-count]").textContent = msg("msgMatches").replace("%d", matches.length);
  search.querySelector("[data-sealed-matches]").hidden = false;
  search.hidden = false;
}

let shown = false;

async function show(key, entries) {
  shown = true;
  const names = await openNames(key, entries);
  fill(names);
  if (search && query) showMatches(names);
}

async function start() {
  if (named.length === 0 && !query) return;
  const entries = await readIndex();
  if (entries.length === 0) return;
  // Locked from the top bar: the names go with the page. Unlocked there:
  // they are filled in where they are.
  document.addEventListener("plainmote:locked", () => {
    if (shown) location.reload();
  });
  const key = await accountKey();
  if (key) return show(key, entries);
  const form = search?.querySelector("[data-sealed-unlock]");
  document.addEventListener("plainmote:unlocked", (event) => {
    if (form) form.hidden = true;
    if (!shown) show(event.detail.key, entries);
  });
  // Locked while searching: say what was left out, and offer to unlock.
  if (!search || !query) return;
  search.querySelector("[data-sealed-search-note]").textContent = msg("msgLocked").replace("%d", entries.length);
  form.hidden = false;
  search.hidden = false;
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const problem = form.querySelector("[data-sealed-error]");
    problem.textContent = "";
    try {
      // What fills the names is the listener above, as from the top bar.
      await unlock(form.elements.password.value);
    } catch (error) {
      problem.textContent = error instanceof WrongPassword ? msg("msgWrong") : error instanceof NoStorage ? msg("msgNoStorage") : msg("msgFailed");
    }
  });
}

if (globalThis.crypto?.subtle) start();

// Finder-style folder browser interactions:
// - Single-click selects an item (with Finder highlight); double-click opens
//   files and enters folders (touch devices keep single-tap open).
// - Right-click anywhere on an item opens the context menu at cursor.
// - Smooth GPU-accelerated pointer-events drag-and-drop with a crisp floating
//   drag pill, live folder/breadcrumb drop targets, and destination feedback.
// - Seamless infinite scroll when a directory holds more than one batch.

const menu = document.getElementById("fb-menu");
const dlgNew = document.getElementById("dlg-folder-new");
const dlgRename = document.getElementById("dlg-folder-rename");
const dlgMove = document.getElementById("dlg-move");
const dlgDelete = document.getElementById("dlg-folder-delete");
const dlgResDelete = document.getElementById("dlg-resource-delete");
const dlgInfo = document.getElementById("dlg-info");
const dlgTags = document.getElementById("dlg-tags");
const dlgSheet = document.getElementById("dlg-sheet");
const sheetFrame = document.getElementById("fb-sheet-frame");
const errBanner = document.getElementById("fb-error");

const isCoarsePointer = () => window.matchMedia?.("(pointer: coarse)").matches;

let activeMenuItem = null;
let selectedEl = null;
let suppressNextClick = false;
let dragState = null;
let loadMoreObserver = null;
let loadingMore = false;
let sheetDirty = false;
let sheetResourceId = "";
let activeTagsItem = null;
let tagsDirty = false;

function resultsRoot() {
  return document.getElementById("resource-results");
}

function currentFolder() {
  return resultsRoot()?.dataset.currentFolder || "";
}

function csrfToken() {
  return resultsRoot()?.dataset.csrf || document.querySelector('meta[name="plainmote-csrf"]')?.content || "";
}

function showError(msg) {
  if (!errBanner) return;
  errBanner.textContent = msg || "";
  errBanner.hidden = !msg;
}

function itemInfo(el) {
  if (!el) return null;
  const sealedLabel = el.querySelector("[data-sealed-name]")?.textContent?.trim();
  return {
    el,
    kind: el.dataset.itemKind,
    id: el.dataset.itemId,
    name: sealedLabel || el.dataset.itemName || "",
    parent: el.dataset.itemParent || "",
    href: el.dataset.itemHref || el.querySelector("a")?.href || "",
    sealed: el.dataset.itemSealed === "1",
    quick: el.dataset.itemQuick === "1",
    shares: Number(el.dataset.itemShares || "0"),
  };
}

function updateStatusBar() {
  const stat = document.querySelector("[data-fb-stat]");
  if (!stat) return;
  const base = stat.dataset.baseLabel || "";
  const selMsg = resultsRoot()?.dataset.msgSelected || "1 selected";
  stat.textContent = selectedEl ? `${base} · ${selMsg}` : base;
}

function setSelected(el) {
  if (selectedEl === el) return;
  selectedEl?.classList.remove("is-selected");
  selectedEl = el || null;
  selectedEl?.classList.add("is-selected");
  updateStatusBar();
}

function syncReturnInputs() {
  const folder = currentFolder();
  for (const input of document.querySelectorAll("[data-fb-parent-input], [data-fb-return-input]")) {
    input.value = folder;
  }
}

function openSheet(url, variant, resourceId = "") {
  if (!dlgSheet || !sheetFrame) {
    location.assign(url);
    return;
  }
  closeMenu();
  sheetDirty = false;
  sheetResourceId = resourceId || "";
  dlgSheet.classList.remove("is-editor", "is-panel", "is-wide");
  dlgSheet.classList.add(variant);
  sheetFrame.src = url;
  if (!dlgSheet.open) dlgSheet.showModal();
}

function selectById(id) {
  if (!id) return;
  const target = document.querySelector(`[data-fb-item][data-item-id="${CSS.escape(id)}"]`);
  if (target) {
    setSelected(target);
    target.scrollIntoView({ block: "nearest" });
  }
}

function tryCloseSheet(force = false) {
  if (!dlgSheet?.open) return;
  if (!force) {
    try {
      const hasUnsaved = Boolean(sheetFrame?.contentDocument?.querySelector("[data-unsaved].show"));
      if (hasUnsaved && !window.confirm(dlgSheet.dataset.msgUnsaved || "Discard unsaved changes?")) {
        return;
      }
    } catch {
      // Ignore if iframe is still navigating.
    }
  }
  const shouldRefresh = sheetDirty;
  const reselectId = sheetResourceId;
  sheetDirty = false;
  sheetResourceId = "";
  dlgSheet.close();
  sheetFrame.src = "about:blank";
  if (shouldRefresh) {
    navigateFolder(location.href, false, reselectId);
  } else if (reselectId) {
    selectById(reselectId);
  }
}

function openItem(info) {
  if (!info?.href) return;
  if (info.kind === "folder") {
    navigateFolder(info.href);
  } else {
    openSheet(`/resources/${encodeURIComponent(info.id)}?modal=edit`, "is-editor", info.id);
  }
}

async function navigateFolder(href, push = true, selectId = "") {
  const results = resultsRoot();
  if (!results) {
    location.assign(href);
    return;
  }
  closeMenu();
  setSelected(null);
  results.setAttribute("aria-busy", "true");
  try {
    const response = await fetch(href, {
      headers: { "X-PlainMote-Fragment": "1" },
      credentials: "same-origin",
    });
    if (!response.ok) {
      location.assign(href);
      return;
    }
    const doc = new DOMParser().parseFromString(await response.text(), "text/html");
    const nextResults = doc.getElementById("resource-results");
    const nextFilters = doc.getElementById("resource-filters");
    if (!nextResults) {
      location.assign(href);
      return;
    }
    results.replaceWith(nextResults);
    const filters = document.getElementById("resource-filters");
    if (filters && nextFilters) {
      const nextFolder = nextResults.dataset.currentFolder || "";
      let folderInput = filters.querySelector('input[name="folder"]');
      if (nextFolder) {
        if (!folderInput) {
          folderInput = document.createElement("input");
          folderInput.type = "hidden";
          folderInput.name = "folder";
          filters.append(folderInput);
        }
        folderInput.value = nextFolder;
      } else {
        folderInput?.remove();
      }
    }
    const newBtn = document.getElementById("new-resource-link");
    if (newBtn) {
      const folder = nextResults.dataset.currentFolder || "";
      newBtn.href = folder ? `/resources/new?folder=${encodeURIComponent(folder)}` : "/resources/new";
    }
    if (push && response.url !== location.href) {
      history.pushState(null, "", response.url);
    }
    syncReturnInputs();
    observeLoadMore();
    document.dispatchEvent(new Event("plainmote:resource-results"));
    if (selectId) selectById(selectId);
  } catch {
    location.assign(href);
  }
}

function closeMenu() {
  if (!menu) return;
  menu.hidden = true;
  activeMenuItem = null;
}

function openMenuAt(info, x, y) {
  if (!menu || !info) return;
  activeMenuItem = info;

  const isFolder = info.kind === "folder";
  const setHidden = (act, hide) => {
    const btn = menu.querySelector(`[data-fb-act="${act}"]`);
    if (btn) btn.hidden = hide;
  };
  setHidden("share", isFolder);
  setHidden("logs", isFolder);
  setHidden("tags", isFolder);
  setHidden("versions", isFolder || info.quick);
  setHidden("rename", !isFolder && (info.sealed || info.quick));
  setHidden("delete", false);

  menu.hidden = false;
  const mw = menu.offsetWidth || 164;
  const mh = menu.offsetHeight || 180;
  const left = Math.max(8, Math.min(x, window.innerWidth - mw - 8));
  const top = y + mh + 8 > window.innerHeight ? Math.max(8, y - mh - 4) : Math.max(8, y);
  menu.style.left = `${left}px`;
  menu.style.top = `${top}px`;
}

function openNewFolder() {
  if (!dlgNew) return;
  syncReturnInputs();
  const input = dlgNew.querySelector('input[name="name"]');
  if (input) input.value = "";
  dlgNew.showModal();
  input?.focus();
}

function openRename(item) {
  if (!dlgRename) return;
  syncReturnInputs();
  const form = dlgRename.querySelector("form");
  const isFolder = item.kind === "folder";
  form.action = isFolder ? "/resources/folders" : "/resources/rename";
  form.querySelector('input[name="folder_id"]').value = isFolder ? item.id : "";
  form.querySelector('input[name="resource_id"]').value = isFolder ? "" : item.id;
  dlgRename.querySelector("[data-rename-title]").textContent = isFolder ? dlgRename.dataset.titleFolder : dlgRename.dataset.titleFile;
  dlgRename.querySelector("[data-rename-label]").textContent = isFolder ? dlgRename.dataset.labelFolder : dlgRename.dataset.labelFile;
  const input = form.querySelector('input[name="name"], input[name="filename"]');
  if (input) {
    input.name = isFolder ? "name" : "filename";
    input.value = item.el?.dataset.infoFilename || item.name;
  }
  dlgRename.showModal();
  input?.focus();
  input?.select();
}

function openDeleteResource(item) {
  if (!dlgResDelete) return;
  syncReturnInputs();
  const form = dlgResDelete.querySelector("form");
  form.action = `/resources/${encodeURIComponent(item.id)}`;
  const warn = dlgResDelete.querySelector("[data-delete-file-warn]");
  if (warn) {
    warn.textContent = item.shares > 0 ? dlgResDelete.dataset.warnOne : dlgResDelete.dataset.warnNone;
  }
  dlgResDelete.showModal();
}

let activeTagsList = [];

function renderTagPills(tags) {
  activeTagsList = tags;
  const list = dlgTags?.querySelector("[data-tags-list]");
  const emptyEl = dlgTags?.querySelector("[data-tags-empty]");
  if (list) {
    list.replaceChildren();
    for (const tag of tags) {
      const chip = document.createElement("span");
      chip.className = "lbl";
      const text = document.createElement("span");
      text.textContent = tag;
      const remove = document.createElement("button");
      remove.type = "button";
      remove.textContent = "×";
      remove.dataset.tagRemove = tag;
      remove.setAttribute("aria-label", tag);
      chip.append(text, remove);
      list.append(chip);
    }
  }
  if (emptyEl) emptyEl.hidden = tags.length > 0;
  syncTagSuggestions();
}

function syncTagSuggestions() {
  const wrap = dlgTags?.querySelector("[data-tags-suggest]");
  if (!wrap) return;
  const active = new Set(activeTagsList);
  const query = (dlgTags.querySelector('input[name="add"]')?.value || "").trim().toLowerCase();
  // Ensure any newly created tag also appears in the quick-pick pool.
  for (const tag of activeTagsList) {
    if (!wrap.querySelector(`[data-tag-pick="${CSS.escape(tag)}"]`)) {
      const btn = document.createElement("button");
      btn.type = "button";
      btn.className = "fb-tag-pick";
      btn.dataset.tagPick = tag;
      btn.textContent = tag;
      wrap.append(btn);
    }
  }
  let visibleCount = 0;
  for (const btn of wrap.querySelectorAll("[data-tag-pick]")) {
    const name = btn.dataset.tagPick || "";
    const isOn = active.has(name);
    btn.classList.toggle("on", isOn);
    btn.setAttribute("aria-pressed", isOn ? "true" : "false");
    const match = !query || name.toLowerCase().includes(query);
    btn.hidden = !match;
    if (match) visibleCount += 1;
  }
  wrap.hidden = visibleCount === 0;
}

function openTags(item) {
  if (!dlgTags || !item) return;
  activeTagsItem = item;
  tagsDirty = false;
  const el = item.el || document.querySelector(`[data-fb-item][data-item-id="${CSS.escape(item.id)}"]`);
  const fileEl = dlgTags.querySelector("[data-tags-file]");
  if (fileEl) fileEl.textContent = item.name ? `· ${item.name}` : "";
  const input = dlgTags.querySelector('input[name="add"]');
  if (input) input.value = "";
  const tags = (el?.dataset.infoTags || "").split("\n").map((t) => t.trim()).filter(Boolean);
  renderTagPills(tags);
  const errEl = dlgTags.querySelector("[data-tags-error]");
  if (errEl) { errEl.textContent = ""; errEl.hidden = true; }
  dlgTags.showModal();
  input?.focus();
}

async function mutateTag(params) {
  if (!activeTagsItem || !dlgTags) return;
  const errEl = dlgTags.querySelector("[data-tags-error]");
  if (errEl) { errEl.textContent = ""; errEl.hidden = true; }
  const body = new URLSearchParams(params);
  body.set("csrf", csrfToken());
  try {
    const response = await fetch(`/resources/${encodeURIComponent(activeTagsItem.id)}/tags`, {
      method: "POST",
      credentials: "same-origin",
      headers: { Accept: "application/json", "Content-Type": "application/x-www-form-urlencoded" },
      body,
    });
    const data = await response.json().catch(() => ({}));
    if (!response.ok) {
      if (errEl) { errEl.textContent = data.message || "Error"; errEl.hidden = false; }
      return;
    }
    const tags = Array.isArray(data.tags) ? data.tags : [];
    renderTagPills(tags);
    const el = document.querySelector(`[data-fb-item][data-item-id="${CSS.escape(activeTagsItem.id)}"]`);
    if (el) el.dataset.infoTags = tags.join("\n");
    tagsDirty = true;
    const input = dlgTags.querySelector('input[name="add"]');
    if (input && params.add) { input.value = ""; input.focus(); }
  } catch {
    if (errEl) { errEl.textContent = "Network error"; errEl.hidden = false; }
  }
}

function openMove(item) {
  if (!dlgMove) return;
  syncReturnInputs();
  const form = dlgMove.querySelector("form");
  const select = dlgMove.querySelector("#fb-move-select");
  if (!form || !select) return;

  const isFolder = item.kind === "folder";
  form.action = isFolder ? "/resources/folders" : "/resources/move";
  form.querySelector('input[name="folder_id"]').value = isFolder ? item.id : "";
  form.querySelector('input[name="resource_id"]').value = isFolder ? "" : item.id;

  const forbidden = new Set();
  if (isFolder) {
    forbidden.add(item.id);
    const parentById = new Map();
    for (const opt of select.options) {
      if (opt.value) parentById.set(opt.value, opt.dataset.parent || "");
    }
    let changed = true;
    while (changed) {
      changed = false;
      for (const [id, parent] of parentById) {
        if (!forbidden.has(id) && forbidden.has(parent)) {
          forbidden.add(id);
          changed = true;
        }
      }
    }
  }
  for (const opt of select.options) {
    opt.disabled = forbidden.has(opt.value);
  }
  select.value = forbidden.has(item.parent) ? "" : item.parent;
  dlgMove.showModal();
  select.focus();
}

function openDeleteFolder(item) {
  if (!dlgDelete) return;
  syncReturnInputs();
  dlgDelete.querySelector('input[name="folder_id"]').value = item.id;
  dlgDelete.showModal();
}

function formatLocalTimestamp(iso) {
  if (!iso) return "—";
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  const pad = (n) => String(n).padStart(2, "0");
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function currentBreadcrumbText() {
  const parts = [...document.querySelectorAll(".fb-crumb .fb-crumb-item")]
    .map((el) => el.textContent.trim())
    .filter(Boolean);
  return parts.join(" / ") || "/";
}

function openInfo(item) {
  if (!dlgInfo || !item) return;
  const el = item.el || document.querySelector(`[data-fb-item][data-item-id="${CSS.escape(item.id)}"]`);
  if (!el) return;

  const isFolder = item.kind === "folder";
  const ds = el.dataset;
  const kindText = el.querySelector("[data-sealed-kind]")?.textContent?.trim() || ds.infoKind || "";
  const iconSlot = dlgInfo.querySelector("[data-info-icon]");
  iconSlot.replaceChildren();
  const tileIcon = el.querySelector(".fb-ico");
  if (tileIcon) {
    iconSlot.append(tileIcon.cloneNode(true));
  } else {
    // List view row: synthesize the full Finder icon so Get Info always looks rich.
    const wrap = document.createElement("span");
    if (isFolder) {
      wrap.className = "fb-ico fb-ico-folder";
      wrap.innerHTML = `<svg viewBox="0 0 64 52" fill="none"><path class="fld-back" d="M4 11a5 5 0 0 1 5-5h14.2a5 5 0 0 1 3.7 1.6l2.4 2.7a3 3 0 0 0 2.2 1H55a5 5 0 0 1 5 5v26a5 5 0 0 1-5 5H9a5 5 0 0 1-5-5V11Z"/><path class="fld-front" d="M4 18.5a4.5 4.5 0 0 1 4.5-4.5h47a4.5 4.5 0 0 1 4.5 4.5V42a5 5 0 0 1-5 5H9a5 5 0 0 1-5-5V18.5Z"/></svg>`;
    } else {
      wrap.className = `fb-ico fb-ico-file cat-${ds.infoCat || "file"}`;
      wrap.innerHTML = `<svg viewBox="0 0 48 56" fill="none"><path class="doc-sheet" d="M9 3.5h20.5L41.5 15.5V49a3.5 3.5 0 0 1-3.5 3.5H9A3.5 3.5 0 0 1 5.5 49V7A3.5 3.5 0 0 1 9 3.5Z"/><path class="doc-fold" d="M29.5 3.5V12a3.5 3.5 0 0 0 3.5 3.5h8.5L29.5 3.5Z"/><line class="doc-line" x1="12" y1="22" x2="29" y2="22"/><line class="doc-line" x1="12" y1="27.5" x2="34" y2="27.5"/><line class="doc-line" x1="12" y1="33" x2="24" y2="33"/></svg>`;
      if (ds.infoBadge) {
        const badge = document.createElement("span");
        badge.className = "fb-ext";
        badge.textContent = ds.infoBadge;
        wrap.append(badge);
      }
    }
    iconSlot.append(wrap);
  }

  dlgInfo.querySelector("[data-info-title]").textContent = item.name;
  dlgInfo.querySelector("[data-info-sub]").textContent = isFolder
    ? `${kindText} · ${ds.infoContains || ""}`.replace(/ · $/, "")
    : `${kindText} · ${ds.infoSize || ""}`.replace(/ · $/, "");

  const setRow = (key, value) => {
    const row = dlgInfo.querySelector(`[data-info-row="${key}"]`);
    const val = dlgInfo.querySelector(`[data-info-val="${key}"]`);
    if (!row || !val) return;
    if (!value) {
      row.hidden = true;
      val.textContent = "";
    } else {
      row.hidden = false;
      val.textContent = value;
    }
  };

  setRow("kind", kindText);
  setRow("filename", !isFolder && ds.infoFilename && ds.infoFilename !== item.name ? ds.infoFilename : "");
  const bytesNum = Number(ds.infoBytes);
  const sizeDetail = !isFolder && ds.infoSize
    ? (Number.isFinite(bytesNum) && bytesNum >= 1024 ? `${ds.infoSize} (${bytesNum.toLocaleString()} B)` : ds.infoSize)
    : "";
  setRow("size", sizeDetail);
  setRow("contains", isFolder ? ds.infoContains : "");
  setRow("where", currentBreadcrumbText());
  setRow("status", !isFolder ? ds.infoStatus : "");
  setRow("encryption", !isFolder ? ds.infoEnc : "");
  setRow("version", !isFolder ? ds.infoVersion : "");
  setRow("created", formatLocalTimestamp(ds.infoCreated));
  setRow("updated", formatLocalTimestamp(ds.infoUpdated));

  const tagsRow = dlgInfo.querySelector('[data-info-row="tags"]');
  const tagsVal = dlgInfo.querySelector('[data-info-val="tags"]');
  const tags = (ds.infoTags || "").split("\n").map((t) => t.trim()).filter(Boolean);
  if (tagsRow && tagsVal) {
    tagsVal.replaceChildren();
    if (tags.length > 0) {
      for (const tag of tags) {
        const a = document.createElement("a");
        a.className = "lbl sm";
        a.href = `/resources/?tag=${encodeURIComponent(tag)}`;
        a.textContent = tag;
        tagsVal.append(a);
      }
      tagsRow.hidden = false;
    } else {
      tagsRow.hidden = true;
    }
  }

  const openLink = dlgInfo.querySelector("[data-info-open]");
  if (openLink) {
    openLink.href = item.href || "#";
    openLink.dataset.infoKind = item.kind;
  }
  dlgInfo.showModal();
}

// --- Smooth Pointer-Events Drag-and-Drop ------------------------------------

function buildDragGhost(sourceEl, info, startX, startY) {
  const ghost = document.createElement("div");
  const isTile = sourceEl.classList.contains("fb-tile");
  ghost.className = isTile ? "fb-drag-ghost is-tile" : "fb-drag-ghost is-row";

  let offsetX = 16;
  let offsetY = 16;

  if (isTile) {
    // Lift the exact full-size Finder icon (with extension badge & lock) + name,
    // anchored at the exact pointer grab point so the icon feels physically lifted.
    const link = sourceEl.querySelector(".fb-tile-link");
    const ico = sourceEl.querySelector(".fb-ico");
    const name = sourceEl.querySelector(".fb-name");
    if (link) {
      const rect = link.getBoundingClientRect();
      ghost.style.width = `${rect.width}px`;
      offsetX = startX - rect.left;
      offsetY = startY - rect.top;
    }
    if (ico) ghost.append(ico.cloneNode(true));
    if (name) {
      const nameClone = name.cloneNode(true);
      ghost.append(nameClone);
    }
  } else {
    const rowName = sourceEl.querySelector(".fb-row-name");
    if (rowName) {
      const rect = rowName.getBoundingClientRect();
      offsetX = Math.max(12, Math.min(startX - rect.left, 160));
      offsetY = Math.max(8, Math.min(startY - rect.top, 24));
      ghost.append(rowName.cloneNode(true));
    }
  }

  const dest = document.createElement("span");
  dest.className = "fb-drag-dest";
  dest.hidden = true;
  ghost.append(dest);

  document.body.append(ghost);
  return { ghost, dest, offsetX, offsetY };
}

function findDropTarget(x, y, info) {
  const elements = document.elementsFromPoint(x, y);
  const cur = currentFolder();
  for (const el of elements) {
    const target = el.closest?.("[data-drop-folder]");
    if (!target) continue;
    const targetId = target.dataset.dropFolder;
    if (info.kind === "folder" && info.id === targetId) continue;
    // Dropping onto the active current-folder breadcrumb is a no-op.
    if (target.classList.contains("fb-crumb-item") && targetId === cur) continue;
    return target;
  }
  return null;
}

function clearDropTarget() {
  if (dragState?.target) {
    dragState.target.classList.remove("is-drop-target");
    dragState.target = null;
  }
}

async function commitMove(info, targetFolderID, sourceEl) {
  showError("");
  sourceEl?.classList.add("is-moving-out");
  const body = new URLSearchParams();
  body.set("csrf", csrfToken());
  body.set("target_folder_id", targetFolderID);
  body.set("return_folder", currentFolder());
  let endpoint;
  if (info.kind === "folder") {
    endpoint = "/resources/folders";
    body.set("action", "move");
    body.set("folder_id", info.id);
  } else {
    endpoint = "/resources/move";
    body.set("resource_id", info.id);
  }
  try {
    const response = await fetch(endpoint, {
      method: "POST",
      credentials: "same-origin",
      headers: {
        Accept: "application/json",
        "Content-Type": "application/x-www-form-urlencoded",
      },
      body,
    });
    const data = await response.json().catch(() => ({}));
    if (!response.ok) {
      sourceEl?.classList.remove("is-moving-out");
      showError(data.message || "Move failed");
      return;
    }
    await navigateFolder(location.href, false);
  } catch {
    sourceEl?.classList.remove("is-moving-out");
    showError("Network error");
  }
}

document.addEventListener("pointerdown", (event) => {
  if (event.button !== 0 || event.pointerType === "touch") return;
  if (event.target.closest("[data-item-menu], button")) return;
  const itemEl = event.target.closest("[data-fb-item]");
  if (!itemEl) return;
  const info = itemInfo(itemEl);
  if (!info) return;
  dragState = {
    pointerId: event.pointerId,
    startX: event.clientX,
    startY: event.clientY,
    active: false,
    sourceEl: itemEl,
    info,
    ghost: null,
    dest: null,
    target: null,
  };
});

document.addEventListener("pointermove", (event) => {
  if (!dragState || event.pointerId !== dragState.pointerId) return;
  const dx = event.clientX - dragState.startX;
  const dy = event.clientY - dragState.startY;
  if (!dragState.active) {
    if (Math.hypot(dx, dy) < 6) return;
    dragState.active = true;
    closeMenu();
    setSelected(dragState.sourceEl);
    document.body.classList.add("fb-dragging");
    dragState.sourceEl.classList.add("is-dragging");
    const built = buildDragGhost(dragState.sourceEl, dragState.info, dragState.startX, dragState.startY);
    dragState.ghost = built.ghost;
    dragState.dest = built.dest;
    dragState.offsetX = built.offsetX;
    dragState.offsetY = built.offsetY;
  }

  dragState.ghost.style.transform = `translate3d(${event.clientX - dragState.offsetX}px, ${event.clientY - dragState.offsetY}px, 0)`;

  const nextTarget = findDropTarget(event.clientX, event.clientY, dragState.info);
  if (nextTarget !== dragState.target) {
    clearDropTarget();
    dragState.target = nextTarget;
    if (nextTarget) {
      nextTarget.classList.add("is-drop-target");
      const label = nextTarget.dataset.dropLabel || "";
      const tpl = resultsRoot()?.dataset.msgDropInto || 'Move to "%s"';
      dragState.dest.textContent = tpl.replace("%s", label);
      dragState.dest.hidden = !label;
      dragState.ghost.classList.add("has-target");
    } else {
      dragState.dest.hidden = true;
      dragState.ghost.classList.remove("has-target");
    }
  }
});

function finishDrag(cancelled) {
  if (!dragState) return;
  const state = dragState;
  dragState = null;
  if (!state.active) return;

  suppressNextClick = true;
  setTimeout(() => { suppressNextClick = false; }, 60);

  document.body.classList.remove("fb-dragging");
  state.sourceEl.classList.remove("is-dragging");
  const target = !cancelled ? state.target : null;
  clearDropTarget();

  if (target && state.ghost) {
    const rect = target.getBoundingClientRect();
    const gw = state.ghost.offsetWidth || 80;
    const gh = state.ghost.offsetHeight || 80;
    state.ghost.classList.add("is-dropping");
    state.ghost.style.transform = `translate3d(${rect.left + (rect.width - gw) / 2}px, ${rect.top + (rect.height - gh) / 2}px, 0) scale(0.45)`;
    state.ghost.style.opacity = "0";
    setTimeout(() => state.ghost?.remove(), 170);
    commitMove(state.info, target.dataset.dropFolder, state.sourceEl);
  } else if (state.ghost) {
    state.ghost.classList.add("is-dropping");
    state.ghost.style.opacity = "0";
    setTimeout(() => state.ghost?.remove(), 120);
  }
}

document.addEventListener("pointerup", (event) => {
  if (dragState && event.pointerId === dragState.pointerId) finishDrag(false);
});
document.addEventListener("pointercancel", (event) => {
  if (dragState && event.pointerId === dragState.pointerId) finishDrag(true);
});

// --- Seamless Infinite Scroll (no traditional numbered pagination) ----------

async function loadNextBatch(wrap) {
  if (loadingMore || !wrap) return;
  const nextURL = wrap.dataset.loadMore;
  if (!nextURL) return;
  loadingMore = true;
  wrap.setAttribute("aria-busy", "true");
  try {
    const response = await fetch(nextURL, {
      headers: { "X-PlainMote-Fragment": "1" },
      credentials: "same-origin",
    });
    if (!response.ok) return;
    const doc = new DOMParser().parseFromString(await response.text(), "text/html");
    const container = document.querySelector("[data-fb-items]");
    const nextContainer = doc.querySelector("[data-fb-items]");
    if (container && nextContainer) {
      // Only append file items (folders were already rendered on batch 1).
      for (const el of nextContainer.querySelectorAll('[data-fb-item][data-item-kind="resource"]')) {
        container.append(el);
      }
    }
    const nextWrap = doc.querySelector("[data-load-more]");
    if (nextWrap) {
      wrap.replaceWith(nextWrap);
    } else {
      wrap.remove();
    }
    observeLoadMore();
    document.dispatchEvent(new Event("plainmote:resource-results"));
  } finally {
    loadingMore = false;
  }
}

function observeLoadMore() {
  loadMoreObserver?.disconnect();
  const wrap = document.querySelector("[data-load-more]");
  if (!wrap || !("IntersectionObserver" in window)) return;
  const scrollRoot = document.querySelector("[data-fb-scroll]") || null;
  loadMoreObserver = new IntersectionObserver((entries) => {
    if (entries.some((e) => e.isIntersecting)) loadNextBatch(wrap);
  }, { root: scrollRoot, rootMargin: "200px" });
  loadMoreObserver.observe(wrap);
}

// --- Click, Double-Click, Context Menu & Keyboard Navigation ----------------

document.addEventListener("click", (event) => {
  if (suppressNextClick) {
    event.preventDefault();
    event.stopPropagation();
    return;
  }

  const closeBtn = event.target.closest("[data-dlg-close]");
  if (closeBtn) {
    closeBtn.closest("dialog")?.close();
    return;
  }

  const newBtn = event.target.closest("[data-folder-new-open]");
  if (newBtn) {
    closeMenu();
    openNewFolder();
    return;
  }

  const loadMoreBtn = event.target.closest("[data-load-more-btn]");
  if (loadMoreBtn && event.button === 0 && !event.metaKey && !event.ctrlKey) {
    event.preventDefault();
    loadNextBatch(loadMoreBtn.closest("[data-load-more]"));
    return;
  }

  const viewLink = event.target.closest("[data-view-link]");
  if (viewLink && event.button === 0 && !event.metaKey && !event.ctrlKey && !event.shiftKey && !event.altKey) {
    event.preventDefault();
    navigateFolder(viewLink.href, false);
    return;
  }

  const crumbLink = event.target.closest("a.fb-crumb-item");
  if (crumbLink && event.button === 0 && !event.metaKey && !event.ctrlKey && !event.shiftKey && !event.altKey) {
    event.preventDefault();
    navigateFolder(crumbLink.href, true);
    return;
  }

  const moreBtn = event.target.closest("[data-item-menu]");
  if (moreBtn) {
    event.preventDefault();
    event.stopPropagation();
    const itemEl = moreBtn.closest("[data-fb-item]");
    const info = itemInfo(itemEl) || {
      kind: moreBtn.dataset.itemMenu,
      id: moreBtn.dataset.itemId,
      name: moreBtn.dataset.itemName || "",
      parent: moreBtn.dataset.itemParent || "",
      href: moreBtn.dataset.itemHref || "",
    };
    setSelected(itemEl);
    if (!menu.hidden && activeMenuItem?.id === info.id) {
      closeMenu();
    } else {
      const rect = moreBtn.getBoundingClientRect();
      openMenuAt(info, rect.right - 136, rect.bottom + 4);
    }
    return;
  }

  const newResLink = event.target.closest("#new-resource-link");
  if (newResLink && event.button === 0 && !event.metaKey && !event.ctrlKey && !event.shiftKey && !event.altKey) {
    event.preventDefault();
    const u = new URL(newResLink.href, location.href);
    u.searchParams.set("modal", "edit");
    openSheet(u.pathname + u.search, "is-editor");
    return;
  }

  const tagRemoveBtn = event.target.closest("[data-tag-remove]");
  if (tagRemoveBtn && dlgTags?.open) {
    event.preventDefault();
    mutateTag({ remove: tagRemoveBtn.dataset.tagRemove });
    return;
  }

  const tagPickBtn = event.target.closest("[data-tag-pick]");
  if (tagPickBtn && dlgTags?.open) {
    event.preventDefault();
    const name = tagPickBtn.dataset.tagPick;
    if (name) {
      if (activeTagsList.includes(name)) mutateTag({ remove: name });
      else mutateTag({ add: name });
    }
    return;
  }

  const actBtn = event.target.closest("[data-fb-act]");
  if (actBtn && activeMenuItem) {
    const item = activeMenuItem;
    closeMenu();
    const act = actBtn.dataset.fbAct;
    if (act === "open") openItem(item);
    else if (act === "share") openSheet(`/resources/${encodeURIComponent(item.id)}?modal=share`, "is-panel", item.id);
    else if (act === "logs") openSheet(`/resources/${encodeURIComponent(item.id)}/logs?modal=share`, "is-panel", item.id);
    else if (act === "tags") openTags(item);
    else if (act === "versions") openSheet(`/resources/${encodeURIComponent(item.id)}/versions?modal=versions`, "is-wide", item.id);
    else if (act === "info") openInfo(item);
    else if (act === "rename") openRename(item);
    else if (act === "move") openMove(item);
    else if (act === "delete") {
      if (item.kind === "folder") openDeleteFolder(item);
      else openDeleteResource(item);
    }
    return;
  }

  const infoOpen = event.target.closest("[data-info-open]");
  if (infoOpen && event.button === 0 && !event.metaKey && !event.ctrlKey) {
    event.preventDefault();
    const kind = infoOpen.dataset.infoKind;
    const href = infoOpen.href;
    dlgInfo?.close();
    if (kind === "folder") {
      navigateFolder(href, true);
    } else {
      const u = new URL(href, location.href);
      u.searchParams.set("modal", "edit");
      const idMatch = /^\/resources\/([0-9a-f-]{36})$/i.exec(u.pathname);
      openSheet(u.pathname + u.search, "is-editor", idMatch ? idMatch[1] : "");
    }
    return;
  }

  if (menu && !menu.hidden && !event.target.closest("#fb-menu")) {
    closeMenu();
  }

  // Finder single-click selection on desktop; single-tap open on touch devices.
  const itemEl = event.target.closest("[data-fb-item]");
  if (itemEl) {
    if (event.metaKey || event.ctrlKey || event.shiftKey || event.altKey || event.button !== 0) {
      return;
    }
    if (isCoarsePointer()) {
      const info = itemInfo(itemEl);
      if (info) {
        event.preventDefault();
        openItem(info);
      }
      return;
    }
    event.preventDefault();
    setSelected(itemEl);
    return;
  }

  if (event.target.closest(".fb-panel")) {
    setSelected(null);
  }
});

// Double-click opens files and enters folders.
document.addEventListener("dblclick", (event) => {
  if (event.target.closest("[data-item-menu], button")) return;
  const itemEl = event.target.closest("[data-fb-item]");
  if (!itemEl) return;
  event.preventDefault();
  setSelected(itemEl);
  openItem(itemInfo(itemEl));
});

// Right-click opens Finder context menu at pointer position.
document.addEventListener("contextmenu", (event) => {
  const itemEl = event.target.closest("[data-fb-item]");
  if (!itemEl) return;
  event.preventDefault();
  setSelected(itemEl);
  openMenuAt(itemInfo(itemEl), event.clientX, event.clientY);
});

// Close dialogs when clicking their backdrop.
for (const dlg of [dlgNew, dlgRename, dlgMove, dlgDelete, dlgResDelete, dlgInfo, dlgTags]) {
  dlg?.addEventListener("click", (event) => {
    if (event.target === dlg) dlg.close();
  });
}

dlgSheet?.addEventListener("click", (event) => {
  if (event.target === dlgSheet) tryCloseSheet(false);
});
dlgSheet?.addEventListener("cancel", (event) => {
  event.preventDefault();
  tryCloseSheet(false);
});

dlgTags?.querySelector("[data-tags-add-form]")?.addEventListener("submit", (event) => {
  event.preventDefault();
  const input = event.currentTarget.querySelector('input[name="add"]');
  const tag = input?.value.trim();
  if (tag) mutateTag({ add: tag });
});
dlgTags?.querySelector('input[name="add"]')?.addEventListener("input", syncTagSuggestions);
dlgTags?.addEventListener("close", () => {
  if (tagsDirty) {
    tagsDirty = false;
    location.reload();
  }
});

window.addEventListener("message", (event) => {
  if (event.origin !== location.origin || !event.data || typeof event.data !== "object") return;
  if (event.data.type === "plainmote:modal-saved") {
    sheetDirty = true;
    if (event.data.resourceId) sheetResourceId = event.data.resourceId;
    tryCloseSheet(true);
  } else if (event.data.type === "plainmote:modal-dirty") {
    sheetDirty = true;
    if (event.data.resourceId) sheetResourceId = event.data.resourceId;
  } else if (event.data.type === "plainmote:modal-close") {
    tryCloseSheet(false);
  }
});

document.addEventListener("keydown", (event) => {
  if (event.key === "Escape") {
    closeMenu();
    setSelected(null);
    return;
  }
  const tag = document.activeElement?.tagName;
  if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || document.querySelector("dialog[open]")) return;

  if ((event.key === "i" || event.key === "I") && selectedEl && !event.altKey) {
    event.preventDefault();
    openInfo(itemInfo(selectedEl));
    return;
  }

  if (event.key === "Enter" && selectedEl) {
    event.preventDefault();
    openItem(itemInfo(selectedEl));
    return;
  }
  if (event.key === "Backspace" && currentFolder()) {
    const crumbs = [...document.querySelectorAll("a.fb-crumb-item")];
    if (crumbs.length >= 2) {
      event.preventDefault();
      navigateFolder(crumbs[crumbs.length - 2].href, true);
    }
    return;
  }
  if (event.key === "ArrowRight" || event.key === "ArrowDown" || event.key === "ArrowLeft" || event.key === "ArrowUp") {
    const items = [...document.querySelectorAll("[data-fb-item]")];
    if (items.length === 0) return;
    event.preventDefault();
    const idx = selectedEl ? items.indexOf(selectedEl) : -1;
    const step = (event.key === "ArrowRight" || event.key === "ArrowDown") ? 1 : -1;
    const next = idx < 0 ? 0 : Math.max(0, Math.min(items.length - 1, idx + step));
    setSelected(items[next]);
    items[next].scrollIntoView({ block: "nearest" });
  }
});

window.addEventListener("popstate", () => {
  if (location.pathname === "/resources/" || location.pathname === "/resources") {
    navigateFolder(location.href, false);
  }
});

document.addEventListener("scroll", closeMenu, { capture: true, passive: true });
window.addEventListener("resize", closeMenu, { passive: true });

document.addEventListener("plainmote:resource-results", () => {
  closeMenu();
  setSelected(null);
  syncReturnInputs();
  observeLoadMore();
});

observeLoadMore();

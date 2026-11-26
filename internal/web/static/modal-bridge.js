// Runs inside same-origin Finder modal sheets (?modal=edit|share|versions).
// Bridges close buttons, Escape key, and save/mutation signals back to the
// parent dashboard without coupling either side's internals.

function notifyParent(type, detail = {}) {
  if (window.parent && window.parent !== window) {
    window.parent.postMessage({ type, ...detail }, location.origin);
  }
}

document.addEventListener("click", (event) => {
  const closeBtn = event.target.closest("[data-modal-close]");
  if (!closeBtn) return;
  event.preventDefault();
  notifyParent("plainmote:modal-close");
});

document.addEventListener("keydown", (event) => {
  if (event.key !== "Escape") return;
  // Exit inline title editing before closing the whole modal.
  if (document.activeElement instanceof HTMLInputElement && document.activeElement.hasAttribute("data-modal-filename")) {
    event.preventDefault();
    document.activeElement.blur();
    return;
  }
  // Let nested dialogs (e.g. share terms or seal confirm) handle Escape first.
  if (document.querySelector("dialog[open]")) return;
  event.preventDefault();
  notifyParent("plainmote:modal-close");
});

function currentResourceID() {
  const match = /^\/resources\/([0-9a-f-]{36})(?:\/|$)/i.exec(location.pathname);
  return match ? match[1] : "";
}

// Signal parent when a create, save, seal/unseal, keep, or restore just landed.
const params = new URLSearchParams(location.search);
const isEditModal = document.body.dataset.modalMode === "edit";
if (isEditModal && (params.has("created") || params.has("saved")) && !document.querySelector(".err, .warnbox")) {
  notifyParent("plainmote:modal-saved", { resourceId: currentResourceID() });
} else if (params.has("created") || params.has("saved") || params.has("sealed") ||
    params.has("unsealed") || params.has("kept") || params.has("restored")) {
  notifyParent("plainmote:modal-dirty", { resourceId: currentResourceID() });
}

// In-place parts.js save inside the edit modal auto-closes on success (keeping
// the modal open if a conflict or error prevented the save).
const editForm = isEditModal ? document.querySelector("form.rform[data-parts]") : null;
editForm?.addEventListener("plainmote:submitted", (event) => {
  if (event.detail?.saved) {
    notifyParent("plainmote:modal-saved", { resourceId: currentResourceID() });
  }
});

// In-place parts.js updates (saving text, creating/revoking shares) also mark
// the parent folder view dirty so icons & share dots refresh on close.
document.addEventListener("plainmote:parts", (event) => {
  const part = event.detail?.root?.dataset?.part;
  if (part === "shares" || part === "base" || part === "notices") {
    notifyParent("plainmote:modal-dirty", { resourceId: currentResourceID() });
  }
});

// Merged modal title input: double-click selects the entire filename (including
// extension), Enter finishes editing cleanly without accidental form submit.
const modalFilename = document.querySelector("[data-modal-filename]");
if (modalFilename instanceof HTMLInputElement) {
  modalFilename.addEventListener("dblclick", () => {
    modalFilename.select();
  });
  modalFilename.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      event.preventDefault();
      modalFilename.blur();
    }
  });
}

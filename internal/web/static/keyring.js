// The account page's master password: setting it, unlocking, locking,
// changing it, a new recovery key, and recovering with one. What is typed
// here is used in this browser and never sent; the service receives only
// what keys.js wraps, at /account/keyring.
import { MIN_PASSWORD, createKeyring, openWithPassword, openWithRecovery, rewrapPassword, rewrapRecovery } from "./keys.js";
import { put, get, lock, setLockMinutes } from "./vault.js";

const section = document.querySelector("[data-keyring]");
const body = document.body.dataset;
const csrf = document.querySelector('input[name="csrf"]')?.value || "";

function message(name) {
  return body[`msgKeyring${name}`] || "";
}

async function readRing() {
  const response = await fetch("/account/keyring", { headers: { Accept: "application/json" }, credentials: "same-origin" });
  if (!response.ok) throw new Error(message("Failed"));
  const ring = await response.json();
  ring.iterations = Number(ring.iterations);
  return ring;
}

async function writeRing(fields) {
  const response = await fetch("/account/keyring", {
    method: "POST",
    credentials: "same-origin",
    headers: { Accept: "application/json" },
    body: new URLSearchParams({ csrf, ...fields }),
  });
  const answer = await response.json().catch(() => ({}));
  if (!response.ok) throw new Error(answer.message || message("Failed"));
  return answer;
}

function showError(form, text) {
  const slot = form.querySelector("[data-keyring-error]");
  if (slot) slot.textContent = text;
}

function busy(form, on) {
  for (const button of form.querySelectorAll("button")) button.disabled = on;
}

// A new password: long enough, typed the same twice. Under twelve
// characters it is accepted with advice, not refused.
function checkNew(form) {
  const password = form.elements.password.value;
  if (password.length < MIN_PASSWORD) return message("TooShort");
  if (password !== form.elements.confirm.value) return message("Mismatch");
  return "";
}

function watchLength(form) {
  const slot = form.querySelector("[data-keyring-length]");
  if (!slot || !form.elements.password) return;
  const update = () => {
    const length = form.elements.password.value.length;
    slot.textContent = length === 0 ? "" : (length < 12 ? message("Short") : message("Long")).replace("%d", length);
  };
  form.elements.password.addEventListener("input", update);
}

// The recovery step shows the key, offers to copy or download it, and does
// nothing until it is marked as kept.
function showRecovery(dialog, recovery, onKept) {
  const passwordStep = dialog.querySelector('[data-keyring-step="password"]');
  const step = dialog.querySelector('[data-keyring-step="recovery"]');
  step.querySelector("[data-keyring-rk]").textContent = recovery;
  const download = step.querySelector("[data-keyring-download]");
  const file = new Blob([`PlainMote recovery key\n${location.origin}\n\n${recovery}\n`], { type: "text/plain" });
  if (download.dataset.url) URL.revokeObjectURL(download.dataset.url);
  download.dataset.url = URL.createObjectURL(file);
  download.href = download.dataset.url;
  step.querySelector("[data-keyring-copy]").onclick = () => navigator.clipboard?.writeText(recovery).catch(() => {});
  step.querySelector("[data-keyring-back]").onclick = () => {
    step.hidden = true;
    passwordStep.hidden = false;
  };
  step.onsubmit = async (event) => {
    event.preventDefault();
    busy(step, true);
    showError(step, "");
    try {
      await onKept();
    } catch (error) {
      showError(step, error.message || message("Failed"));
      busy(step, false);
    }
  };
  passwordStep.hidden = true;
  step.hidden = false;
  step.elements.kept.checked = false;
}

function finish() {
  location.reload();
}

const user = section?.dataset.user || "";

function setStatus(unlocked) {
  const status = section.querySelector("[data-keyring-status]");
  if (status) {
    status.classList.toggle("on", unlocked);
    status.classList.toggle("off", !unlocked);
    status.querySelector("span").textContent = unlocked ? status.dataset.unlocked : message("Locked");
  }
  const unlock = section.querySelector("[data-keyring-unlock]");
  if (unlock) unlock.hidden = unlocked;
  const lockNow = section.querySelector("[data-keyring-lock-now]");
  if (lockNow) lockNow.hidden = !unlocked;
}

const flows = {
  // Setting the password: the recovery key is shown before anything is
  // written, and the keyring is sent only once it has been kept.
  setup(dialog) {
    const form = dialog.querySelector('[data-keyring-step="password"]');
    watchLength(form);
    form.onsubmit = async (event) => {
      event.preventDefault();
      const problem = checkNew(form);
      if (problem) return showError(form, problem);
      busy(form, true);
      showError(form, "");
      try {
        const made = await createKeyring(user, form.elements.password.value);
        showRecovery(dialog, made.recovery, async () => {
          const ring = await writeRing({ action: "create", ...made.record });
          await put(user, made.key, ring.lock_minutes);
          finish();
        });
      } catch (error) {
        showError(form, error.message || message("Failed"));
      }
      busy(form, false);
    };
  },

  password(dialog) {
    const form = dialog.querySelector('[data-keyring-step="password"]');
    watchLength(form);
    form.onsubmit = async (event) => {
      event.preventDefault();
      const problem = checkNew(form);
      if (problem) return showError(form, problem);
      busy(form, true);
      showError(form, "");
      try {
        const ring = await readRing();
        let accountKey;
        try {
          accountKey = await openWithPassword(user, ring, form.elements.current.value, true);
        } catch {
          throw new Error(message("Wrong"));
        }
        const next = await rewrapPassword(user, accountKey, form.elements.password.value);
        await writeRing({ action: "password", version: String(ring.version), ...next.fields });
        await put(user, next.key, ring.lock_minutes);
        finish();
      } catch (error) {
        showError(form, error.message || message("Failed"));
        busy(form, false);
      }
    };
  },

  recovery(dialog) {
    const form = dialog.querySelector('[data-keyring-step="password"]');
    form.onsubmit = async (event) => {
      event.preventDefault();
      busy(form, true);
      showError(form, "");
      try {
        const ring = await readRing();
        let accountKey;
        try {
          accountKey = await openWithPassword(user, ring, form.elements.current.value, true);
        } catch {
          throw new Error(message("Wrong"));
        }
        const next = await rewrapRecovery(user, accountKey);
        showRecovery(dialog, next.recovery, async () => {
          await writeRing({ action: "recovery", version: String(ring.version), ...next.fields });
          finish();
        });
      } catch (error) {
        showError(form, error.message || message("Failed"));
      }
      busy(form, false);
    };
  },

  recover(dialog) {
    const form = dialog.querySelector('[data-keyring-step="password"]');
    watchLength(form);
    form.onsubmit = async (event) => {
      event.preventDefault();
      const problem = checkNew(form);
      if (problem) return showError(form, problem);
      busy(form, true);
      showError(form, "");
      try {
        const ring = await readRing();
        let accountKey;
        try {
          accountKey = await openWithRecovery(user, ring, form.elements.recovery.value, true);
        } catch {
          throw new Error(message("WrongRecovery"));
        }
        const next = await rewrapPassword(user, accountKey, form.elements.password.value);
        await writeRing({ action: "password", version: String(ring.version), ...next.fields });
        await put(user, next.key, ring.lock_minutes);
        finish();
      } catch (error) {
        showError(form, error.message || message("Failed"));
        busy(form, false);
      }
    };
  },
};

async function start() {
  if (!section || !globalThis.crypto?.subtle) return;
  for (const [name, setup] of Object.entries(flows)) {
    const dialog = document.querySelector(`[data-keyring-dialog="${name}"]`);
    if (!dialog) continue;
    setup(dialog);
    for (const close of dialog.querySelectorAll("[data-keyring-close]")) close.onclick = () => dialog.close();
    for (const opener of document.querySelectorAll(`[data-keyring-open="${name}"]`)) {
      opener.disabled = false;
      opener.addEventListener("click", () => {
        for (const form of dialog.querySelectorAll("form")) {
          form.reset();
          form.hidden = form.dataset.keyringStep !== "password";
          showError(form, "");
          busy(form, false);
        }
        dialog.showModal();
      });
    }
  }

  const unlock = section.querySelector("[data-keyring-unlock]");
  if (!unlock) return;
  const lockMinutes = Number(section.dataset.lock) || 15;
  // The time chosen here applies to a key already unlocked in this browser.
  await setLockMinutes(user, lockMinutes);
  setStatus(Boolean(await get(user)));
  unlock.addEventListener("submit", async (event) => {
    event.preventDefault();
    busy(unlock, true);
    showError(unlock, "");
    try {
      const ring = await readRing();
      let key;
      try {
        key = await openWithPassword(user, ring, unlock.elements.password.value);
      } catch {
        throw new Error(message("Wrong"));
      }
      if (!(await put(user, key, ring.lock_minutes))) throw new Error(message("NoStorage"));
      unlock.reset();
      setStatus(true);
    } catch (error) {
      showError(unlock, error.message || message("Failed"));
    }
    busy(unlock, false);
  });
  section.querySelector("[data-keyring-lock-now]")?.addEventListener("click", async () => {
    await lock(user);
    setStatus(false);
  });
  document.addEventListener("plainmote:locked", () => setStatus(false));
}

start();

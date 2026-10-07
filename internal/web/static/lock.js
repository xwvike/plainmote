// Runs on every page: keeps an unlocked account key (vault.js) no longer than
// it may be kept. Signed out, any key goes; signed in, only the account's own
// stays, and only until it has sat idle for its time. Signing out takes it
// before the request leaves. Pages with nothing unlocked do nothing more than
// look, and create nothing.
//
// On an account with a master password the top bar carries its lock: the
// state, and unlocking or locking in place. Either way the page hears of it
// - plainmote:unlocked with the key, plainmote:locked - and what it shows
// encrypted opens, or closes again, without reloading.
import { present, sweep, touch, alive, lock } from "./vault.js";

const user = document.querySelector('meta[name="plainmote-user"]')?.content || "";

function announce() {
  document.dispatchEvent(new CustomEvent("plainmote:locked"));
}

// Signing out: the key goes first, then the form is sent as it was.
for (const form of document.querySelectorAll('form[action="/logout"]')) {
  form.addEventListener("submit", (event) => {
    if (form.dataset.locked === "true") return;
    event.preventDefault();
    lock(user).finally(() => {
      form.dataset.locked = "true";
      form.submit();
    });
  });
}

let watching = false;

async function watch() {
  if (watching || !(await present())) return;
  if (!(await sweep(user)) || !user) return;
  watching = true;
  let last = Date.now();
  const bump = () => {
    if (Date.now() - last < 30000) return;
    last = Date.now();
    touch(user).then((fresh) => {
      if (!fresh) announce();
    });
  };
  for (const type of ["pointerdown", "keydown", "scroll"]) addEventListener(type, bump, { passive: true });
  const check = () => alive(user).then((fresh) => {
    if (!fresh) {
      clearInterval(timer);
      announce();
    }
  });
  const timer = setInterval(check, 30000);
  document.addEventListener("visibilitychange", () => {
    if (document.visibilityState === "visible") check();
  });
}

// --- The top bar's lock -------------------------------------------------------

const control = document.querySelector("[data-keylock]");

function showState(unlocked) {
  if (!control) return;
  control.classList.toggle("unlocked", unlocked);
  control.querySelector("[data-keylock-state]").textContent = unlocked ? control.dataset.msgUnlocked : control.dataset.msgLocked;
  control.querySelector("[data-keylock-unlock]").hidden = unlocked;
  control.querySelector("[data-keylock-open]").hidden = !unlocked;
}

function startControl() {
  const form = control.querySelector("[data-keylock-unlock]");
  const password = form.querySelector("input");
  const problem = form.querySelector("[data-keylock-error]");
  control.addEventListener("toggle", () => {
    if (control.open && !form.hidden) password.focus();
  });
  // A popover: away from it, or Escape, closes it.
  document.addEventListener("click", (event) => {
    if (control.open && !control.contains(event.target)) control.open = false;
  });
  control.addEventListener("keydown", (event) => {
    if (event.key === "Escape") {
      control.open = false;
      control.querySelector("summary").focus();
    }
  });
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    problem.textContent = "";
    for (const button of form.querySelectorAll("button")) button.disabled = true;
    try {
      // The keys' code is loaded only when it is needed.
      const { unlock, WrongPassword, NoStorage } = await import("./unlock.js");
      try {
        await unlock(password.value, user);
        form.reset();
        control.open = false;
      } catch (error) {
        problem.textContent = error instanceof WrongPassword ? control.dataset.msgWrong
          : error instanceof NoStorage ? control.dataset.msgNoStorage : control.dataset.msgFailed;
        password.select();
      }
    } finally {
      for (const button of form.querySelectorAll("button")) button.disabled = false;
    }
  });
  control.querySelector("[data-keylock-lock]").addEventListener("click", async () => {
    await lock(user);
    control.open = false;
    announce();
  });
  document.addEventListener("plainmote:unlocked", () => {
    showState(true);
    watch();
  });
  document.addEventListener("plainmote:locked", () => showState(false));
  alive(user).then(showState).catch(() => showState(false));
}

if (control && user) startControl();
watch();

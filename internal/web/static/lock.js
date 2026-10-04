// Runs on every page: keeps an unlocked account key (vault.js) no longer than
// it may be kept. Signed out, any key goes; signed in, only the account's own
// stays, and only until it has sat idle for its time. Signing out takes it
// before the request leaves. Pages with nothing unlocked do nothing more than
// look, and create nothing.
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

async function watch() {
  if (!(await present())) return;
  if (!(await sweep(user)) || !user) return;
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

watch();

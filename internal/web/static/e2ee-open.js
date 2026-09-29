// Opening an encrypted share. The page itself holds nothing; this fetches the
// ciphertext once - that request is the one that counts as a use - and
// decrypts it here with the key after # in the address, or with the
// passphrase the recipient types, then hands the text to the browser. A
// wrong passphrase is tried again against the bytes already fetched, never by
// fetching again.
import { inspect, open } from "./e2ee.js";

const page = document.querySelector("[data-decrypt]");
const status = page.querySelector("[data-status]");
const form = page.querySelector("[data-unlock]");
const msg = (name) => page.dataset[name] || "";

const say = (message, bad = false) => {
  status.textContent = message;
  status.classList.toggle("err", bad);
  status.hidden = !message;
};

// The plaintext opens the way every other text link does: as the browser's
// own view of plain text. The address bar then shows the blob's address, not
// the one with the key; the blob lives only as long as this tab keeps it, so
// a reload finds nothing, and opening the link again is how to see it again.
function show({ content }) {
  window.location.replace(URL.createObjectURL(new Blob([content], { type: "text/plain; charset=utf-8" })));
}

async function start() {
  say(msg("msgLoading"));
  let envelope;
  try {
    const response = await fetch(page.dataset.source, { cache: "no-store", credentials: "omit", redirect: "error" });
    if (response.status === 401 || response.status === 404) return say(msg("msgExpired"), true);
    if (!response.ok) throw new Error(String(response.status));
    envelope = new Uint8Array(await response.arrayBuffer());
    inspect(envelope);
  } catch (_) {
    return say(msg("msgFailed"), true);
  }

  if (inspect(envelope).passphrase) {
    say(msg("msgPassphrase"));
    form.hidden = false;
    const input = form.querySelector("input");
    input.focus();
    form.addEventListener("submit", async (event) => {
      event.preventDefault();
      const button = form.querySelector("button");
      button.disabled = true;
      try {
        show(await open(envelope, { passphrase: input.value }));
      } catch (_) {
        say(msg("msgWrong"), true);
        input.select();
      } finally {
        button.disabled = false;
      }
    });
    return;
  }

  const hash = window.location.hash;
  if (!hash.startsWith("#k=") || hash.length <= 3) return say(msg("msgNoKey"), true);
  try {
    show(await open(envelope, { key: hash.slice(3) }));
  } catch (_) {
    say(msg("msgWrong"), true);
  }
}

start();

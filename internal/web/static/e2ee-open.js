// Opening an encrypted share. The page itself holds nothing; this fetches the
// ciphertext once - that request is the one that counts as a use - and
// decrypts it here with the key after # in the address, or with the
// passphrase the recipient types. A wrong passphrase is tried again against
// the bytes already fetched, never by fetching again.
import { inspect, open } from "./e2ee.js";

const page = document.querySelector("[data-decrypt]");
const status = page.querySelector("[data-status]");
const form = page.querySelector("[data-unlock]");
const output = page.querySelector("[data-output]");
const text = page.querySelector("[data-plain]");
const meta = page.querySelector("[data-meta]");
const copy = page.querySelector("[data-copy-plain]");
const download = page.querySelector("[data-download]");
const msg = (name) => page.dataset[name] || "";

const say = (message, bad = false) => {
  status.textContent = message;
  status.classList.toggle("err", bad);
  status.hidden = !message;
};

function sizeText(bytes) {
  if (bytes >= 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(2)} MiB`;
  if (bytes >= 1024) return `${(bytes / 1024).toFixed(2)} KiB`;
  return `${bytes} B`;
}

function show({ name, content }) {
  const filename = name || "share.txt";
  text.textContent = new TextDecoder("utf-8").decode(content);
  meta.textContent = `${name ? `${name} · ` : ""}${sizeText(content.length)}`;
  document.title = `${filename} · PlainMote`;
  const url = URL.createObjectURL(new Blob([content], { type: "application/octet-stream" }));
  download.href = url;
  download.download = filename;
  copy.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(text.textContent);
      copy.classList.add("copied");
      setTimeout(() => copy.classList.remove("copied"), 1400);
    } catch (_) { /* the text stays selectable */ }
  });
  form.hidden = true;
  say("");
  output.hidden = false;
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

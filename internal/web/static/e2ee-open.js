// Opening an encrypted share. The page itself holds nothing; this fetches the
// ciphertext once - that request is the one that counts as a use - and
// decrypts it here with the key after # in the address, or with the
// passphrase the recipient types. A wrong passphrase is tried again against
// the bytes already fetched, never by fetching again.
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

function sizeText(bytes) {
  if (bytes >= 1024 * 1024) return `${(bytes / (1024 * 1024)).toFixed(2)} MiB`;
  if (bytes >= 1024) return `${(bytes / 1024).toFixed(2)} KiB`;
  return `${bytes} B`;
}

// Text becomes the page, and the page becomes what a browser shows for any
// other text link: our styles gone, the text in its own <pre>, wrapped, in
// the reader's light or dark. The address - key and all - stays, so a reload
// fetches and decrypts again, as a reload of any link fetches again.
function showText(name, text) {
  for (const sheet of document.querySelectorAll('link[rel="stylesheet"]')) sheet.remove();
  const root = document.documentElement;
  root.removeAttribute("data-theme");
  root.style.colorScheme = "light dark";
  const pre = document.createElement("pre");
  pre.style.whiteSpace = "pre-wrap";
  pre.style.overflowWrap = "break-word";
  pre.textContent = text;
  document.body.replaceChildren(pre);
  document.title = name || decodeURIComponent(window.location.pathname.split("/").pop() || "");
}

// Anything that is not text is handed over the way an unencrypted file is:
// as a download, under its own name. The link stays for a second try.
function offerFile(name, content) {
  const link = document.createElement("a");
  link.href = URL.createObjectURL(new Blob([content], { type: "application/octet-stream" }));
  link.download = name || "file";
  link.textContent = `${link.download} · ${sizeText(content.length)}`;
  form.hidden = true;
  say(msg("msgDownload"));
  status.append(" ", link);
  link.click();
}

function show({ name, content }) {
  let text;
  try {
    text = new TextDecoder("utf-8", { fatal: true }).decode(content);
  } catch (_) {
    return offerFile(name, content);
  }
  showText(name, text);
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

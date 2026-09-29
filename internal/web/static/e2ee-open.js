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

// What decrypted bytes are, by their own signature: the image, audio and
// video families an unencrypted file is shown as. Anything else is not
// named, and is handed over as a download.
function mediaKind(bytes) {
  const ascii = (at, text) => [...text].every((char, i) => bytes[at + i] === char.charCodeAt(0));
  if (bytes[0] === 0x89 && ascii(1, "PNG")) return ["img", "image/png"];
  if (bytes[0] === 0xff && bytes[1] === 0xd8 && bytes[2] === 0xff) return ["img", "image/jpeg"];
  if (ascii(0, "GIF8")) return ["img", "image/gif"];
  if (ascii(0, "RIFF") && ascii(8, "WEBP")) return ["img", "image/webp"];
  if (ascii(0, "BM")) return ["img", "image/bmp"];
  if (ascii(4, "ftyp")) {
    const brand = String.fromCharCode(...bytes.subarray(8, 12));
    if (brand === "avif" || brand === "avis") return ["img", "image/avif"];
    if (brand.startsWith("M4A")) return ["audio", "audio/mp4"];
    return ["video", "video/mp4"];
  }
  if (bytes[0] === 0x1a && bytes[1] === 0x45 && bytes[2] === 0xdf && bytes[3] === 0xa3) return ["video", "video/webm"];
  if (ascii(0, "OggS")) return ["audio", "audio/ogg"];
  if (ascii(0, "fLaC")) return ["audio", "audio/flac"];
  if (ascii(0, "RIFF") && ascii(8, "WAVE")) return ["audio", "audio/wav"];
  if (ascii(0, "ID3") || (bytes[0] === 0xff && (bytes[1] & 0xe0) === 0xe0)) return ["audio", "audio/mpeg"];
  return null;
}

// The content becomes the page, and the page becomes what a browser shows
// for any other link to the same thing: our styles gone, nothing of ours on
// it, in the reader's light or dark. The address - key and all - stays, so a
// reload fetches and decrypts again, as a reload of any link fetches again.
function bare(name) {
  for (const sheet of document.querySelectorAll('link[rel="stylesheet"]')) sheet.remove();
  const root = document.documentElement;
  root.removeAttribute("data-theme");
  root.style.colorScheme = "light dark";
  document.title = name || decodeURIComponent(window.location.pathname.split("/").pop() || "");
}

// Text in its own wrapped <pre>, as a browser shows a text file.
function showText(name, text) {
  bare(name);
  const pre = document.createElement("pre");
  pre.style.whiteSpace = "pre-wrap";
  pre.style.overflowWrap = "break-word";
  pre.textContent = text;
  document.body.replaceChildren(pre);
}

// An image, or a player, alone and centred on a dark page, as a browser shows
// one opened by itself.
function showMedia(name, content, [tag, type]) {
  bare(name);
  const root = document.documentElement;
  root.style.height = "100%";
  const body = document.body;
  body.style.margin = "0";
  body.style.height = "100%";
  body.style.background = "#0e0e0e";
  const media = document.createElement(tag);
  media.src = URL.createObjectURL(new Blob([content], { type }));
  if (tag === "img") media.alt = name || "";
  else media.controls = true;
  Object.assign(media.style, {
    position: "absolute", inset: "0", margin: "auto", maxWidth: "100%", maxHeight: "100%", display: "block",
  });
  body.replaceChildren(media);
}

// Anything else is handed over the way an unencrypted file is: as a
// download, under its own name. The link stays for a second try.
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

// UTF-8 text first, then what the bytes prove to be media, then text in
// another encoding - a UTF-16 export from Windows, a GBK log - read with the
// editor's detection, fetched only when it is needed. Only what is none of
// these is a download.
async function show({ name, content }) {
  let text = null;
  try {
    text = new TextDecoder("utf-8", { fatal: true }).decode(content);
  } catch (_) { /* not UTF-8 */ }
  if (text !== null) return showText(name, text);
  // A UTF-16 or UTF-32 byte order mark says text before anything else does:
  // FF FE also reads as the start of an MP3 frame.
  const [b0, b1, b2, b3] = content;
  const unicode = (b0 === 0xff && b1 === 0xfe) || (b0 === 0xfe && b1 === 0xff) || (b0 === 0 && b1 === 0 && b2 === 0xfe && b3 === 0xff);
  const kind = unicode ? null : mediaKind(content);
  if (kind) return showMedia(name, content, kind);
  try {
    const editor = await import("./editor.js");
    const encoding = editor.detectFileEncoding(content);
    if (encoding) return showText(name, editor.decodeBytes(content, encoding));
  } catch (_) { /* not text in any encoding the editor reads */ }
  offerFile(name, content);
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
        await show(await open(envelope, { passphrase: input.value }));
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
    await show(await open(envelope, { key: hash.slice(3) }));
  } catch (_) {
    say(msg("msgWrong"), true);
  }
}

start();

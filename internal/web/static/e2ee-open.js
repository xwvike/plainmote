// Opening an encrypted share. The page itself holds nothing; this fetches the
// ciphertext once - that request is the one that counts as a use - and
// decrypts it here with the key after # in the address, or with the
// four-character code the recipient types.
import { inspect, open, fromBase64URL } from "./e2ee.js";
import { isBundle, openBundle } from "./seal.js";
import { sizeText } from "./upload.js";

const page = document.querySelector("[data-decrypt]");
const msg = (name) => page.dataset[name] || "";

// Nothing of ours is on screen once this runs: the page is emptied to a bare
// one in the reader's light or dark, a line of status in the middle until
// there is something to show - the four code cells, or the content itself.
// Its few rules are a constructed sheet, which the page's policy allows where
// it would refuse a <style> element.
const BARE_CSS = `
body.bare { margin: 0; min-height: 100vh; display: grid; place-items: center; font: 14px/1.5 system-ui, sans-serif; }
.bare-box { display: grid; justify-items: center; gap: 16px; padding: 24px; text-align: center; }
.bare-note { margin: 0; opacity: .7; }
.bare-note.bad { opacity: 1; color: #c0392b; }
.pin-cells { display: flex; gap: 10px; }
.pin-cells input { width: 46px; height: 56px; padding: 0; box-sizing: border-box; text-align: center;
  font: 600 24px ui-monospace, SFMono-Regular, Menlo, monospace; text-transform: uppercase;
  border: 1px solid color-mix(in srgb, CanvasText 28%, transparent); border-radius: 6px;
  background: Canvas; color: CanvasText; caret-color: transparent; outline: none; }
.pin-cells input:focus { border-color: CanvasText; box-shadow: 0 0 0 1px CanvasText; }
.bare-box.busy .pin-cells { opacity: .45; }
.bare-box.wrong .pin-cells { animation: pin-shake .32s; }
@keyframes pin-shake { 25% { transform: translateX(-6px); } 50% { transform: translateX(6px); } 75% { transform: translateX(-3px); } }
`;

// A browser without constructed sheets still gets a working page, only
// unstyled.
function bareSheets() {
  try {
    const sheet = new CSSStyleSheet();
    sheet.replaceSync(BARE_CSS);
    return [sheet];
  } catch (_) {
    return [];
  }
}

let box = null;
let note = null;

function bareShell() {
  bare("");
  document.adoptedStyleSheets = bareSheets();
  document.body.className = "bare";
  box = document.createElement("div");
  box.className = "bare-box";
  note = document.createElement("p");
  note.className = "bare-note";
  note.setAttribute("role", "status");
  box.append(note);
  document.body.replaceChildren(box);
}

const say = (message, bad = false) => {
  note.textContent = message;
  note.classList.toggle("bad", bad);
};

// What decrypted bytes are, by their own signature: the image, audio and
// video families an unencrypted file is shown as. Anything else is not
// named, and is handed over as a download.
function mediaKind(bytes) {
  const ascii = (at, text) => [...text].every((char, i) => bytes[at + i] === char.charCodeAt(0));
  if (bytes[0] === 0x89 && ascii(1, "PNG")) return ["img", "image/png"];
  if (bytes[0] === 0xff && bytes[1] === 0xd8 && bytes[2] === 0xff) return ["img", "image/jpeg"];
  if (ascii(0, "GIF8")) return ["img", "image/gif"];
  if (ascii(0, "RIFF") && ascii(8, "WEBP")) return ["img", "image/webp"];
  // "BM" alone is two letters; a bitmap also names a known header size.
  if (ascii(0, "BM") && [12, 40, 56, 108, 124].includes(bytes[14]) && !bytes[15] && !bytes[16] && !bytes[17]) return ["img", "image/bmp"];
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
  // An MPEG frame: the sync bits, then a layer and a bitrate that exist.
  const frame = bytes[0] === 0xff && (bytes[1] & 0xe0) === 0xe0 && (bytes[1] & 0x06) !== 0 && (bytes[2] & 0xf0) !== 0xf0;
  if (ascii(0, "ID3") || frame) return ["audio", "audio/mpeg"];
  return null;
}

// The content becomes the page, and the page becomes what a browser shows
// for any other link to the same thing: our styles gone, nothing of ours on
// it, in the reader's light or dark. The address - key and all - stays, so a
// reload fetches and decrypts again, as a reload of any link fetches again.
function bare(name) {
  for (const sheet of document.querySelectorAll('link[rel="stylesheet"]')) sheet.remove();
  document.adoptedStyleSheets = [];
  document.body.className = "";
  const root = document.documentElement;
  root.removeAttribute("data-theme");
  root.style.colorScheme = "light dark";
  let tail = window.location.pathname.split("/").pop() || "";
  try {
    tail = decodeURIComponent(tail);
  } catch (_) { /* shown as it is */ }
  document.title = name || tail;
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
  bareShell();
  say(msg("msgDownload"));
  note.append(" ", link);
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

// Four cells for the code, one character each. Typing moves on, Backspace
// moves back, a pasted code fills them all; letters are taken in either case,
// and the fourth character submits.
function pinCells(onComplete) {
  const row = document.createElement("div");
  row.className = "pin-cells";
  const cells = [];
  const value = () => cells.map((cell) => cell.value).join("");
  const fill = (text, from) => {
    let at = from;
    for (const char of String(text).toUpperCase().replace(/[^A-Z0-9]/g, "")) {
      if (at >= cells.length) break;
      cells[at].value = char;
      at += 1;
    }
    cells[Math.min(at, cells.length - 1)].focus();
    if (value().length === cells.length) onComplete(value());
  };
  for (let index = 0; index < 4; index += 1) {
    const cell = document.createElement("input");
    cell.autocomplete = "off";
    cell.spellcheck = false;
    cell.setAttribute("autocapitalize", "characters");
    cell.setAttribute("aria-label", `${index + 1} / 4`);
    cell.addEventListener("focus", () => cell.select());
    cell.addEventListener("input", () => {
      const typed = cell.value;
      cell.value = "";
      fill(typed, index);
    });
    cell.addEventListener("paste", (event) => {
      event.preventDefault();
      fill(event.clipboardData.getData("text"), index);
    });
    cell.addEventListener("keydown", (event) => {
      if (event.key === "Backspace" && !cell.value && index > 0) {
        event.preventDefault();
        cells[index - 1].value = "";
        cells[index - 1].focus();
      } else if (event.key === "ArrowLeft" && index > 0) {
        cells[index - 1].focus();
      } else if (event.key === "ArrowRight" && index < 3) {
        cells[index + 1].focus();
      }
    });
    cells.push(cell);
  }
  row.append(...cells);
  return {
    row,
    reset() {
      for (const cell of cells) cell.value = "";
      cells[0].focus();
    },
    disable(disabled) {
      for (const cell of cells) cell.disabled = disabled;
    },
    focus() { cells[0].focus(); },
  };
}

async function start() {
  bareShell();
  say(msg("msgLoading"));
  let envelope;
  try {
    const response = await fetch(page.dataset.source, { cache: "no-store", credentials: "omit", redirect: "error" });
    if (response.status === 401 || response.status === 404) return say(msg("msgExpired"), true);
    if (!response.ok) throw new Error(String(response.status));
    envelope = new Uint8Array(await response.arrayBuffer());
    if (!isBundle(envelope)) inspect(envelope);
  } catch (_) {
    return say(msg("msgFailed"), true);
  }

  // An encrypted resource: what came back carries the content key wrapped
  // for this link, which the key after # opens.
  if (isBundle(envelope)) {
    const hash = window.location.hash;
    if (!hash.startsWith("#k=") || hash.length <= 3) return say(msg("msgNoKey"), true);
    try {
      const opened = await openBundle(envelope, fromBase64URL(hash.slice(3)));
      return show({ name: opened.filename || opened.name, content: opened.content });
    } catch (_) {
      return say(msg("msgWrong"), true);
    }
  }

  if (inspect(envelope).passphrase) {
    // A wrong code is tried again against the bytes already fetched, never
    // by fetching again: only opening the link counts as a use.
    say(msg("msgPin"));
    const pin = pinCells(async (code) => {
      box.classList.remove("wrong");
      box.classList.add("busy");
      pin.disable(true);
      try {
        await show(await open(envelope, { passphrase: code }));
      } catch (_) {
        box.classList.remove("busy");
        pin.disable(false);
        void box.offsetWidth;
        box.classList.add("wrong");
        say(msg("msgPinWrong"), true);
        pin.reset();
      }
    });
    box.prepend(pin.row);
    pin.focus();
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

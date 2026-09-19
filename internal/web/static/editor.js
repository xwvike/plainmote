// The content editor.
//
// This is progressive enhancement, not a replacement: the <textarea> stays in
// the DOM, keeps its name, and is what the form posts. CodeMirror is mounted
// beside it and its document is copied back on submit. If this module fails to
// load, throws, or is blocked, the page is exactly what it was before - a
// textarea in a form - which is the behaviour every no-JS path depends on.
import {
  analyseEncoding, bracketMatching, closeBrackets, closeBracketsKeymap, Compartment,
  crosshairCursor, defaultKeymap, drawSelection, dropCursor, EditorState,
  EditorView, highlightActiveLine, highlightActiveLineGutter,
  highlightSelectionMatches, highlightSpecialChars, HighlightStyle, history,
  historyKeymap, indentOnInput, indentUnit, json, keymap,
  lineNumbers, properties, rectangularSelection, search, searchKeymap, shell,
  StreamLanguage, syntaxHighlighting, tags, toml, xml, yaml,
} from "./vendor/codemirror.js";
import { uploadController } from "./upload.js";

const LANGUAGES = {
  yaml: () => yaml(),
  json: () => json(),
  xml: () => xml(),
  toml: () => StreamLanguage.define(toml),
  sh: () => StreamLanguage.define(shell),
  ini: () => StreamLanguage.define(properties),
};

function languageFor(token) {
  const make = LANGUAGES[String(token || "").toLowerCase()];
  return make ? make() : [];
}

const EXTENSIONS = {
  yaml: "yaml", yml: "yaml", json: "json", toml: "toml", xml: "xml", sh: "sh",
  ini: "ini", cfg: "ini", conf: "ini", env: "ini", properties: "ini",
};

const CONTENT_TYPES = {
  "application/yaml": "yaml",
  "application/json": "json",
  "application/toml": "toml",
  "application/xml": "xml",
};

// Syntax highlighting belongs entirely to the browser layer. A recognised
// filename extension wins; without one, the stored content type is a fallback.
function tokenFor(filename, contentType) {
  const name = String(filename || "");
  const dot = name.lastIndexOf(".");
  if (dot >= 0) return EXTENSIONS[name.slice(dot + 1).toLowerCase()] || "";
  const base = String(contentType || "").split(";", 1)[0].trim().toLowerCase();
  return CONTENT_TYPES[base] || "";
}

// Line endings are file metadata, not text. CodeMirror always stores an LF
// document and joins with state.lineBreak on the way out, so the ending is
// carried in a facet and reapplied by sliceDoc() - never by doc.toString(),
// which hardcodes LF and would rewrite every line in a CRLF file.
const EOL_SEQUENCES = { crlf: "\r\n", lf: "\n" };

function normalizeEOLName(value) {
  return String(value || "").trim().toLowerCase() === "crlf" ? "crlf" : "lf";
}

// Majority wins, and only CRLF or LF is ever chosen: a lone CR counts as a
// break but is not an ending a file gets written back with. Same rule the
// Go side uses, so both ends agree on what a file is.
function detectEOL(text) {
  const crlf = (text.match(/\r\n/g) || []).length;
  const lf = (text.match(/\n/g) || []).length - crlf;
  const cr = (text.match(/\r/g) || []).length - crlf;
  return crlf > lf + cr ? "crlf" : "lf";
}

function submissionSource(hasUpload, hasStoredBytes, contentChanged) {
  if (hasUpload && !contentChanged) return "upload";
  if (hasStoredBytes && !contentChanged) return "stored";
  return "editor";
}

const ENCODING_ALIASES = {
  ascii: "utf-8", "us-ascii": "utf-8", utf8: "utf-8",
  "utf-8-sig": "utf-8bom", utf8bom: "utf-8bom",
  "utf-16": "utf-16le-bom", utf16le: "utf-16le", utf16be: "utf-16be",
  "utf-32": "utf-32le-bom", utf32le: "utf-32le", utf32be: "utf-32be",
  gb2312: "gbk", "gb-2312": "gbk", "gb-18030": "gb18030", "x-gbk": "gbk",
  "shift-jis": "shift_jis", sjis: "shift_jis", ms932: "shift_jis", "windows-31j": "shift_jis",
  "iso8859-1": "windows-1252", "iso-8859-1": "windows-1252", latin1: "windows-1252",
  "tis-620": "windows-874", "iso-8859-11": "windows-874",
  "iso-8859-8-i": "iso-8859-8", "x-mac-cyrillic": "mac-cyrillic",
};

const SUPPORTED_ENCODINGS = new Set([
  "utf-8", "utf-8bom", "utf-16le", "utf-16le-bom", "utf-16be", "utf-16be-bom",
  "utf-32le", "utf-32le-bom", "utf-32be", "utf-32be-bom", "gb18030", "gbk", "big5",
  "shift_jis", "euc-jp", "iso-2022-jp", "euc-kr", "windows-874",
  ...Array.from({ length: 9 }, (_, index) => `windows-${1250 + index}`),
  ...[2, 3, 4, 5, 6, 7, 8, 9, 10, 13, 14, 15].map((part) => `iso-8859-${part}`),
  "koi8-r", "koi8-u", "ibm866", "macintosh", "mac-cyrillic",
]);

function normalizeEncoding(value) {
  let name = String(value || "").trim().toLowerCase().replaceAll("_", "-");
  name = ENCODING_ALIASES[name] || name;
  if (name === "shift-jis") name = "shift_jis";
  return SUPPORTED_ENCODINGS.has(name) ? name : "";
}

function startsWithBytes(bytes, prefix) {
  return prefix.every((value, index) => bytes[index] === value);
}

function bomEncoding(bytes) {
  for (const [prefix, encoding] of [
    [[0x00, 0x00, 0xfe, 0xff], "utf-32be-bom"],
    [[0xff, 0xfe, 0x00, 0x00], "utf-32le-bom"],
    [[0xef, 0xbb, 0xbf], "utf-8bom"],
    [[0xfe, 0xff], "utf-16be-bom"],
    [[0xff, 0xfe], "utf-16le-bom"],
  ]) {
    if (startsWithBytes(bytes, prefix)) return encoding;
  }
  return "";
}

function unicodeEncodingWithoutBOM(bytes) {
  if (bytes.length >= 8 && bytes.length % 4 === 0) {
    const units = bytes.length / 4;
    for (const [encoding, positions] of [["utf-32le", [2, 3]], ["utf-32be", [0, 1]]]) {
      let zeros = 0;
      for (let offset = 0; offset < bytes.length; offset += 4) {
        for (const position of positions) if (bytes[offset + position] === 0) zeros += 1;
      }
      if (zeros >= units) {
        try {
          if (likelyText(decodeBytes(bytes, encoding))) return encoding;
        } catch (error) {
          // Try the other byte order.
        }
      }
    }
  }
  if (bytes.length >= 8 && bytes.length % 2 === 0) {
    const units = bytes.length / 2;
    for (const [encoding, highPosition] of [["utf-16le", 1], ["utf-16be", 0]]) {
      let highZeros = 0;
      let lowZeros = 0;
      for (let offset = 0; offset < bytes.length; offset += 2) {
        if (bytes[offset + highPosition] === 0) highZeros += 1;
        if (bytes[offset + (1 - highPosition)] === 0) lowZeros += 1;
      }
      if (highZeros * 100 >= units * 30 && highZeros >= lowZeros * 2) {
        try {
          if (likelyText(decodeBytes(bytes, encoding))) return encoding;
        } catch (error) {
          // Try the other byte order.
        }
      }
    }
  }
  return "";
}

function encodingBOM(encoding) {
  return {
    "utf-8bom": [0xef, 0xbb, 0xbf],
    "utf-16le-bom": [0xff, 0xfe], "utf-16be-bom": [0xfe, 0xff],
    "utf-32le-bom": [0xff, 0xfe, 0x00, 0x00], "utf-32be-bom": [0x00, 0x00, 0xfe, 0xff],
  }[encoding] || [];
}

function decodeUTF32(bytes, bigEndian) {
  if (bytes.length % 4 !== 0) throw new TypeError("invalid UTF-32 byte length");
  const codePoints = [];
  for (let index = 0; index < bytes.length; index += 4) {
    const value = bigEndian
      ? ((bytes[index] * 0x1000000) + (bytes[index + 1] << 16) + (bytes[index + 2] << 8) + bytes[index + 3])
      : (bytes[index] + (bytes[index + 1] << 8) + (bytes[index + 2] << 16) + (bytes[index + 3] * 0x1000000));
    if (value > 0x10ffff || (value >= 0xd800 && value <= 0xdfff)) throw new TypeError("invalid UTF-32 code point");
    codePoints.push(value);
  }
  let decoded = "";
  for (let index = 0; index < codePoints.length; index += 4096) {
    decoded += String.fromCodePoint(...codePoints.slice(index, index + 4096));
  }
  return decoded;
}

function decodeBytes(bytes, encoding) {
  const canonical = normalizeEncoding(encoding);
  if (!canonical) throw new TypeError(`unsupported encoding: ${encoding}`);
  const bom = encodingBOM(canonical);
  const content = bom.length > 0 && startsWithBytes(bytes, bom) ? bytes.subarray(bom.length) : bytes;
  if (canonical.startsWith("utf-32")) return decodeUTF32(content, canonical.includes("be"));
  const decoderName = {
    "utf-8bom": "utf-8",
    "utf-16le-bom": "utf-16le", "utf-16be-bom": "utf-16be",
    "mac-cyrillic": "x-mac-cyrillic",
  }[canonical] || canonical;
  return new TextDecoder(decoderName, { fatal: true }).decode(content);
}

function likelyText(content) {
  if (content.length === 0) return true;
  let controls = 0;
  let total = 0;
  for (const character of content) {
    const value = character.codePointAt(0);
    total += 1;
    if (value === 0) return false;
    if ((value < 0x20 && value !== 0x09 && value !== 0x0a && value !== 0x0d && value !== 0x0c) || (value >= 0x7f && value <= 0x9f)) {
      controls += 1;
    }
  }
  return controls * 100 <= total * 2;
}

function detectFileEncoding(bytes) {
  const bom = bomEncoding(bytes);
  if (bom) return bom;
  try {
    const decoded = decodeBytes(bytes, "utf-8");
    if (likelyText(decoded)) return "utf-8";
  } catch (error) {
    // Ranked legacy candidates below handle non-UTF-8 input.
  }
  const unicode = unicodeEncodingWithoutBOM(bytes);
  if (unicode) return unicode;
  for (const candidate of analyseEncoding(bytes)) {
    if (candidate.confidence < 20) continue;
    const encoding = normalizeEncoding(candidate.name);
    if (!encoding) continue;
    try {
      if (likelyText(decodeBytes(bytes, encoding))) return encoding;
    } catch (error) {
      // Try the next candidate supported by both browser and server.
    }
  }
  return "";
}

// Exported for the repository's browser-side encoding smoke test. The page
// itself uses the same functions below, so the test does not maintain a second
// implementation of the detection rules.
export { decodeBytes, detectFileEncoding, submissionSource, SUPPORTED_ENCODINGS };

// Colours come from the stylesheet's tokens, so the editor stays inside the
// same palette as the rest of the page instead of shipping its own.
const highlight = HighlightStyle.define([
  { tag: [tags.propertyName, tags.definition(tags.propertyName)], color: "var(--accent)" },
  { tag: [tags.keyword, tags.modifier, tags.operatorKeyword], color: "var(--accent-d)" },
  { tag: [tags.string, tags.special(tags.string)], color: "var(--ok)" },
  { tag: [tags.number, tags.bool, tags.null, tags.atom], color: "var(--warn)" },
  { tag: [tags.comment, tags.lineComment, tags.blockComment], color: "var(--ink3)", fontStyle: "italic" },
  { tag: [tags.tagName, tags.angleBracket], color: "var(--accent)" },
  { tag: tags.attributeName, color: "var(--ink2)" },
  { tag: tags.invalid, color: "var(--bad)" },
]);

// Give short and empty documents a useful editing area without inserting fake
// newlines into their saved content. The scroller, content surface, and gutter
// all fill that area; long documents scroll inside it.
//
// Height comes from .cm-host in the stylesheet rather than from here, so the
// page decides how much room the editor gets - a resource page hands it a
// whole column, a narrow viewport hands it less. The panel border closes the
// box, so the editor draws none of its own - and it draws no focus ring of its
// own either. Focus recolours that panel border, from the stylesheet, which is
// what every other control on these pages does.
//
// Drawing the ring in here does not work: .cm-gutters is sticky with a z-index
// of 200, so it paints over an inset shadow and over an inset outline alike,
// and the ring came out missing for the whole width of the line numbers. The
// panel border sits outside the editor and is in no such contest.
const theme = EditorView.theme({
  "&": { border: "0", background: "var(--panel)", color: "var(--ink)", height: "100%", overflow: "hidden" },
  "&.cm-focused": { outline: "none" },
  ".cm-scroller": { fontFamily: "var(--mono)", fontSize: "12px", lineHeight: "1.65", minHeight: 0, overflow: "auto" },
  ".cm-content": { minHeight: "100%", padding: "6px 0" },
  ".cm-line": { padding: "0 7px" },
  ".cm-gutters": { alignSelf: "stretch", background: "var(--head)", border: "none", borderRight: "1px solid var(--line)", color: "var(--ink3)", fontSize: "11px" },
  // Translucent, so the selection layer beneath it still shows through. The
  // gutter has no selection to hide and stays opaque.
  ".cm-activeLine": { background: "var(--zebra-t)" },
  ".cm-activeLineGutter": { background: "var(--zebra)", color: "var(--ink2)" },
  ".cm-cursor": { borderLeftColor: "var(--ink)" },
  // Grey while the editor is not focused, accent while it is, the way every
  // other text field on the platform behaves.
  ".cm-selectionBackground, .cm-content ::selection": { background: "var(--sel-off)" },
  "&.cm-focused .cm-selectionBackground, &.cm-focused .cm-content ::selection": { background: "var(--sel)" },
  ".cm-selectionMatch": { background: "color-mix(in oklab, var(--warn) 22%, white)" },
  ".cm-panels": { background: "var(--head)", color: "var(--ink)", borderBottom: "1px solid var(--line)" },
  ".cm-panels input, .cm-panels button": { font: "400 11.5px var(--sans)" },
  ".cm-searchMatch": { background: "color-mix(in oklab, var(--warn) 30%, white)" },
  ".cm-searchMatch.cm-searchMatch-selected": { background: "color-mix(in oklab, var(--warn) 55%, white)" },
});

function enhance(textarea) {
  const form = textarea.form;
  const language = new Compartment();
  const upload = form ? form.querySelector("input[data-upload]") : null;
  const uploads = uploadController(upload);
  const encodingSelect = form ? form.querySelector("[data-encoding-select]") : null;
  const reopenEncoding = form ? form.querySelector("[data-reopen-encoding]") : null;
  const eolSelect = form ? form.querySelector("[data-eol-select]") : null;
  const revert = form ? form.querySelector("[data-revert]") : null;
  const lineEnding = new Compartment();
  const host = document.createElement("div");
  host.className = "cm-host";
  textarea.parentNode.insertBefore(host, textarea.nextSibling);
  if (uploads) uploads.registerCurrent(host);

  let view;
  let applyingSource = false;
  let operationVersion = 0;
  let documentBeforeUpload = null;
  let dirtyBeforeUpload = false;
  let editedBeforeUpload = false;
  let editorDirty = false;
  let documentEdited = false;
  let sourceBytes = null;

  const setUploadStatus = (message) => {
    if (uploads) uploads.setStatus(message);
  };

  // There is nothing to revert to until something has been changed, and a
  // "discard changes" button offered the moment a save lands reads as though
  // the save did not take.
  const markDirty = () => {
    if (revert) revert.classList.add("show");
  };

  const updateReopenAvailability = () => {
    if (reopenEncoding) reopenEncoding.disabled = !sourceBytes && (!textarea.dataset.rawUrl || documentEdited);
  };

  // Selecting a file normally leaves its bytes untouched. Once the user
  // changes content, encoding, or line endings, however, the editor is the
  // source of truth and the form must submit its text instead of the file.
  const markContentForSave = (message, keepSourceBytes = false) => {
    editorDirty = true;
    markDirty();
    operationVersion += 1;
    if (!keepSourceBytes) sourceBytes = null;
    if (upload && upload.files.length > 0) {
      if (uploads) uploads.useEditor(message);
      else upload.value = "";
      documentBeforeUpload = null;
    }
    updateReopenAvailability();
    setUploadStatus(message);
  };

  const setEncoding = (encoding, valid = true) => {
    const canonical = normalizeEncoding(encoding);
    if (!encodingSelect || !canonical) return false;
    const option = Array.from(encodingSelect.options).find((item) => item.value === canonical);
    if (!option) return false;
    encodingSelect.value = canonical;
    encodingSelect.dataset.valid = valid ? "true" : "false";
    textarea.dataset.encoding = canonical;
    return true;
  };

  if (encodingSelect) {
    const initialEncoding = normalizeEncoding(textarea.dataset.encoding) || "utf-8";
    setEncoding(initialEncoding);
  }

  // The server detects the ending from the stored bytes and sends it here,
  // because by the time the body reaches this script it cannot be recovered:
  // the HTML parser turns every CRLF in the document into a bare LF.
  let currentEOL = normalizeEOLName(textarea.dataset.eol || (eolSelect ? eolSelect.value : "lf"));
  const setEOL = (name) => {
    currentEOL = normalizeEOLName(name);
    if (eolSelect) eolSelect.value = currentEOL;
    textarea.dataset.eol = currentEOL;
    if (view) {
      view.dispatch({ effects: lineEnding.reconfigure(EditorState.lineSeparator.of(EOL_SEQUENCES[currentEOL])) });
    }
  };

  try {
    view = new EditorView({
      parent: host,
      state: EditorState.create({
        doc: textarea.value,
        extensions: [
          lineNumbers(),
          highlightActiveLineGutter(),
          highlightSpecialChars(),
          history(),
          drawSelection(),
          dropCursor(),
          indentOnInput(),
          indentUnit.of("  "),
          lineEnding.of(EditorState.lineSeparator.of(EOL_SEQUENCES[currentEOL])),
          bracketMatching(),
          closeBrackets(),
          rectangularSelection(),
          crosshairCursor(),
          highlightActiveLine(),
          highlightSelectionMatches(),
          search({ top: true }),
          syntaxHighlighting(highlight),
          theme,
          EditorView.contentAttributes.of({
            "aria-label": textarea.getAttribute("aria-label") || "资源内容",
            "aria-multiline": "true",
            spellcheck: "false",
          }),
          EditorView.updateListener.of((update) => {
            if (!update.docChanged || applyingSource) return;
            documentEdited = true;
            markContentForSave("");
          }),
          language.of(languageFor(tokenFor(textarea.dataset.filename, textarea.dataset.contentType))),
          keymap.of([
            ...closeBracketsKeymap, ...defaultKeymap, ...searchKeymap,
            ...historyKeymap,
          ]),
        ],
      }),
    });
  } catch (error) {
    host.remove();
    throw error;
  }

  // Only now is the textarea taken out of view. Doing it here rather than in
  // the stylesheet means a thrown constructor leaves a working textarea behind.
  textarea.classList.add("cm-source");

  if (form) {
    // The editor reports its own edits through markContentForSave; this covers
    // everything else on the form - the name, the filename, the pickers and
    // the file input.
    form.addEventListener("input", markDirty);
    form.addEventListener("change", markDirty);
    // sliceDoc() joins with state.lineBreak, which the lineEnding compartment
    // owns. doc.toString() would hardcode LF and silently convert the whole
    // file - including every line the editor never touched.
    const sync = () => {
      textarea.value = view.state.sliceDoc();
    };
    form.addEventListener("submit", sync);
    form.addEventListener("formdata", (event) => {
      sync();
      if (!textarea.name) return;
      const source = submissionSource(
        Boolean(upload && upload.files.length > 0),
        Boolean(textarea.dataset.rawUrl),
        editorDirty,
      );
      if (source === "upload") {
        // The server deliberately gives an uploaded file precedence. Do not
        // send the editor preview as a second full copy of the same content.
        event.formData.delete(textarea.name);
        if (encodingSelect && encodingSelect.dataset.valid !== "true") {
          event.formData.delete(encodingSelect.name);
        }
      } else if (source === "stored") {
        // Keep the stored bytes byte-for-byte identical when fields outside
        // the editor change. The server interprets an absent content field on
        // an existing resource as "retain the current object".
        event.formData.delete(textarea.name);
      } else {
        // Encoding and line-ending changes make the editor authoritative too.
        // Deleting the file here is a second line of defence in case a browser
        // retains it after upload.value was cleared.
        if (upload && upload.name) event.formData.delete(upload.name);
        event.formData.set(textarea.name, textarea.value);
      }
    });
    const filename = form.elements.filename;
    const configureLanguage = () => {
      view.dispatch({ effects: language.reconfigure(languageFor(tokenFor(filename ? filename.value : "", textarea.dataset.contentType))) });
    };

    // The filename decides the type, and on a new resource it is typed after
    // the content just as often as before it.
    if (filename) {
      filename.addEventListener("input", () => {
        configureLanguage();
      });
    }

    const replaceDocument = (content, dirty = false, edited = false) => {
      applyingSource = true;
      try {
        view.dispatch({
          changes: { from: 0, to: view.state.doc.length, insert: content },
          selection: { anchor: 0 },
          scrollIntoView: true,
        });
        textarea.value = content;
        editorDirty = dirty;
        documentEdited = edited;
      } finally {
        applyingSource = false;
      }
    };

    const openBytes = (bytes, encoding) => {
      const content = decodeBytes(bytes, encoding);
      setEOL(detectEOL(content));
      replaceDocument(content);
      sourceBytes = bytes;
      setEncoding(encoding);
      return content;
    };

    const reopenWithEncoding = async (encoding) => {
      const version = ++operationVersion;
      let bytes = sourceBytes;

      if (!bytes && textarea.dataset.rawUrl) {
        setUploadStatus(`正在按 ${encoding} 重新解码…`);
        try {
          const response = await fetch(textarea.dataset.rawUrl, { cache: "no-store" });
          if (!response.ok) throw new Error(`HTTP ${response.status}`);
          bytes = new Uint8Array(await response.arrayBuffer());
        } catch (error) {
          if (version !== operationVersion) return;
          setEncoding(encoding, false);
          setUploadStatus(`无法读取原始文件，请稍后重试。`);
          console.error("editor: cannot read resource for decoding", error);
          return;
        }
      }
      if (version !== operationVersion || !bytes) return;

      try {
        openBytes(bytes, encoding);
        markContentForSave("", true);
      } catch (error) {
        if (version !== operationVersion) return;
        setEncoding(encoding, false);
        setUploadStatus(`无法按 ${encoding} 解码，请更换编码。`);
      }
    };

    if (eolSelect) {
      eolSelect.addEventListener("change", () => {
        setEOL(eolSelect.value);
        const label = currentEOL === "crlf" ? "CRLF" : "LF";
        markContentForSave("", true);
      });
    }

    if (encodingSelect) {
      encodingSelect.addEventListener("change", () => {
        const encoding = normalizeEncoding(encodingSelect.value);
        if (!encoding) return;
        setEncoding(encoding);
        markContentForSave("", true);
      });
    }

    if (reopenEncoding) {
      reopenEncoding.addEventListener("click", () => {
        const encoding = normalizeEncoding(encodingSelect ? encodingSelect.value : "");
        if (encoding) void reopenWithEncoding(encoding);
      });
      updateReopenAvailability();
    }

    if (uploads) {
      uploads.subscribe(async ({ file, kind, version, oversize }) => {
        operationVersion = Math.max(operationVersion + 1, version);

        if (!file) {
          if (documentBeforeUpload !== null) replaceDocument(documentBeforeUpload, dirtyBeforeUpload, editedBeforeUpload);
          documentBeforeUpload = null;
          sourceBytes = null;
          updateReopenAvailability();
          setUploadStatus("");
          return;
        }

        if (documentBeforeUpload === null) {
          documentBeforeUpload = view.state.doc.toString();
          dirtyBeforeUpload = editorDirty;
          editedBeforeUpload = documentEdited;
        } else {
          // A second selection replaces the first preview, not the content
          // that was in the editor before file selection began.
          replaceDocument(documentBeforeUpload, dirtyBeforeUpload, editedBeforeUpload);
        }

        if (oversize) {
          sourceBytes = null;
          editorDirty = false;
          documentEdited = false;
          updateReopenAvailability();
          return;
        }

        if (kind !== "text" && kind !== "unknown") {
          sourceBytes = null;
          editorDirty = false;
          documentEdited = false;
          updateReopenAvailability();
          return;
        }

        let bytes;
        try {
          bytes = await file.arrayBuffer();
        } catch (error) {
          if (!uploads.current(version, file)) return;
          sourceBytes = null;
          editorDirty = false;
          documentEdited = false;
          updateReopenAvailability();
          uploads.showFile(file, `无法读取 ${file.name}，请重新选择文件。`);
          console.error("editor: cannot read uploaded file", error);
          return;
        }
        if (!uploads.current(version, file)) return;

        const source = new Uint8Array(bytes);
        sourceBytes = source;
        updateReopenAvailability();
        const encoding = detectFileEncoding(source);
        if (!encoding) {
          replaceDocument(documentBeforeUpload, dirtyBeforeUpload, editedBeforeUpload);
          editorDirty = false;
          documentEdited = false;
          if (encodingSelect) encodingSelect.dataset.valid = "false";
          if (kind === "text") {
            uploads.showText(file, `无法自动识别 ${file.name} 的编码，请选择编码后重新解码。`);
          } else {
            uploads.showFile(file);
          }
          return;
        }

        try {
          openBytes(source, encoding);
        } catch (error) {
          replaceDocument(documentBeforeUpload, dirtyBeforeUpload, editedBeforeUpload);
          editorDirty = false;
          documentEdited = false;
          if (encodingSelect) encodingSelect.dataset.valid = "false";
          if (kind === "text") {
            uploads.showText(file, `${file.name} 无法按 ${encoding} 解码，请选择编码后重新解码。`);
          } else {
            uploads.showFile(file);
          }
          return;
        }

        uploads.showText(file);
      });
    }
  }
}

// A remote resource has no editable body - the bytes live upstream and are
// fetched fresh for the page - but it reads like one, so it gets the same
// surface with editing taken out: line numbers, highlighting and search stay,
// the document cannot be changed, and nothing is wired to the form. Kept apart
// from enhance() because every path in there is about producing bytes to post.
function preview(textarea) {
  const host = document.createElement("div");
  host.className = "cm-host";
  textarea.parentNode.insertBefore(host, textarea.nextSibling);

  new EditorView({
    parent: host,
    state: EditorState.create({
      doc: textarea.value,
      extensions: [
        lineNumbers(),
        highlightSpecialChars(),
        drawSelection(),
        EditorState.readOnly.of(true),
        EditorView.editable.of(false),
        // A preview answers "what is at this address", so it matches what a
        // browser shows rather than what an editor shows: a long line wraps
        // instead of scrolling off the side. The editor keeps the editor
        // convention and does not wrap.
        EditorView.lineWrapping,
        highlightSelectionMatches(),
        search({ top: true }),
        syntaxHighlighting(highlight),
        theme,
        languageFor(tokenFor(textarea.dataset.filename, textarea.dataset.contentType)),
        keymap.of(searchKeymap),
        EditorView.contentAttributes.of({
          "aria-label": textarea.getAttribute("aria-label") || "资源内容",
          "aria-readonly": "true",
          spellcheck: "false",
        }),
      ],
    }),
  });

  textarea.classList.add("cm-source");
}

for (const textarea of document.querySelectorAll("textarea[data-editor]")) {
  try {
    enhance(textarea);
  } catch (error) {
    // One editor failing must not take the page with it: the textarea it was
    // meant to replace is still there and still posts.
    console.error("editor: falling back to the plain textarea", error);
  }
}

for (const textarea of document.querySelectorAll("textarea[data-preview]")) {
  try {
    preview(textarea);
  } catch (error) {
    // Same rule: the disabled textarea behind it already shows the content.
    console.error("editor: falling back to the plain preview", error);
  }
}

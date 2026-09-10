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

function sizeText(size) {
  if (size >= 1024 * 1024) return `${(size / (1024 * 1024)).toFixed(2)} MiB`;
  if (size >= 1024) return `${(size / 1024).toFixed(2)} KiB`;
  return `${size} B`;
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
export { decodeBytes, detectFileEncoding, SUPPORTED_ENCODINGS };

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
const theme = EditorView.theme({
  "&": { border: "1px solid var(--line2)", background: "var(--panel)", color: "var(--ink)", height: "200px", maxHeight: "60vh", overflow: "hidden" },
  "&.cm-focused": { outline: "none", borderColor: "var(--accent)" },
  ".cm-scroller": { fontFamily: "var(--mono)", fontSize: "12px", lineHeight: "1.65", minHeight: 0, overflow: "auto" },
  ".cm-content": { minHeight: "100%", padding: "6px 0" },
  ".cm-line": { padding: "0 7px" },
  ".cm-gutters": { alignSelf: "stretch", background: "var(--head)", border: "none", borderRight: "1px solid var(--line)", color: "var(--ink3)", fontSize: "11px" },
  ".cm-activeLine": { background: "var(--zebra)" },
  ".cm-activeLineGutter": { background: "var(--zebra)", color: "var(--ink2)" },
  ".cm-cursor": { borderLeftColor: "var(--ink)" },
  ".cm-selectionBackground, .cm-content ::selection": { background: "var(--accent-s)" },
  "&.cm-focused .cm-selectionBackground": { background: "var(--accent-s)" },
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
  const uploadStatus = form ? form.querySelector("[data-upload-status]") : null;
  const encodingSelect = form ? form.querySelector("[data-encoding-select]") : null;
  const host = document.createElement("div");
  host.className = "cm-host";
  textarea.parentNode.insertBefore(host, textarea.nextSibling);

  let view;
  let applyingSource = false;
  let operationVersion = 0;
  let documentBeforeUpload = null;
  let dirtyBeforeUpload = false;
  let editorDirty = false;
  let sourceBytes = null;

  const setUploadStatus = (message) => {
    if (uploadStatus) uploadStatus.textContent = message;
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

            editorDirty = true;
            sourceBytes = null;
            operationVersion += 1;
            if (!upload || upload.files.length === 0) return;

            // Once the uploaded preview is edited, the editor becomes the
            // source of truth. Clearing the file input keeps the server's
            // existing "uploaded file wins" rule from discarding those edits.
            upload.value = "";
            documentBeforeUpload = null;
            setUploadStatus(`已采用编辑器中的修改，保存时将按 ${encodingSelect ? encodingSelect.value : "utf-8"} 编码写入。`);
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
    const sync = () => {
      textarea.value = view.state.doc.toString();
    };
    form.addEventListener("submit", sync);
    form.addEventListener("formdata", (event) => {
      sync();
      if (!textarea.name) return;
      if (upload && upload.files.length > 0) {
        // The server deliberately gives an uploaded file precedence. Do not
        // send the editor preview as a second full copy of the same content.
        event.formData.delete(textarea.name);
        if (encodingSelect && encodingSelect.dataset.valid !== "true") {
          event.formData.delete(encodingSelect.name);
        }
      } else if (textarea.dataset.rawUrl && !editorDirty) {
        // Keep the stored bytes byte-for-byte identical when only metadata or
        // the chosen decoding changes. The server interprets an absent content
        // field on an existing resource as "retain the current object".
        event.formData.delete(textarea.name);
      } else {
        event.formData.set(textarea.name, textarea.value);
      }
    });
    const filename = form.elements.filename;
    let suggestedFilename = "";
    const configureLanguage = () => {
      view.dispatch({ effects: language.reconfigure(languageFor(tokenFor(filename ? filename.value : "", textarea.dataset.contentType))) });
    };

    // The filename decides the type, and on a new resource it is typed after
    // the content just as often as before it.
    if (filename) {
      filename.addEventListener("input", () => {
        if (filename.value !== suggestedFilename) suggestedFilename = "";
        configureLanguage();
      });
    }

    const replaceDocument = (content, dirty = false) => {
      applyingSource = true;
      try {
        view.dispatch({
          changes: { from: 0, to: view.state.doc.length, insert: content },
          selection: { anchor: 0 },
          scrollIntoView: true,
        });
        textarea.value = content;
        editorDirty = dirty;
      } finally {
        applyingSource = false;
      }
    };

    const openBytes = (bytes, encoding) => {
      const content = decodeBytes(bytes, encoding);
      replaceDocument(content);
      sourceBytes = bytes;
      setEncoding(encoding);
      return content;
    };

    if (encodingSelect) {
      encodingSelect.addEventListener("change", async () => {
        const encoding = normalizeEncoding(encodingSelect.value);
        if (!encoding) return;
        const version = ++operationVersion;

        if (sourceBytes) {
          try {
            openBytes(sourceBytes, encoding);
            setUploadStatus(`已按 ${encoding} 重新打开，可以继续编辑。`);
          } catch (error) {
            encodingSelect.dataset.valid = "false";
            setUploadStatus(`无法按 ${encoding} 解码，请选择其他编码。`);
          }
          return;
        }

        if (textarea.dataset.rawUrl && !editorDirty) {
          setUploadStatus(`正在按 ${encoding} 重新打开…`);
          try {
            const response = await fetch(textarea.dataset.rawUrl, { cache: "no-store" });
            if (!response.ok) throw new Error(`HTTP ${response.status}`);
            const bytes = new Uint8Array(await response.arrayBuffer());
            if (version !== operationVersion || editorDirty) return;
            openBytes(bytes, encoding);
            setUploadStatus(`已按 ${encoding} 重新打开，可以继续编辑。`);
          } catch (error) {
            if (version !== operationVersion) return;
            encodingSelect.dataset.valid = "false";
            setUploadStatus(`无法按 ${encoding} 重新打开，请选择其他编码。`);
            console.error("editor: cannot reopen resource with encoding", error);
          }
          return;
        }

        setEncoding(encoding);
        setUploadStatus(`保存时将按 ${encoding} 编码写入。`);
      });
    }

    if (upload) {
      upload.addEventListener("change", async () => {
        const version = ++operationVersion;
        const file = upload.files[0];

        if (!file) {
          if (documentBeforeUpload !== null) replaceDocument(documentBeforeUpload, dirtyBeforeUpload);
          documentBeforeUpload = null;
          sourceBytes = null;
          setUploadStatus("");
          return;
        }

        if (documentBeforeUpload === null) {
          documentBeforeUpload = view.state.doc.toString();
          dirtyBeforeUpload = editorDirty;
        } else {
          // A second selection replaces the first preview, not the content
          // that was in the editor before file selection began.
          replaceDocument(documentBeforeUpload, dirtyBeforeUpload);
        }

        if (filename && (filename.value.trim() === "" || filename.value === suggestedFilename)) {
          filename.value = file.name;
          suggestedFilename = file.name;
          configureLanguage();
        }

        const maxBytes = Number(upload.dataset.maxBytes || 0);
        if (maxBytes > 0 && file.size > maxBytes) {
          sourceBytes = null;
          setUploadStatus(`文件大小为 ${sizeText(file.size)}，超过 ${sizeText(maxBytes)} 上限。`);
          return;
        }

        setUploadStatus(`正在读取 ${file.name}…`);
        let bytes;
        try {
          bytes = await file.arrayBuffer();
        } catch (error) {
          if (version !== operationVersion) return;
          sourceBytes = null;
          setUploadStatus(`无法读取 ${file.name}，请重新选择文件。`);
          console.error("editor: cannot read uploaded file", error);
          return;
        }
        if (version !== operationVersion || upload.files[0] !== file) return;

        const source = new Uint8Array(bytes);
        sourceBytes = source;
        const encoding = detectFileEncoding(source);
        if (!encoding) {
          replaceDocument(documentBeforeUpload, dirtyBeforeUpload);
          if (encodingSelect) encodingSelect.dataset.valid = "false";
          setUploadStatus(`${file.name} 未能自动识别为文本；可从编码列表手动选择，或按原始文件上传。`);
          return;
        }

        try {
          openBytes(source, encoding);
        } catch (error) {
          replaceDocument(documentBeforeUpload, dirtyBeforeUpload);
          if (encodingSelect) encodingSelect.dataset.valid = "false";
          setUploadStatus(`${file.name} 无法按检测到的 ${encoding} 解码；可手动选择其他编码。`);
          return;
        }

        setUploadStatus(`已自动按 ${encoding} 打开 ${file.name}（${sizeText(file.size)}），可以修改编码或内容。`);
      });
    }
  }
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

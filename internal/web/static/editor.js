// The content editor.
//
// This is progressive enhancement, not a replacement: the <textarea> stays in
// the DOM, keeps its name, and is what the form posts. CodeMirror is mounted
// beside it and its document is copied back on submit. If this module fails to
// load, throws, or is blocked, the page is exactly what it was before - a
// textarea in a form - which is the behaviour every no-JS path depends on.
import {
  bracketMatching, closeBrackets, closeBracketsKeymap, Compartment,
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
  const host = document.createElement("div");
  host.className = "cm-host";
  textarea.parentNode.insertBefore(host, textarea.nextSibling);

  let view;
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
      if (textarea.name) event.formData.set(textarea.name, textarea.value);
    });
    // The filename decides the type, and on a new resource it is typed after
    // the content just as often as before it.
    const filename = form.elements.filename;
    if (filename) {
      filename.addEventListener("input", () => {
        view.dispatch({ effects: language.reconfigure(languageFor(tokenFor(filename.value, textarea.dataset.contentType))) });
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

// The editor surface PlainMote uses, bundled into one file.
// Regenerate with tools/codemirror/build.sh.
//
// basicSetup is deliberately not used: it pulls in autocompletion and linting,
// and this editor has no vocabulary to complete and no linter to run. Every
// extension below is picked by hand and listed in editor.js.
export { EditorState, Compartment } from "@codemirror/state";
export {
  EditorView, keymap, lineNumbers, highlightActiveLine,
  highlightActiveLineGutter, drawSelection, rectangularSelection,
  crosshairCursor, highlightSpecialChars, dropCursor,
} from "@codemirror/view";
export {
  defaultKeymap, history, historyKeymap, indentMore, indentLess,
} from "@codemirror/commands";
export {
  syntaxHighlighting, HighlightStyle, indentUnit, bracketMatching,
  indentOnInput, StreamLanguage,
} from "@codemirror/language";
export { closeBrackets, closeBracketsKeymap } from "@codemirror/autocomplete";
export { search, searchKeymap, highlightSelectionMatches } from "@codemirror/search";
export { tags } from "@lezer/highlight";

// The formats this service actually carries. Extensions map to these in
// editor.js; anything unrecognised gets no language and stays plain text.
export { yaml } from "@codemirror/lang-yaml";
export { json } from "@codemirror/lang-json";
export { xml } from "@codemirror/lang-xml";
export { toml } from "@codemirror/legacy-modes/mode/toml";
export { properties } from "@codemirror/legacy-modes/mode/properties";
export { shell } from "@codemirror/legacy-modes/mode/shell";

// Encoding detection runs on the original Uint8Array before any browser text
// decoder can replace bytes. The UI filters these ranked candidates through
// the encodings it can also save losslessly on the Go side.
import jschardet from "jschardet";
export const analyseEncoding = (bytes) => {
  let input = "";
  for (let offset = 0; offset < bytes.length; offset += 0x8000) {
    input += String.fromCharCode(...bytes.subarray(offset, offset + 0x8000));
  }
  return jschardet.detectAll(input).map((candidate) => ({
    confidence: candidate.confidence * 100,
    name: candidate.encoding,
  }));
};

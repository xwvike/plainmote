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
  defaultKeymap, history, historyKeymap,
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

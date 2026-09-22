// File selection is independent from the text editor. This module stays small
// enough to load on every local-resource page: it owns filename suggestion,
// size feedback and previews that do not need to read the file into JavaScript.
// The editor subscribes only when a selected file may contain text.

const MEDIA_EXTENSIONS = {
  png: "image", jpg: "image", jpeg: "image", gif: "image", webp: "image",
  avif: "image", bmp: "image", ico: "image",
  mp3: "audio", m4a: "audio", aac: "audio", ogg: "audio", opus: "audio",
  flac: "audio", wav: "audio",
  mp4: "video", webm: "video", mov: "video",
};

const MEDIA_TYPES = {
  "image/png": "image", "image/jpeg": "image", "image/gif": "image",
  "image/webp": "image", "image/avif": "image", "image/bmp": "image",
  "image/x-icon": "image", "image/vnd.microsoft.icon": "image",
  "audio/mpeg": "audio", "audio/mp4": "audio", "audio/aac": "audio",
  "audio/ogg": "audio", "audio/flac": "audio", "audio/wav": "audio",
  "audio/x-wav": "audio", "video/mp4": "video", "video/webm": "video",
  "video/quicktime": "video",
};

const TEXT_EXTENSIONS = new Set([
  "txt", "text", "md", "log", "conf", "cfg", "ini", "env", "list",
  "rules", "properties", "pem", "crt", "key", "pub", "sh", "service",
  "json", "yaml", "yml", "toml", "xml", "csv",
]);

const FILE_EXTENSIONS = new Set(["pdf", "zip", "gz"]);
const UNSAFE_EXTENSIONS = new Set(["html", "htm", "js", "mjs", "cjs", "svg", "xhtml"]);
const TEXT_TYPES = new Set([
  "application/json", "application/yaml", "application/toml", "application/xml",
  "text/csv", "text/plain",
]);
const UNSAFE_TYPES = ["html", "javascript", "ecmascript", "svg", "xhtml"];

function extensionOf(name) {
  const value = String(name || "");
  const dot = value.lastIndexOf(".");
  return dot >= 0 ? value.slice(dot + 1).toLowerCase() : "";
}

// The extension list mirrors the server's explicit content-type list. Unknown
// files remain candidates so the editor can still recognise extensionless and
// legacy-encoded text; if it cannot, they fall back to an ordinary file card.
export function classifyUpload(file) {
  const extension = extensionOf(file && file.name);
  if (MEDIA_EXTENSIONS[extension]) return MEDIA_EXTENSIONS[extension];
  if (TEXT_EXTENSIONS.has(extension)) return "text";
  if (FILE_EXTENSIONS.has(extension) || UNSAFE_EXTENSIONS.has(extension)) return "file";

  const type = String(file && file.type || "").split(";", 1)[0].trim().toLowerCase();
  if (UNSAFE_TYPES.some((word) => type.includes(word))) return "file";
  if (MEDIA_TYPES[type]) return MEDIA_TYPES[type];
  if (TEXT_TYPES.has(type) || type.startsWith("text/")) return "text";
  if (type && type !== "application/octet-stream") return "file";
  return "unknown";
}

export function sizeText(size) {
  if (size >= 1024 * 1024) return `${(size / (1024 * 1024)).toFixed(2)} MiB`;
  if (size >= 1024) return `${(size / 1024).toFixed(2)} KiB`;
  return `${size} B`;
}

const controllers = new WeakMap();

function message(name, values = {}) {
  let text = document.body.dataset[name] || "";
  for (const [key, value] of Object.entries(values)) text = text.replaceAll(`{${key}}`, value);
  return text;
}

// Metadata alone gives the controls a duration, but browsers are allowed to
// leave the video surface blank until playback starts. A tiny seek asks for
// the first decodable frame without autoplaying or preloading the whole file.
export function primeVideo(video) {
  if (!video) return;
  video.preload = "metadata";
  let primed = false;
  const seekFirstFrame = () => {
    if (primed || !Number.isFinite(video.duration) || video.duration <= 0) return;
    primed = true;
    try {
      video.currentTime = Math.min(0.001, video.duration / 2);
    } catch (_) {
      primed = false;
    }
  };
  video.addEventListener("loadedmetadata", seekFirstFrame, { once: true });
  if (video.readyState >= 1) seekFirstFrame();
  else video.load();
}

function enhanceUpload(input) {
  const form = input.form;
  const body = form ? form.querySelector("[data-content-body]") : null;
  const preview = body ? body.querySelector("[data-upload-preview]") : null;
  const status = form ? form.querySelector("[data-upload-status]") : null;
  const textControls = form ? form.querySelector("[data-text-controls]") : null;
  const contentMeta = form ? form.querySelector("[data-content-meta]") : null;
  const filename = form && form.elements ? form.elements.namedItem("filename") : null;
  const initialControlsHidden = textControls ? textControls.hidden : true;
  const initialMeta = contentMeta ? contentMeta.textContent : "";
  const listeners = new Set();
  let objectURL = "";
  let suggestedFilename = "";
  let version = 0;
  let mode = "original";

  const currentContent = () => body ? body.querySelectorAll("[data-current-content]") : [];
  const setStatus = (message) => {
    if (status) status.textContent = message || "";
  };
  const releaseObjectURL = () => {
    if (!objectURL) return;
    URL.revokeObjectURL(objectURL);
    objectURL = "";
  };
  const setCurrentVisible = (visible) => {
    for (const element of currentContent()) element.hidden = !visible;
  };
  const setTextControlsVisible = (visible) => {
    if (textControls) textControls.hidden = !visible;
  };
  const updateMeta = (file) => {
    if (!contentMeta || !file) return;
    const type = file.type || "application/octet-stream";
    contentMeta.textContent = `${type} · ${sizeText(file.size)}`;
  };
  const clearPreview = () => {
    releaseObjectURL();
    if (preview) {
      preview.replaceChildren();
      preview.hidden = true;
    }
  };
  const showOriginal = () => {
    mode = "original";
    clearPreview();
    setCurrentVisible(true);
    if (textControls) textControls.hidden = initialControlsHidden;
    if (contentMeta) contentMeta.textContent = initialMeta;
  };
  const fileCard = (file) => {
    const card = document.createElement("div");
    card.className = "file-card";
    const name = document.createElement("strong");
    name.textContent = file.name;
    const detail = document.createElement("span");
    detail.textContent = `${file.type || "application/octet-stream"} · ${sizeText(file.size)}`;
    card.append(name, detail);
    return card;
  };
  const showFile = (file, message = "") => {
    if (!file || !preview) return;
    mode = "file";
    clearPreview();
    setCurrentVisible(false);
    setTextControlsVisible(false);
    preview.append(fileCard(file));
    preview.hidden = false;
    updateMeta(file);
    setStatus(message);
  };
  const showMedia = (file, kind) => {
    if (!file || !preview) return;
    mode = kind;
    clearPreview();
    setCurrentVisible(false);
    setTextControlsVisible(false);
    objectURL = URL.createObjectURL(file);
    let media;
    if (kind === "image") {
      media = document.createElement("img");
      media.alt = file.name;
    } else if (kind === "audio") {
      media = document.createElement("audio");
      media.controls = true;
      media.preload = "metadata";
      media.setAttribute("aria-label", file.name);
    } else {
      media = document.createElement("video");
      media.controls = true;
      media.preload = "metadata";
      media.setAttribute("aria-label", file.name);
    }
    media.className = `resource-media resource-${kind}`;
    media.src = objectURL;
    preview.append(media);
    preview.hidden = false;
    if (kind === "video") primeVideo(media);
    updateMeta(file);
    setStatus("");
  };
  const showText = (file, message = "") => {
    mode = "text";
    clearPreview();
    setCurrentVisible(true);
    setTextControlsVisible(true);
    if (file) updateMeta(file);
    else if (contentMeta) contentMeta.textContent = initialMeta;
    setStatus(message);
  };

  const notify = (selection) => {
    for (const listener of listeners) listener(selection);
  };

  const select = () => {
    const currentVersion = ++version;
    const file = input.files && input.files[0];
    if (!file) {
      showOriginal();
      setStatus("");
      notify({ file: null, kind: "original", version: currentVersion, oversize: false });
      return;
    }

    if (filename && (filename.value.trim() === "" || filename.value === suggestedFilename)) {
      filename.value = file.name;
      suggestedFilename = file.name;
      filename.dispatchEvent(new Event("input", { bubbles: true }));
    }

    const maxBytes = Number(input.dataset.maxBytes || 0);
    const oversize = maxBytes > 0 && file.size > maxBytes;
    const kind = classifyUpload(file);
    if (kind === "image" || kind === "audio" || kind === "video") {
      showMedia(file, kind);
    } else if (kind === "file" || listeners.size === 0) {
      showFile(file);
    } else {
      showText(file, message("msgUploadReading", { name: file.name }));
    }
    if (oversize) {
      setStatus(message("msgUploadOversize", { size: sizeText(file.size), limit: sizeText(maxBytes) }));
    }
    notify({ file, kind, version: currentVersion, oversize });
  };

  const controller = {
    subscribe(listener) {
      listeners.add(listener);
      return () => listeners.delete(listener);
    },
    current(selectionVersion, file) {
      return selectionVersion === version && input.files && input.files[0] === file;
    },
    registerCurrent(element) {
      element.dataset.currentContent = "";
      element.hidden = mode !== "original" && mode !== "text";
    },
    showText,
    showFile,
    setStatus,
    useEditor(message) {
      input.value = "";
      version += 1;
      showText(null, message || "");
    },
  };
  controllers.set(input, controller);
  input.addEventListener("change", select);
  return controller;
}

export function uploadController(input) {
  if (!input) return null;
  return controllers.get(input) || enhanceUpload(input);
}

if (typeof document !== "undefined") {
  for (const input of document.querySelectorAll("input[data-upload]")) uploadController(input);
  for (const video of document.querySelectorAll("video[data-prime-video]")) primeVideo(video);
}

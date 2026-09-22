// Fetch once, then let the native player seek and replay a local Blob. Never
// retry automatically: every request for the original bytes consumes a use.
const container = document.querySelector("[data-media-source]");
let objectURL = "";

function filenameFrom(response) {
  const disposition = response.headers.get("Content-Disposition") || "";
  const encoded = disposition.match(/filename\*=UTF-8''([^;]+)/i);
  if (encoded) {
    try { return decodeURIComponent(encoded[1]); } catch (_) { /* use quoted name */ }
  }
  const quoted = disposition.match(/filename="((?:\\.|[^"\\])*)"/i);
  return quoted ? quoted[1].replace(/\\(.)/g, "$1") : "media";
}

async function loadMedia() {
  try {
    const response = await fetch(container.dataset.mediaSource, {
      cache: "no-store", credentials: "same-origin", redirect: "error",
    });
    if (!response.ok) {
      throw new Error(response.status === 401 || response.status === 404
        ? container.dataset.linkExpired : container.dataset.loadFailed);
    }
    const blob = await response.blob();
    const filename = filenameFrom(response);
    const kind = blob.type.startsWith("audio/") ? "audio" : blob.type.startsWith("video/") ? "video" : "";
    if (!kind) throw new Error(container.dataset.typeChanged);
    objectURL = URL.createObjectURL(blob);
    document.title = filename;
    const player = document.createElement(kind);
    player.controls = true;
    player.preload = "auto";
    player.playsInline = true;
    player.src = objectURL;
    player.addEventListener("error", () => {
      const message = document.createElement("p");
      message.append(container.dataset.unsupported);
      const download = document.createElement("a");
      download.href = objectURL;
      download.download = filename;
      download.textContent = container.dataset.download;
      message.append(download);
      container.replaceChildren(message);
    }, { once: true });
    container.replaceChildren(player);
  } catch (error) {
    container.querySelector("[role=status]").textContent = error instanceof TypeError
      ? container.dataset.networkFailed : error.message;
  }
}

window.addEventListener("pagehide", (event) => {
  if (!event.persisted && objectURL) URL.revokeObjectURL(objectURL);
});

loadMedia();

// Direct-to-object-storage presigned PUT uploads with XHR progress reporting
// and transparent fallback to the application server.
//
// Files at or above DIRECT_THRESHOLD (256 KiB) ask POST /uploads/presign for a
// short-lived presigned PUT URL and signed upload ticket, PUT the bytes straight
// to R2/S3 (avoiding double-hop tunneling through the web server), and then send
// only the small metadata commit with upload_token. Smaller files - or any
// environment where direct PUT is unavailable or blocked by CORS - upload via
// XHR straight to the existing PlainMote endpoint with the same live progress.

export const DIRECT_THRESHOLD = 256 * 1024;
export const UPLOAD_JSON_HEADER = "X-PlainMote-Upload";

let directDisabled = false;

export function canDirectUpload() {
  return !directDisabled && typeof document !== "undefined" && document.body?.dataset?.directUpload === "1";
}

export function readCSRF(form) {
  const fromField = form?.elements?.namedItem?.("csrf")?.value;
  if (fromField) return fromField;
  if (form?.dataset?.csrf) return form.dataset.csrf;
  return document.querySelector('meta[name="plainmote-csrf"]')?.content || "";
}

// Compute SHA-256 hex digest in browser WebCrypto in parallel with presigning.
export async function sha256Hex(blobOrBytes) {
  try {
    if (!globalThis.crypto?.subtle) return "";
    const buffer = blobOrBytes instanceof Uint8Array
      ? blobOrBytes
      : await blobOrBytes.arrayBuffer();
    const digest = new Uint8Array(await globalThis.crypto.subtle.digest("SHA-256", buffer));
    let hex = "";
    for (let i = 0; i < digest.length; i++) {
      hex += digest[i].toString(16).padStart(2, "0");
    }
    return hex;
  } catch {
    return "";
  }
}

export function xhrSend(method, url, body, { headers = {}, withCredentials = false, onProgress = null } = {}) {
  return new Promise((resolve, reject) => {
    const xhr = new XMLHttpRequest();
    xhr.open(method, url, true);
    xhr.withCredentials = withCredentials;
    for (const [k, v] of Object.entries(headers)) xhr.setRequestHeader(k, v);
    if (onProgress && xhr.upload) {
      xhr.upload.addEventListener("progress", (event) => {
        if (!event.lengthComputable || event.total <= 0) return;
        const percent = Math.min(99, Math.max(0, Math.round((event.loaded * 100) / event.total)));
        onProgress({ loaded: event.loaded, total: event.total, percent });
      });
    }
    xhr.addEventListener("load", () => {
      resolve({
        status: xhr.status,
        ok: xhr.status >= 200 && xhr.status < 300,
        responseText: xhr.responseText || "",
        responseURL: xhr.responseURL || "",
      });
    });
    xhr.addEventListener("error", () => reject(new TypeError("network error")));
    xhr.addEventListener("abort", () => reject(new TypeError("upload aborted")));
    xhr.send(body);
  });
}

// Attempts a direct presigned PUT upload. Returns { token, resourceID, sha256 }
// on success, returns null when the caller should fall back to uploading through
// the web server, or throws an Error for user-facing refusals (quota, rate limit,
// file too large, expired session).
export async function presignAndUpload({ purpose, blob, resourceID = "", csrf = "", onProgress = null }) {
  if (!canDirectUpload() || !blob || blob.size < DIRECT_THRESHOLD) return null;

  const shaPromise = sha256Hex(blob);
  let presign;
  try {
    const response = await fetch("/uploads/presign", {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", Accept: "application/json" },
      body: JSON.stringify({ purpose, size: blob.size, resource_id: resourceID, csrf }),
    });
    if (response.status === 501) {
      directDisabled = true;
      return null;
    }
    const data = await response.json().catch(() => ({}));
    if (!response.ok) {
      if (data && data.message) throw new Error(data.message);
      if (response.status >= 400 && response.status < 500) return null;
      return null;
    }
    presign = data;
  } catch (err) {
    if (err instanceof Error && !(err instanceof TypeError)) throw err;
    return null;
  }

  if (!presign || !presign.url || !presign.token) return null;

  try {
    const put = await xhrSend("PUT", presign.url, blob, { withCredentials: false, onProgress });
    if (!put.ok) {
      // Most commonly R2 bucket CORS is not configured yet or an edge firewall
      // blocks direct storage access; remember and fall back seamlessly.
      if (put.status === 0 || put.status === 403) directDisabled = true;
      return null;
    }
  } catch {
    directDisabled = true;
    return null;
  }

  const sha256 = await shaPromise;
  return { token: presign.token, resourceID: presign.resource_id || resourceID, sha256 };
}

// Manages button text and a quiet hairline <progress> bar inside a form's
// .savebar while an upload is in flight.
export function createUploadProgress(form, button) {
  const originalLabel = button ? button.textContent : "";
  const tplUploading = document.body?.dataset?.msgUploading || "Uploading %d%%…";
  const msgSaving = document.body?.dataset?.msgSaving || "Saving…";
  const barHost = form?.querySelector(".savebar") || form;
  let bar = null;

  const ensureBar = () => {
    if (bar || !barHost) return;
    bar = document.createElement("progress");
    bar.className = "upload-prog";
    bar.max = 100;
    bar.value = 0;
    barHost.append(bar);
  };

  return {
    setPhase(text) {
      if (button && text) button.textContent = text;
    },
    setProgress({ percent }) {
      ensureBar();
      if (bar) bar.value = percent;
      if (button) button.textContent = tplUploading.replace("%d%%", `${percent}%`).replace("%d", `${percent}`);
    },
    setSaving() {
      ensureBar();
      if (bar) bar.value = 100;
      if (button) button.textContent = msgSaving;
    },
    reset() {
      if (bar) {
        bar.remove();
        bar = null;
      }
      if (button) button.textContent = originalLabel;
    },
  };
}

// An export with encrypted resources in it, decrypted here: the archive the
// service builds holds them as ciphertext with sealed.json beside them; once
// unlocked, this takes that archive apart, decrypts each encrypted content
// under the name it really has, and hands over the archive as it would have
// been without encryption. Locked, the service's archive downloads as it is.
import { readZip, writeZip } from "./zip.js";
import { openContentKey, decryptContent, decryptMeta } from "./seal.js";
import { fromBase64URL } from "./e2ee.js";
import { accountKey } from "./unlock.js";


// One path segment, as the service makes them.
function segment(value) {
  const name = value.trim().replace(/[\u0000-\u001f\u007f/\\]/g, "_");
  return name === "." || name === ".." ? name.replace(/\./g, "_") : name;
}

function fileName(meta) {
  const name = segment(meta.filename || "");
  if (name) return name;
  const base = segment(meta.name || "").replace(/^[ .]+|[ .]+$/g, "") || "content";
  return /^text\//.test(meta.type) || /(json|xml|yaml|toml)/.test(meta.type) ? `${base}.txt` : base;
}

export async function decryptArchive(key, bytes) {
  const files = await readZip(bytes);
  const manifest = files.find((file) => file.name === "sealed.json");
  if (!manifest) return null;
  const sealed = JSON.parse(new TextDecoder().decode(manifest.data)).resources || [];
  const byPath = new Map(files.map((file) => [file.name, file]));
  const replaced = new Set(["sealed.json"]);
  const out = [];
  for (const resource of sealed) {
    const contentKey = await openContentKey(fromBase64URL(resource.sealed_key), key, resource.id);
    const sides = [{ path: `files/${resource.id}/content.sealed`, meta: resource.sealed_meta, dir: `files/${resource.id}/` }]
      .concat(resource.versions.map((v) => ({ path: `files/${resource.id}/versions/v${v.n}/content.sealed`, meta: v.sealed_meta, dir: `files/${resource.id}/versions/v${v.n}/` })));
    for (const side of sides) {
      const file = byPath.get(side.path);
      if (!file) continue;
      const meta = await decryptMeta(contentKey, resource.id, fromBase64URL(side.meta || resource.sealed_meta));
      out.push({ name: side.dir + fileName(meta), data: await decryptContent(contentKey, resource.id, file.data), modified: file.modified });
      replaced.add(side.path);
    }
  }
  return writeZip(files.filter((file) => !replaced.has(file.name)).concat(out));
}

function save(blob, name) {
  const link = document.createElement("a");
  link.href = URL.createObjectURL(blob);
  link.download = name;
  document.body.append(link);
  link.click();
  link.remove();
  setTimeout(() => URL.revokeObjectURL(link.href), 60000);
}

async function start() {
  const form = document.querySelector('form[action="/account/export"][data-sealed]');
  if (!form || !globalThis.crypto?.subtle || typeof DecompressionStream !== "function") return;
  const note = form.parentElement.querySelector("[data-export-note]");
  const key = await accountKey();
  if (note) note.textContent = key ? form.dataset.msgUnlocked : form.dataset.msgLocked;
  if (!key) return;
  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const button = form.querySelector("button");
    button.disabled = true;
    try {
      const response = await fetch(form.action, { method: "POST", body: new FormData(form), credentials: "same-origin" });
      if (!response.ok || !(response.headers.get("Content-Type") || "").includes("zip")) {
        // A refusal spends no export: sent again the ordinary way, it comes
        // back as the page that says why.
        form.submit();
        return;
      }
      const name = (/filename="([^"]+)"/.exec(response.headers.get("Content-Disposition") || "") || [])[1] || "plainmote.zip";
      const bytes = new Uint8Array(await response.arrayBuffer());
      const decrypted = await decryptArchive(key, bytes);
      save(decrypted || new Blob([bytes], { type: "application/zip" }), name);
    } catch {
      if (note) note.textContent = form.dataset.msgFailed;
    }
    button.disabled = false;
  });
}

if (typeof document !== "undefined") start();

// An encrypted resource's formats, in WebCrypto (docs/encryption.md).
//
//   content key CK   random AES-256-GCM, one per resource
//   content          "PMr1" | IV (12) | ciphertext and tag
//                    additional data "PMr1" + "pm/content/v1:<resource id>"
//   metadata         {"n": name, "f": filename, "t": type}, as JSON, in the
//                    same shape with "PMm1" and "pm/meta/v1:<resource id>"
//   CK by the account key   IV | wrapped | tag, "pm/ck/resource/v1:<id>"
//   CK by a link key        the same, "pm/ck/link/v1"
//   link key by account key the same, "pm/lk/v1:<link id>"
//   a link opened by a code salt (16) | CK by the key PBKDF2 derives from the
//                           code and salt, "pm/ck/code/v1" - 76 bytes, where
//                           CK by a link key is 60
//   the code, for its owner IV | the code encrypted by the account key,
//                           "pm/code/v1:<link id>"
//   what a link delivers    "PMs1" | id | CK by link key | metadata, each
//                           after a 2-byte length, then the content

const IV_BYTES = 12;
const KEY_BYTES = 32;
const WRAPPED_BYTES = IV_BYTES + KEY_BYTES + 16;
const AES = { name: "AES-GCM", length: 256 };
const CONTENT = new TextEncoder().encode("PMr1");
const META = new TextEncoder().encode("PMm1");
const BUNDLE = new TextEncoder().encode("PMs1");
const SALT_BYTES = 16;
const CODE_WRAPPED_BYTES = SALT_BYTES + WRAPPED_BYTES;
export const CODE_ITERATIONS = 600000;

const subtle = () => globalThis.crypto.subtle;
const random = (length) => globalThis.crypto.getRandomValues(new Uint8Array(length));
const utf8 = (text) => new TextEncoder().encode(text);

function concat(...parts) {
  const out = new Uint8Array(parts.reduce((total, part) => total + part.length, 0));
  let offset = 0;
  for (const part of parts) {
    out.set(part, offset);
    offset += part.length;
  }
  return out;
}

function startsWith(bytes, magic) {
  return bytes.length >= magic.length && magic.every((byte, i) => bytes[i] === byte);
}

// newContentKey makes a resource's key. It is extractable so that it can be
// wrapped - for the account, and for each link - and is never stored as is.
export function newContentKey() {
  return subtle().generateKey(AES, true, ["encrypt", "decrypt"]);
}

async function wrap(key, wrappingKey, additionalData) {
  const iv = random(IV_BYTES);
  return concat(iv, new Uint8Array(await subtle().wrapKey("raw", key, wrappingKey, { name: "AES-GCM", iv, additionalData })));
}

async function unwrap(wrapped, wrappingKey, additionalData, usages, extractable) {
  if (wrapped.length !== WRAPPED_BYTES) throw new Error("malformed wrapped key");
  return subtle().unwrapKey("raw", wrapped.slice(IV_BYTES), wrappingKey,
    { name: "AES-GCM", iv: wrapped.slice(0, IV_BYTES), additionalData }, AES, extractable, usages);
}

export const wrapContentKey = (contentKey, accountKey, resourceID) =>
  wrap(contentKey, accountKey, utf8(`pm/ck/resource/v1:${resourceID}`));

// openContentKey unwraps a resource's key with the account key. Extractable
// only when a link is about to be made for it.
export const openContentKey = (wrapped, accountKey, resourceID, extractable = false) =>
  unwrap(wrapped, accountKey, utf8(`pm/ck/resource/v1:${resourceID}`), ["encrypt", "decrypt"], extractable);

async function seal(magic, purpose, key, plain) {
  const iv = random(IV_BYTES);
  const sealed = new Uint8Array(await subtle().encrypt({ name: "AES-GCM", iv, additionalData: concat(magic, purpose) }, key, plain));
  return concat(magic, iv, sealed);
}

async function open(magic, purpose, key, blob) {
  if (!startsWith(blob, magic) || blob.length <= magic.length + IV_BYTES + 16) throw new Error("not sealed content");
  const iv = blob.slice(magic.length, magic.length + IV_BYTES);
  return new Uint8Array(await subtle().decrypt({ name: "AES-GCM", iv, additionalData: concat(magic, purpose) }, key, blob.slice(magic.length + IV_BYTES)));
}

export const encryptContent = (key, resourceID, bytes) => seal(CONTENT, utf8(`pm/content/v1:${resourceID}`), key, bytes);
export const decryptContent = (key, resourceID, blob) => open(CONTENT, utf8(`pm/content/v1:${resourceID}`), key, blob);

export async function encryptMeta(key, resourceID, meta) {
  const plain = utf8(JSON.stringify({ n: meta.name || "", f: meta.filename || "", t: meta.type || "" }));
  return seal(META, utf8(`pm/meta/v1:${resourceID}`), key, plain);
}

export async function decryptMeta(key, resourceID, blob) {
  const plain = await open(META, utf8(`pm/meta/v1:${resourceID}`), key, blob);
  const meta = JSON.parse(new TextDecoder().decode(plain));
  return { name: String(meta.n || ""), filename: String(meta.f || ""), type: String(meta.t || "") };
}

// A link's key: random bytes for the address after #, the content key
// wrapped by them for the service to hand out, and the same bytes wrapped by
// the account key so their owner can show the address again.
export async function newLinkKeys(contentKey, accountKey, linkID) {
  const bytes = random(KEY_BYTES);
  const linkKey = await subtle().importKey("raw", bytes, "AES-GCM", true, ["wrapKey", "unwrapKey"]);
  return {
    bytes,
    sealedKey: await wrap(contentKey, linkKey, utf8("pm/ck/link/v1")),
    ownerKey: await wrap(linkKey, accountKey, utf8(`pm/lk/v1:${linkID}`)),
  };
}

// openLinkKey gives back a link's key bytes, for its owner to see the full
// address again.
export async function openLinkKey(ownerKey, accountKey, linkID) {
  const key = await unwrap(ownerKey, accountKey, utf8(`pm/lk/v1:${linkID}`), ["wrapKey", "unwrapKey"], true);
  return new Uint8Array(await subtle().exportKey("raw", key));
}

// A code a recipient types: four characters from letters and digits that
// cannot be mistaken for one another - no 0 or O, no 1, I or L - read in
// either case. Drawn without bias from the browser's own randomness.
const CODE_ALPHABET = "ABCDEFGHJKMNPQRSTUVWXYZ23456789";

export function newCode() {
  let code = "";
  while (code.length < 4) {
    const [value] = random(1);
    if (value < 248) code += CODE_ALPHABET[value % CODE_ALPHABET.length];
  }
  return code;
}

export const normalCode = (code) => String(code).toUpperCase();

async function codeKey(code, salt) {
  const material = await subtle().importKey("raw", utf8(normalCode(code)), "PBKDF2", false, ["deriveKey"]);
  return subtle().deriveKey({ name: "PBKDF2", hash: "SHA-256", salt, iterations: CODE_ITERATIONS }, material, AES, false, ["wrapKey", "unwrapKey"]);
}

// The account key only wraps keys, so the code is carried as one: its
// bytes as raw key material, which AES-GCM wrapping encrypts as they are.
const codeMaterial = (code) => subtle().importKey("raw", utf8(normalCode(code)), { name: "HMAC", hash: "SHA-256" }, true, ["sign"]);

// A link opened by a code: the content key wrapped by the key the code
// derives, behind the salt it was derived with, and the code itself wrapped
// by the account key so its owner can read it again.
export async function newCodeLinkKeys(contentKey, accountKey, linkID, code) {
  const salt = random(SALT_BYTES);
  return {
    sealedKey: concat(salt, await wrap(contentKey, await codeKey(code, salt), utf8("pm/ck/code/v1"))),
    ownerKey: await wrap(await codeMaterial(code), accountKey, utf8(`pm/code/v1:${linkID}`)),
  };
}

export async function openLinkCode(ownerKey, accountKey, linkID) {
  if (ownerKey.length <= IV_BYTES + 16) throw new Error("malformed code");
  const key = await subtle().unwrapKey("raw", ownerKey.slice(IV_BYTES), accountKey,
    { name: "AES-GCM", iv: ownerKey.slice(0, IV_BYTES), additionalData: utf8(`pm/code/v1:${linkID}`) },
    { name: "HMAC", hash: "SHA-256" }, true, ["sign"]);
  return new TextDecoder().decode(await subtle().exportKey("raw", key));
}

export function isBundle(bytes) {
  return startsWith(bytes, BUNDLE);
}

function bundleParts(bytes) {
  let offset = BUNDLE.length;
  const take = () => {
    if (offset + 2 > bytes.length) throw new Error("truncated bundle");
    const length = (bytes[offset] << 8) | bytes[offset + 1];
    const part = bytes.slice(offset + 2, offset + 2 + length);
    if (part.length !== length) throw new Error("truncated bundle");
    offset += 2 + length;
    return part;
  };
  const resourceID = new TextDecoder().decode(take());
  const wrapped = take();
  const metaBlob = take();
  return { resourceID, wrapped, metaBlob, content: bytes.slice(offset) };
}

// bundleWantsCode tells a link opened by a code from one whose key is in
// its address.
export const bundleWantsCode = (bytes) => bundleParts(bytes).wrapped.length === CODE_WRAPPED_BYTES;

// openBundle decrypts what a link delivered, with the key from its address
// or the code its recipient was told. A wrong code fails as a wrong key does.
export async function openBundle(bytes, { key: linkKeyBytes = null, code = "" } = {}) {
  const { resourceID, wrapped, metaBlob, content } = bundleParts(bytes);
  let key;
  if (wrapped.length === CODE_WRAPPED_BYTES) {
    if (!code) throw new Error("code needed");
    const derived = await codeKey(code, wrapped.slice(0, SALT_BYTES));
    key = await unwrap(wrapped.slice(SALT_BYTES), derived, utf8("pm/ck/code/v1"), ["decrypt"], false);
  } else {
    if (!linkKeyBytes || linkKeyBytes.length !== KEY_BYTES) throw new Error("malformed key");
    const linkKey = await subtle().importKey("raw", linkKeyBytes, "AES-GCM", false, ["unwrapKey"]);
    key = await unwrap(wrapped, linkKey, utf8("pm/ck/link/v1"), ["decrypt"], false);
  }
  const meta = await decryptMeta(key, resourceID, metaBlob);
  return { ...meta, content: await decryptContent(key, resourceID, content) };
}

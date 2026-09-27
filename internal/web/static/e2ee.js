// End-to-end encryption for quick shares. Everything here runs in the browser
// with the platform's own WebCrypto - no library, no code of ours doing the
// cryptography itself. The server only ever sees what seal() returns.
//
// Envelope (internal/store/e2ee.go checks its shape):
//
//   "PMe1" | flags | salt (16, passphrase only) | IV (12) | AES-256-GCM ciphertext + tag
//
// Flag bit 0: the key is derived from a passphrase (PBKDF2-SHA-256) instead of
// travelling in the link. The header is bound to the ciphertext as additional
// data, so it cannot be changed without decryption failing.
//
// Inside the ciphertext: a 2-byte big-endian name length, the name in UTF-8,
// then the content exactly as given.

const MAGIC = new TextEncoder().encode("PMe1");
const FLAG_PASSPHRASE = 1;
const SALT_BYTES = 16;
const IV_BYTES = 12;
const KEY_BYTES = 32;
export const PBKDF2_ITERATIONS = 600000;

const subtle = () => globalThis.crypto.subtle;
const random = (length) => globalThis.crypto.getRandomValues(new Uint8Array(length));

export function toBase64URL(bytes) {
  let binary = "";
  for (const byte of bytes) binary += String.fromCharCode(byte);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

export function fromBase64URL(text) {
  if (!/^[A-Za-z0-9_-]*$/.test(text)) throw new Error("malformed key");
  const padded = text.replace(/-/g, "+").replace(/_/g, "/") + "===".slice((text.length + 3) % 4);
  return Uint8Array.from(atob(padded), (char) => char.charCodeAt(0));
}

async function rawKey(bytes, usage) {
  if (bytes.length !== KEY_BYTES) throw new Error("malformed key");
  return subtle().importKey("raw", bytes, "AES-GCM", false, [usage]);
}

async function passphraseKey(passphrase, salt, usage) {
  const material = await subtle().importKey("raw", new TextEncoder().encode(passphrase), "PBKDF2", false, ["deriveKey"]);
  return subtle().deriveKey(
    { name: "PBKDF2", hash: "SHA-256", salt, iterations: PBKDF2_ITERATIONS },
    material, { name: "AES-GCM", length: 256 }, false, [usage],
  );
}

function concat(...parts) {
  const out = new Uint8Array(parts.reduce((total, part) => total + part.length, 0));
  let offset = 0;
  for (const part of parts) {
    out.set(part, offset);
    offset += part.length;
  }
  return out;
}

// seal encrypts content (bytes) and its name. With a passphrase the key is
// derived from it and nothing goes in the link; without one a random key is
// made and returned, base64url, for the part of the link after #.
export async function seal(content, name, passphrase = "") {
  const nameBytes = new TextEncoder().encode(name || "");
  if (nameBytes.length > 0xffff) throw new Error("name too long");
  const plain = concat(new Uint8Array([nameBytes.length >> 8, nameBytes.length & 0xff]), nameBytes, content);

  let header;
  let key;
  let linkKey = "";
  if (passphrase) {
    const salt = random(SALT_BYTES);
    header = concat(MAGIC, new Uint8Array([FLAG_PASSPHRASE]), salt);
    key = await passphraseKey(passphrase, salt, "encrypt");
  } else {
    const bytes = random(KEY_BYTES);
    header = concat(MAGIC, new Uint8Array([0]));
    key = await rawKey(bytes, "encrypt");
    linkKey = toBase64URL(bytes);
  }
  const iv = random(IV_BYTES);
  const sealed = new Uint8Array(await subtle().encrypt({ name: "AES-GCM", iv, additionalData: header }, key, plain));
  return { envelope: concat(header, iv, sealed), key: linkKey };
}

// inspect reads the envelope's header: whether it wants a passphrase, and the
// pieces decryption needs. It throws on anything that is not an envelope.
export function inspect(envelope) {
  const bytes = envelope instanceof Uint8Array ? envelope : new Uint8Array(envelope);
  if (bytes.length < MAGIC.length + 1 || MAGIC.some((byte, i) => bytes[i] !== byte)) throw new Error("not an envelope");
  const flags = bytes[MAGIC.length];
  if (flags & ~FLAG_PASSPHRASE) throw new Error("unknown envelope");
  const passphrase = (flags & FLAG_PASSPHRASE) !== 0;
  let offset = MAGIC.length + 1;
  const salt = passphrase ? bytes.slice(offset, offset + SALT_BYTES) : null;
  if (passphrase) offset += SALT_BYTES;
  const header = bytes.slice(0, offset);
  const iv = bytes.slice(offset, offset + IV_BYTES);
  const sealed = bytes.slice(offset + IV_BYTES);
  if (iv.length !== IV_BYTES || sealed.length <= 16) throw new Error("truncated envelope");
  return { passphrase, salt, header, iv, sealed };
}

// open decrypts an envelope with the link's key or the passphrase, and gives
// back the name and the content bytes. A wrong key, a wrong passphrase and a
// changed byte anywhere all fail the same way: AES-GCM refuses to decrypt.
export async function open(envelope, { key = "", passphrase = "" } = {}) {
  const parts = inspect(envelope);
  const cryptoKey = parts.passphrase
    ? await passphraseKey(passphrase, parts.salt, "decrypt")
    : await rawKey(fromBase64URL(key), "decrypt");
  const plain = new Uint8Array(await subtle().decrypt(
    { name: "AES-GCM", iv: parts.iv, additionalData: parts.header }, cryptoKey, parts.sealed,
  ));
  if (plain.length < 2) throw new Error("truncated content");
  const nameLength = (plain[0] << 8) | plain[1];
  if (plain.length < 2 + nameLength) throw new Error("truncated content");
  const name = new TextDecoder("utf-8", { fatal: false }).decode(plain.subarray(2, 2 + nameLength));
  return { name, content: plain.slice(2 + nameLength) };
}

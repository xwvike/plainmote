// The master password's keys, in the browser's own WebCrypto - no library,
// no code of ours doing the cryptography itself. docs/encryption.md is the
// full design; in short:
//
//   master password --PBKDF2-SHA-256--> master key (never stored)
//   master key  --wraps--> account key (random; the service keeps it wrapped)
//   recovery key --wraps--> the same account key
//
// The account key wraps each encrypted resource's content key. Once
// unwrapped for use it is a non-extractable CryptoKey: scripts on the page
// can wrap and unwrap with it, and nothing can read its bytes.
//
// A wrapped key is IV (12) | AES-256-GCM of the 32 key bytes | tag (16). The
// additional data names what the key is for and whose it is, so a wrapped
// key moved to another account or another purpose fails to unwrap.

import { toBase64URL, fromBase64URL } from "./e2ee.js";

export const KDF = "pbkdf2-sha256";
export const ITERATIONS = 600000;
export const MIN_PASSWORD = 8;
const SALT_BYTES = 16;
const IV_BYTES = 12;
const KEY_BYTES = 32;
const WRAPPED_BYTES = IV_BYTES + KEY_BYTES + 16;
const AES = { name: "AES-GCM", length: 256 };
const WRAPS = ["wrapKey", "unwrapKey"];

const subtle = () => globalThis.crypto.subtle;
const random = (length) => globalThis.crypto.getRandomValues(new Uint8Array(length));
const utf8 = (text) => new TextEncoder().encode(text);

export const purpose = {
  password: (userID) => utf8(`pm/ak/password/v1:${userID}`),
  recovery: (userID) => utf8(`pm/ak/recovery/v1:${userID}`),
};

function concat(a, b) {
  const out = new Uint8Array(a.length + b.length);
  out.set(a, 0);
  out.set(b, a.length);
  return out;
}

// The password is normalised so that the same characters typed on another
// system, which may compose them differently, derive the same key.
export async function passwordKey(password, salt, iterations = ITERATIONS) {
  const material = await subtle().importKey("raw", utf8(password.normalize("NFC")), "PBKDF2", false, ["deriveKey"]);
  return subtle().deriveKey({ name: "PBKDF2", hash: "SHA-256", salt, iterations }, material, AES, false, WRAPS);
}

async function recoveryWrappingKey(bytes) {
  if (bytes.length !== KEY_BYTES) throw new Error("malformed recovery key");
  return subtle().importKey("raw", bytes, "AES-GCM", false, WRAPS);
}

async function wrap(key, wrappingKey, additionalData) {
  const iv = random(IV_BYTES);
  const sealed = new Uint8Array(await subtle().wrapKey("raw", key, wrappingKey, { name: "AES-GCM", iv, additionalData }));
  return concat(iv, sealed);
}

// unwrap fails - AES-GCM refuses - on a wrong password, a wrong recovery key,
// another account's or another purpose's key, or a changed byte.
async function unwrap(wrapped, wrappingKey, additionalData, extractable) {
  if (wrapped.length !== WRAPPED_BYTES) throw new Error("malformed wrapped key");
  return subtle().unwrapKey(
    "raw", wrapped.slice(IV_BYTES), wrappingKey,
    { name: "AES-GCM", iv: wrapped.slice(0, IV_BYTES), additionalData },
    AES, extractable, WRAPS,
  );
}

// Recovery keys are 256 random bits written in Crockford's base32: 52
// characters in 13 groups of four, with no I, L, O or U to misread.
const BASE32 = "0123456789ABCDEFGHJKMNPQRSTVWXYZ";

export function encodeRecovery(bytes) {
  if (bytes.length !== KEY_BYTES) throw new Error("malformed recovery key");
  let value = 0n;
  for (const byte of bytes) value = (value << 8n) | BigInt(byte);
  let out = "";
  for (let digit = 51; digit >= 0; digit--) out += BASE32[Number((value >> BigInt(digit * 5)) & 31n)];
  return out.match(/.{4}/g).join("-");
}

export function decodeRecovery(text) {
  const cleaned = text.toUpperCase().replace(/[\s-]/g, "").replace(/O/g, "0").replace(/[IL]/g, "1");
  if (cleaned.length !== 52) throw new Error("malformed recovery key");
  let value = 0n;
  for (const char of cleaned) {
    const digit = BASE32.indexOf(char);
    if (digit < 0) throw new Error("malformed recovery key");
    value = (value << 5n) | BigInt(digit);
  }
  if (value >> 256n) throw new Error("malformed recovery key");
  const bytes = new Uint8Array(KEY_BYTES);
  for (let i = KEY_BYTES - 1; i >= 0; i--) {
    bytes[i] = Number(value & 0xffn);
    value >>= 8n;
  }
  return bytes;
}

// createKeyring makes everything a new master password needs: the record
// the service keeps, the recovery key to show once, and the account key
// ready for use.
export async function createKeyring(userID, password) {
  const salt = random(SALT_BYTES);
  const accountKey = await subtle().generateKey(AES, true, WRAPS);
  const recovery = random(KEY_BYTES);
  const master = await passwordKey(password, salt);
  const byPassword = await wrap(accountKey, master, purpose.password(userID));
  const byRecovery = await wrap(accountKey, await recoveryWrappingKey(recovery), purpose.recovery(userID));
  return {
    record: {
      kdf: KDF, iterations: String(ITERATIONS), salt: toBase64URL(salt),
      wrapped_by_password: toBase64URL(byPassword), wrapped_by_recovery: toBase64URL(byRecovery),
    },
    recovery: encodeRecovery(recovery),
    key: await unwrap(byPassword, master, purpose.password(userID), false),
  };
}

// openWithPassword unwraps the account key with the master password. The
// extractable form is only for rewrapping it, and is never kept.
export async function openWithPassword(userID, ring, password, extractable = false) {
  if (ring.kdf !== KDF) throw new Error("unsupported key derivation");
  const master = await passwordKey(password, fromBase64URL(ring.salt), ring.iterations);
  return unwrap(fromBase64URL(ring.wrapped_by_password), master, purpose.password(userID), extractable);
}

export async function openWithRecovery(userID, ring, recoveryText, extractable = false) {
  const wrappingKey = await recoveryWrappingKey(decodeRecovery(recoveryText));
  return unwrap(fromBase64URL(ring.wrapped_by_recovery), wrappingKey, purpose.recovery(userID), extractable);
}

// rewrapPassword wraps an (extractable) account key under a new password,
// with a new salt.
export async function rewrapPassword(userID, accountKey, password) {
  const salt = random(SALT_BYTES);
  const master = await passwordKey(password, salt);
  const wrapped = await wrap(accountKey, master, purpose.password(userID));
  return {
    fields: { kdf: KDF, iterations: String(ITERATIONS), salt: toBase64URL(salt), wrapped_by_password: toBase64URL(wrapped) },
    key: await unwrap(wrapped, master, purpose.password(userID), false),
  };
}

// rewrapRecovery wraps an (extractable) account key under a new recovery key.
export async function rewrapRecovery(userID, accountKey) {
  const recovery = random(KEY_BYTES);
  const wrapped = await wrap(accountKey, await recoveryWrappingKey(recovery), purpose.recovery(userID));
  return { fields: { wrapped_by_recovery: toBase64URL(wrapped) }, recovery: encodeRecovery(recovery) };
}

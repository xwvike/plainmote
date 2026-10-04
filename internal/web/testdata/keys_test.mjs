// Run by TestKeysModule. Exits non-zero on the first failure.
const k = await import(process.env.KEYS_MODULE);
const assert = (ok, what) => { if (!ok) { console.error("FAIL: " + what); process.exit(1); } };
const refuses = async (promise, what) => {
  try { await promise; } catch { return; }
  assert(false, what);
};
const alice = "11111111-2222-3333-4444-555555555555";
const bob = "99999999-2222-3333-4444-555555555555";

const made = await k.createKeyring(alice, "correct horse");
const ring = { ...made.record, iterations: Number(made.record.iterations) };
assert(ring.kdf === "pbkdf2-sha256" && ring.iterations === 600000, "parameters");
assert(/^([0-9A-HJKMNP-TV-Z]{4}-){12}[0-9A-HJKMNP-TV-Z]{4}$/.test(made.recovery), "recovery key format");
assert(k.encodeRecovery(k.decodeRecovery(made.recovery.toLowerCase().replace(/0/g, "o"))) === made.recovery, "recovery key round trip");
await refuses(crypto.subtle.exportKey("raw", made.key), "the unlocked key must not be exportable");

await k.openWithPassword(alice, ring, "correct horse");
// NFC and NFD spell the same password.
const accented = await k.createKeyring(alice, "café au lait");
await k.openWithPassword(alice, { ...accented.record, iterations: 600000 }, "café au lait");
await refuses(k.openWithPassword(alice, ring, "wrong horse"), "a wrong password");
await refuses(k.openWithPassword(bob, ring, "correct horse"), "another account's id");
await refuses(k.openWithRecovery(alice, { ...ring, wrapped_by_recovery: ring.wrapped_by_password }, made.recovery), "a key moved to another purpose");
await k.openWithRecovery(alice, ring, made.recovery);
const other = await k.createKeyring(alice, "x".repeat(8));
await refuses(k.openWithRecovery(alice, ring, other.recovery), "another recovery key");

// A new password and a new recovery key wrap the same account key.
const extractable = await k.openWithRecovery(alice, ring, made.recovery, true);
const next = await k.rewrapPassword(alice, extractable, "battery staple");
const ring2 = { ...ring, ...next.fields, iterations: Number(next.fields.iterations) };
await k.openWithPassword(alice, ring2, "battery staple");
await refuses(k.openWithPassword(alice, ring2, "correct horse"), "the old password after a change");
const recovered = await k.rewrapRecovery(alice, extractable);
const ring3 = { ...ring2, ...recovered.fields };
await k.openWithRecovery(alice, ring3, recovered.recovery);
await refuses(k.openWithRecovery(alice, ring3, made.recovery), "the old recovery key after a new one");

// Both unwrap to the same key: what one wraps, the other opens.
const viaPassword = await k.openWithPassword(alice, ring3, "battery staple");
const viaRecovery = await k.openWithRecovery(alice, ring3, recovered.recovery);
const content = await crypto.subtle.generateKey({ name: "AES-GCM", length: 256 }, true, ["encrypt"]);
const iv = crypto.getRandomValues(new Uint8Array(12));
const wrapped = await crypto.subtle.wrapKey("raw", content, viaPassword, { name: "AES-GCM", iv });
await crypto.subtle.unwrapKey("raw", wrapped, viaRecovery, { name: "AES-GCM", iv }, "AES-GCM", false, ["encrypt"]);
console.log("ok");
